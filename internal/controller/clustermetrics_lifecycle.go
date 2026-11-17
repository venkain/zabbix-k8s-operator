/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ensureClusterMetrics converges the cluster-metrics ingestion and
// reports readiness, plus the drift-check cadence while the feature is
// on (0 while off — a no-op then). Steps: cluster-scoped RBAC (both
// identities), the monitor token (the service-account token controller
// fills the Secret asynchronously), then the Zabbix-side host
// registration. Failures set the ClusterMetricsReady condition and
// return false so the next reconcile retries at a human pace.
func (r *ZabbixReconciler) ensureClusterMetrics(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) (bool, time.Duration) {
	if z.Spec.ClusterMetrics == nil {
		return true, 0
	}
	log := logf.FromContext(ctx)
	cm := z.Spec.ClusterMetrics

	// Record the placement (namespace + name prefix of the derived
	// objects) on the status BEFORE anything else: the disable and
	// deletion paths address the objects through this record once the
	// spec field is gone, and it is what pins the placement while the
	// feature stays enabled (a mid-flight override change is ignored).
	// Writing it unconditionally is idempotent — the resolver honors
	// the record over the spec from now on.
	status.ClusterMetricsNamespace = zabbix.ClusterMetricsNamespace(z)
	status.ClusterMetricsNamePrefix = zabbix.ClusterMetricsNamePrefix(z)

	// Cluster-scoped objects have no namespaced owner reference, so
	// they are reconciled here instead of through ConvergeAll and are
	// removed in the finalizer.
	role := zabbix.ClusterMetricsClusterRole(z)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error { return nil }); err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"ClusterRBACFailed", fmt.Sprintf("reconciling ClusterRole %s: %v", role.Name, err), z.Generation)
		return false, clusterMetricsRetryDelay
	}
	binding := zabbix.ClusterMetricsClusterRoleBinding(z)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error { return nil }); err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"ClusterRBACFailed", fmt.Sprintf("reconciling ClusterRoleBinding %s: %v", binding.Name, err), z.Generation)
		return false, clusterMetricsRetryDelay
	}

	// The monitor token: created as an empty service-account-token
	// Secret, filled asynchronously by the token controller.
	monitorTokenValue, err := monitorToken(ctx, r.Client, z)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"MonitorTokenPending", err.Error(), z.Generation)
		return false, clusterMetricsRetryDelay
	}

	apiTokenValue, err := apiToken(ctx, r.Client, z)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"APITokenPending", err.Error(), z.Generation)
		return false, clusterMetricsRetryDelay
	}

	hostID, created, err := zabbix.EnsureClusterHost(ctx, r.HTTP, zabbix.APIEndpoint(z), apiTokenValue,
		cm.ClusterName, monitorTokenValue, cm.TLSVerify)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"HostRegistrationFailed", fmt.Sprintf("registering cluster host %q: %v", cm.ClusterName, err), z.Generation)
		return false, clusterMetricsRetryDelay
	}
	if created {
		log.Info("Created cluster host", "host", cm.ClusterName, "hostid", hostID)
	}

	setCond(&status.Conditions, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionTrue,
		"ClusterHostRegistered",
		fmt.Sprintf("cluster host %q registered with template %q (hostid %d)", cm.ClusterName, zabbix.ClusterTemplate, hostID),
		z.Generation)
	status.ClusterMetricsHostName = cm.ClusterName
	return true, clusterMetricsRetryDelay
}

// monitorToken reads the monitor service account's long-lived token
// from the token Secret. An empty Secret means the token controller has
// not filled it yet (it runs asynchronously after the ServiceAccount and
// Secret converge).
func monitorToken(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix) (string, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: zabbix.ClusterMetricsNamespace(z),
		Name:      zabbix.MonitorTokenSecretName(zabbix.ClusterMetricsNamePrefix(z)),
	}, &secret); err != nil {
		return "", fmt.Errorf("reading monitor token secret: %w", err)
	}
	token, ok := secret.Data["token"]
	if !ok || len(token) == 0 {
		return "", fmt.Errorf("monitor token Secret %q is not populated yet (token controller pending)", secret.Name)
	}
	return string(token), nil
}

