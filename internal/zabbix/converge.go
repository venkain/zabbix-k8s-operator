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

// Package zabbix contains the resource builders and convergence helpers
// used by the Zabbix controllers. All object construction is pure (no I/O)
// so builders are trivially unit-testable; the controller applies them via
// CreateOrUpdate.
package zabbix

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// fieldManager is the server-side-apply field manager identifying every
// field the operator owns. Objects converged by this operator are fully
// owned: fields it stops setting are removed again on the next apply.
const fieldManager = "zabbix-k8s-operator"

// Converge creates or updates obj so it matches the built (desired) state,
// with the owning resource (a Zabbix or a ZabbixProxyGroup instance) as
// controller. Every owned object flows through here.
//
// Convergence uses server-side apply: the API server merges and compares
// only the fields present in the built object, so API defaulting (e.g.
// revisionHistoryLimit, progressDeadlineSeconds, protocol) never shows up
// as a diff. A repeated apply of an unchanged configuration is a server
// -side no-op — no update, no watch event, no reconcile loop. This is why
// Converge does not use controllerutil.CreateOrUpdate with wholesale spec
// reapplication: the client-side deep-equal there sees defaulted live
// fields as drift and updates every object on every reconcile (a storm).
func Converge(ctx context.Context, c client.Client, owner client.Object, desired client.Object) error {
	logger := log.FromContext(ctx).WithValues("object", fmt.Sprintf("%T", desired), "name", desired.GetName())

	// Ownership must be part of the applied configuration so the created
	// object is owned (and garbage-collected) from birth. Kubernetes
	// ownership is same-namespace only: a dependent placed in another
	// namespace (cluster-metrics placement override) gets no owner
	// reference — the API server rejects cross-namespace ones — and
	// relies on operator-driven cleanup instead (the disable path and
	// the deletion finalizer).
	if desired.GetNamespace() != "" && owner.GetNamespace() != "" &&
		desired.GetNamespace() != owner.GetNamespace() {
		logger.V(1).Info("object placed outside the owner's namespace; applying without owner reference",
			"namespace", desired.GetNamespace())
	} else if err := controllerutil.SetControllerReference(owner, desired, c.Scheme()); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	// Apply configurations carry their GVK explicitly.
	var gvk schema.GroupVersionKind
	switch desired.(type) {
	case *appsv1.Deployment:
		gvk = appsv1.SchemeGroupVersion.WithKind("Deployment")
	case *appsv1.StatefulSet:
		gvk = appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	case *corev1.Service:
		gvk = corev1.SchemeGroupVersion.WithKind("Service")
	case *policyv1.PodDisruptionBudget:
		gvk = policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget")
	case *corev1.ServiceAccount:
		gvk = corev1.SchemeGroupVersion.WithKind("ServiceAccount")
	case *corev1.Secret:
		gvk = corev1.SchemeGroupVersion.WithKind("Secret")
	default:
		return fmt.Errorf("unsupported owned object type %T", desired)
	}
	desired.GetObjectKind().SetGroupVersionKind(gvk)

	// The new Client.Apply entry point requires an ApplyConfiguration;
	// unstructured conversion preserves omitted fields as absent, which
	// is exactly the apply semantics Converge relies on.
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return fmt.Errorf("converting %T to apply configuration: %w", desired, err)
	}
	applyCfg := &unstructured.Unstructured{Object: raw}
	applyCfg.SetGroupVersionKind(gvk)

	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(applyCfg), &client.ApplyOptions{
		FieldManager: fieldManager,
		Force:        new(true),
	}); err != nil {
		return err
	}
	logger.V(1).Info("applied object")
	return nil
}

// ConvergeAll creates/updates every object the Zabbix resource owns.
// server is the pre-shaped server StatefulSet — the steady-state builder
// (ServerStatefulSet) or a transition shape (ServerWorkloadForRollout).
// Passing it in keeps the rollout phase decision in one place and avoids
// applying two different server shapes in a single reconcile.
func ConvergeAll(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix, db *DatabaseEnv, server *appsv1.StatefulSet) error {
	objects := []client.Object{
		server,
		ServerService(z),
		ActiveServerService(z),
		FrontendDeployment(z, db),
		FrontendService(z),
		ServerDisruptionBudget(z),
		FrontendDisruptionBudget(z),
	}

	if z.Spec.WebService != nil {
		objects = append(objects,
			WebServiceDeployment(z),
			WebServiceService(z),
			WebServiceDisruptionBudget(z),
		)
	}

	// Cluster-metrics ingestion (namespaced objects, in the placement
	// namespace — the resource's own namespace or the override; the
	// cluster-scoped RBAC has no namespaced owner and is reconciled by
	// the controller directly).
	if z.Spec.ClusterMetrics != nil {
		objects = append(objects,
			KubeStateMetricsDeployment(z),
			KubeStateMetricsService(z),
			KubeStateMetricsServiceAccount(z),
			MonitorServiceAccount(z),
			MonitorTokenSecret(z),
		)
	}

	for _, obj := range objects {
		if err := Converge(ctx, c, z, obj); err != nil {
			return fmt.Errorf("converging %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// ServerReplicas returns the configured server replica count (1 = standalone).
func ServerReplicas(z *zabbixv1alpha1.Zabbix) int32 {
	if z.Spec.Server.Replicas != nil {
		return *z.Spec.Server.Replicas
	}
	return 1
}

// ComponentIsReady reports whether the given deployment has at least the
// desired number of ready replicas.
func ComponentIsReady(ctx context.Context, c client.Client, namespace, name string, desired int32) (bool, error) {
	var dep appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas >= desired, nil
}

// StatefulSetIsReady reports whether the given StatefulSet has at least
// the desired number of ready replicas.
func StatefulSetIsReady(ctx context.Context, c client.Client, namespace, name string, desired int32) (bool, error) {
	var sts appsv1.StatefulSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sts.Status.ReadyReplicas >= desired, nil
}

// SetCondition applies a condition onto any conditions slice (the Zabbix
// status and the ZabbixProxyGroup status use the same metav1.Condition
// listType=map structure).
func SetCondition(conditions *[]metav1.Condition, condType string, condStatus metav1.ConditionStatus, reason, message string, generation int64) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}