// finalizeClusterMetrics runs when the resource is being deleted: the
// server-side Zabbix host and the cluster-scoped RBAC have no Kubernetes
// garbage collection to lean on. An unreachable Zabbix API keeps the
// finalizer (requeue with backoff) so the host is never leaked silently.
func (r *ZabbixReconciler) finalizeClusterMetrics(ctx context.Context, z *zabbixv1alpha1.Zabbix) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !zabbix.ClusterMetricsIsEnabled(z) || !controllerutil.ContainsFinalizer(z, ClusterMetricsFinalizer) {
		return ctrl.Result{}, nil
	}

	// 1. Remove the server-side host. A missing API token Secret (e.g.
	// the user deleted it) is logged and skipped: without a token the
	// host cannot be addressed anyway.
	if apiTokenValue, err := apiToken(ctx, r.Client, z); err != nil {
		log.Error(err, "Skipping Zabbix host cleanup: API token unavailable")
	} else if err := zabbix.DeleteClusterHost(ctx, r.HTTP, zabbix.APIEndpoint(z), apiTokenValue,
		z.Spec.ClusterMetrics.ClusterName); err != nil {
		log.Error(err, "Could not delete cluster host")
		return ctrl.Result{}, fmt.Errorf("deleting cluster host %q: %w", z.Spec.ClusterMetrics.ClusterName, err)
	}

	if err := r.removeClusterMetricsObjects(ctx, z); err != nil {
		return ctrl.Result{}, err
	}

	// 3. Release the finalizer.
	controllerutil.RemoveFinalizer(z, ClusterMetricsFinalizer)
	if err := r.Update(ctx, z); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing cluster-metrics cleanup finalizer: %w", err)
	}
	log.Info("Cluster-metrics cleanup finished", "instance", z.Name)
	return ctrl.Result{}, nil
}

// disableClusterMetrics runs when the feature flag is turned OFF on a
// live resource (the finalizer is still present from a previous
// enablement). Kubernetes garbage collection cannot help here — the
// objects' owner (the Zabbix resource) stays alive — so the operator
// removes everything itself: the Zabbix host (best effort; a stale
// unmanaged host is preferable to wedging reconciliation, unlike the
// deletion path where the finalizer blocks), the namespaced endpoint
// objects, and the cluster-scoped RBAC. The finalizer is released:
// with the feature off there is nothing left to guard.
func (r *ZabbixReconciler) disableClusterMetrics(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(z, ClusterMetricsFinalizer) {
		return nil
	}

	// 1. Best-effort server-side host cleanup. The host name lives in
	// the status (the spec field is gone on disable); without a token
	// or a recorded host the step is skipped with a log — a stale
	// unmanaged host is preferable to wedging reconciliation, unlike
	// the deletion path where the finalizer blocks.
	hostName := z.Status.ClusterMetricsHostName
	if hostName == "" {
		log.Info("No registered cluster host recorded; skipping Zabbix host cleanup on disable")
	} else if apiTokenValue, err := apiToken(ctx, r.Client, z); err != nil {
		log.Error(err, "Skipping Zabbix host cleanup on disable: API token unavailable")
	} else if err := zabbix.DeleteClusterHost(ctx, r.HTTP, zabbix.APIEndpoint(z), apiTokenValue,
		hostName); err != nil {
		// The host outlives the feature as an unmanaged Zabbix
		// object; removing it manually is one API call away.
		log.Error(err, "Could not delete cluster host on disable; leaving it as an unmanaged host")
	}

	// 2. Namespaced + cluster-scoped objects.
	if err := r.removeClusterMetricsObjects(ctx, z); err != nil {
		return err
	}

	// 3. Release the finalizer.
	controllerutil.RemoveFinalizer(z, ClusterMetricsFinalizer)
	if err := r.Update(ctx, z); err != nil {
		return fmt.Errorf("removing cluster-metrics cleanup finalizer: %w", err)
	}
	log.Info("Cluster-metrics disabled and cleaned up", "instance", z.Name)
	return nil
}

// removeClusterMetricsObjects deletes the k8s-side cluster-metrics
// objects (namespaced endpoint objects and the cluster-scoped RBAC).
// Already-gone objects are not an error.
func (r *ZabbixReconciler) removeClusterMetricsObjects(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	for _, obj := range []client.Object{
		zabbix.KubeStateMetricsDeployment(z),
		zabbix.KubeStateMetricsService(z),
		zabbix.KubeStateMetricsServiceAccount(z),
		zabbix.MonitorServiceAccount(z),
		zabbix.MonitorTokenSecret(z),
		zabbix.ClusterMetricsClusterRole(z),
		zabbix.ClusterMetricsClusterRoleBinding(z),
		// Hardening: the KSM egress policy is cross-namespace and
		// owner-referenced — it is removed here on the disable and
		// deletion paths. (The MCP policy lives in the resource's own
		// namespace and dies with the owner reference.)
		zabbix.KSMNetworkPolicy(z),
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting cluster-metrics object %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}
