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
	"net/http"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// databaseRetryDelay is the requeue delay when the database connection
// details cannot be resolved yet (Secret missing or incomplete). The
// database may still be provisioning (e.g. a CloudNativePG Cluster
// coming up), so retry at a human pace rather than error-spinning.
const databaseRetryDelay = 30 * time.Second

// haRetryDelay is the reconcile cadence for HA role maintenance: the
// failover reached by a standby node takes at least 5 s (clean stop) and
// typically the configured failover delay (default 1 min); a 15 s
// cadence converges the -active routing shortly after the server's own
// takeover decision.
const haRetryDelay = 15 * time.Second

// clusterMetricsRetryDelay is the requeue delay while cluster-metrics
// ingestion is converging: the service-account token controller fills
// the monitor token Secret asynchronously, and the Zabbix API must be
// reachable for the host registration.
const clusterMetricsRetryDelay = 30 * time.Second

// ClusterMetricsFinalizer guards the server-side Zabbix host and the
// cluster-scoped RBAC the feature creates: deleting the resource must
// remove the host (otherwise the server keeps scraping a dead endpoint)
// and must not leave orphaned ClusterRoles.
const ClusterMetricsFinalizer = "zabbix.venkain.io/cluster-metrics-cleanup"

// ZabbixReconciler reconciles a Zabbix object. One resource owns one
// complete Zabbix deployment: server, frontend, web service, and their
// database wiring. All object construction lives in internal/zabbix as
// pure builders; this controller resolves inputs, converges the owned
// objects, and reports observed state on the status subresource.
type ZabbixReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTP is the client used for Zabbix API calls (nil = default).
	HTTP *http.Client
}

// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// Cluster-metrics ingestion: owned ServiceAccount/token Secret and the
// cluster-scoped RBAC both identities need. The manager must hold a
// superset of the rules it grants (Kubernetes privilege-escalation
// protection), so these read rules exist in the manager role too.
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=nodes;namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=endpoints;services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services/proxy;nodes/proxy,verbs=create;get
// +kubebuilder:rbac:groups=apps,resources=daemonsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the cluster toward the state described by the Zabbix
// resource: resolve the database wiring, converge every owned object,
// then publish per-component readiness on the status subresource.
func (r *ZabbixReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var z zabbixv1alpha1.Zabbix
	if err := r.Get(ctx, req.NamespacedName, &z); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted: owned objects are garbage-collected through owner
			// references; nothing else to clean up.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting Zabbix %s: %w", req.NamespacedName, err)
	}

	if !z.DeletionTimestamp.IsZero() {
		return r.finalizeClusterMetrics(ctx, &z)
	}

	// The status is built on a copy of the CURRENT status: SetStatusCondition
	// preserves lastTransitionTime of unchanged conditions, so a write that
	// changes nothing can be detected and skipped below. Without the skip,
	// every Status().Update bumps resourceVersion, the watch requeues the
	// resource, and the controller hot-loops on itself.
	status := z.Status.DeepCopy()
	status.ObservedGeneration = z.Generation

	// 1. Resolve the database wiring. Without it nothing can run.
	db, err := zabbix.ResolveDatabase(ctx, r.Client, z.Namespace, z.Spec.Database)
	if err != nil {
		log.Error(err, "database resolution failed; requeueing")
		setCond(status, zabbixv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse, "DatabaseUnresolved", err.Error(), z.Generation)
		return persistStatus(ctx, r.Client, &z, *status, databaseRetryDelay)
	}

	// 1b. The cluster-metrics cleanup finalizer comes before the first
	// server-side registration below: deletion must always be able to
	// clean up the Zabbix host and the cluster-scoped RBAC.
	if zabbix.ClusterMetricsIsEnabled(&z) && !controllerutil.ContainsFinalizer(&z, ClusterMetricsFinalizer) {
		controllerutil.AddFinalizer(&z, ClusterMetricsFinalizer)
		if err := r.Update(ctx, &z); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding cluster-metrics cleanup finalizer: %w", err)
		}
	}
	// 1c. Feature flag turned OFF while the finalizer (and all the
	// cluster-metrics side effects) are still around: garbage
	// collection cannot help — the resource itself stays alive — so
	// the operator removes the host, the endpoint objects and the
	// cluster-scoped RBAC itself, then releases the finalizer.
	if !zabbix.ClusterMetricsIsEnabled(&z) && controllerutil.ContainsFinalizer(&z, ClusterMetricsFinalizer) {
		if err := r.disableClusterMetrics(ctx, &z); err != nil {
			return ctrl.Result{}, fmt.Errorf("disabling cluster metrics: %w", err)
		}
		dropCondition(status, zabbixv1alpha1.ConditionClusterMetricsReady)
		status.ClusterMetricsHostName = ""
		status.ClusterMetricsNamespace = ""
		status.ClusterMetricsNamePrefix = ""
	}
	setCond(status, zabbixv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue, "DatabaseResolved",
		fmt.Sprintf("connection details resolved from secret %s", db.SecretName()), z.Generation)

	// 2. One-time migration: HA node identity needs stable pod names, so
	// the server runs as a StatefulSet (stable name across restarts means
	// its registry entry is reused instead of leaked). Instances still
	// running the legacy server Deployment are drained first — a clean
	// stop marks the standalone registry entry inactive, which the first
	// StatefulSet pod needs before it can start in HA mode.
	if requeue, err := r.migrateLegacyServerDeployment(ctx, &z); err != nil {
		return ctrl.Result{}, err
	} else if requeue {
		return persistStatus(ctx, r.Client, &z, *status, haRetryDelay)
	}

	// 3. Shape the server workload. During a standalone↔HA transition
	// the phase machine (ServerWorkloadForRollout) caps replicas and
	// adds/retires the HA identity so the server never has to start in
	// HA mode against an active standalone registry entry — the failure
	// mode the monolithic apply would hit. Outside transitions, the
	// steady-state builder is used unchanged.
	var liveSTS appsv1.StatefulSet
	liveErr := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: zabbix.ServerName(z.Name)}, &liveSTS)
	liveHA := liveErr == nil && zabbix.StatefulSetHasHAIdentity(&liveSTS)

	needHA := zabbix.ServerReplicas(&z) > 1 || liveHA
	var nodes []zabbix.HANode
	if needHA {
		token, err := apiToken(ctx, r.Client, &z)
		if err != nil {
			// No token (frontend not up yet, secret not seeded) is
			// orthogonal to convergence: continue so the stack can
			// still converge, and let HA maintenance surface it.
			log.Error(err, "resolving API token; HA routing deferred")
		} else {
			fetched, err := zabbix.FetchHANodes(ctx, r.HTTP, zabbix.APIEndpoint(&z), token)
			if err != nil {
				// Registry unknown: the phase machine holds instead
				// of scaling into it; maintenance refetches below.
				log.Error(err, "fetching HA node registry failed; holding HA rollout")
			} else {
				nodes = fetched
			}
		}
	}

	serverSTS := zabbix.ServerStatefulSet(&z, db)
	if needHA {
		var phase zabbix.ServerRolloutPhase
		serverSTS, phase = zabbix.ServerWorkloadForRollout(&z, db, liveOrNone(liveErr, &liveSTS), nodes)
		if phase != zabbix.RolloutSteady {
			log.Info("server rollout in transition", "phase", phase)
		}
	}

	// 4. Converge every owned object.
	if err := zabbix.ConvergeAll(ctx, r.Client, &z, db, serverSTS); err != nil {
		return ctrl.Result{}, fmt.Errorf("converging Zabbix %s: %w", req.NamespacedName, err)
	}

	// 5. Publish component readiness.
	componentsReady, err := r.publishComponentReadiness(ctx, &z, status)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5b. Cluster-metrics ingestion: converge the cluster-scoped RBAC,
	// wait for the monitor token, and register the cluster host with
	// the server through the Zabbix API.
	clusterMetricsDelay := time.Duration(0)
	if z.Spec.ClusterMetrics != nil {
		componentsReady = r.ensureClusterMetrics(ctx, &z, status) && componentsReady
		clusterMetricsDelay = clusterMetricsRetryDelay
	}

	readyMsg := "all components ready"
	if !componentsReady {
		readyMsg = "waiting for components to become ready"
	}
	setCond(status, zabbixv1alpha1.ConditionReady, readyCondition(componentsReady), "ComponentsReady", readyMsg, z.Generation)

	// 6. HA role maintenance (HA participants only): per-pod labels
	// follow the ha_node registry so the -active Service routes to the
	// active node. Standalone servers keep the active label from the
	// pod template and need no API access. During a rollout (HA
	// template live or scaled up) maintenance must run even at one
	// replica: it is what reports when the registry allows the next
	// transition step.
	requeueDelay := time.Duration(0)
	if needHA {
		if err := r.maintainHARoles(ctx, &z, status, nodes); err != nil {
			// The -active Service keeps routing to the last known
			// labels; the reconcile error requeues with backoff until
			// the registry becomes reachable again.
			return ctrl.Result{}, fmt.Errorf("maintaining HA roles: %w", err)
		}
		requeueDelay = haRetryDelay
	}
	if clusterMetricsDelay > requeueDelay {
		// Keep the drift-check cadence even when ready: the host
		// registration lives outside Kubernetes and must be realigned.
		requeueDelay = clusterMetricsDelay
	}

	return persistStatus(ctx, r.Client, &z, *status, requeueDelay)
}

// liveOrNone returns the live StatefulSet when it exists, nil otherwise.
// The rollout machine treats a nil workload as "fresh install".
func liveOrNone(getErr error, sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	if getErr != nil {
		return nil
	}
	return sts
}

// persistStatus writes the given status only when it differs from the
// status currently stored on the resource (no-op writes would requeue the
// resource through the watch and hot-loop the controller), and returns the
// reconcile result. Stale-resourceVersion conflicts (the object changed
// between the head-of-reconcile read and this write — e.g. through the
// apply in the same pass) are retried once with a fresh read.
func persistStatus(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix, status zabbixv1alpha1.ZabbixStatus, requeue time.Duration) (ctrl.Result, error) {
	if reflect.DeepEqual(z.Status, status) {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	z.Status = status
	for attempt := range 2 {
		if attempt > 0 {
			// Re-read so the write carries the current resourceVersion.
			fresh := &zabbixv1alpha1.Zabbix{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(z), fresh); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-reading for status update: %w", err)
			}
			fresh.Status = status
			z = fresh
		}
		if err := c.Status().Update(ctx, z); err == nil {
			return ctrl.Result{RequeueAfter: requeue}, nil
		} else if !apierrors.IsConflict(err) {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// migrateLegacyServerDeployment drains and removes the legacy server
// Deployment left by earlier operator versions (the server moved to a
// StatefulSet for stable HA node identity). A clean stop of the drained
// pod marks its standalone registry entry inactive, which the first
// StatefulSet pod requires before it can start. The legacy object is
// deleted only after the StatefulSet serves traffic, keeping a rollback
// path in the meantime.
func (r *ZabbixReconciler) migrateLegacyServerDeployment(ctx context.Context, z *zabbixv1alpha1.Zabbix) (bool, error) {
	log := logf.FromContext(ctx)

	var legacy appsv1.DeploymentList
	if err := r.List(ctx, &legacy, client.InNamespace(z.Namespace), client.MatchingLabels{
		zabbix.InstanceLabel:  z.Name,
		zabbix.ComponentLabel: zabbix.ComponentServer,
	}); err != nil {
		return false, fmt.Errorf("listing legacy server deployments: %w", err)
	}
	if len(legacy.Items) == 0 {
		return false, nil
	}

	stsReady, err := zabbix.StatefulSetIsReady(ctx, r.Client, z.Namespace, zabbix.ServerName(z.Name), zabbix.ServerReplicas(z))
	if err != nil {
		return false, fmt.Errorf("checking server statefulset during migration: %w", err)
	}

	requeue := false
	for i := range legacy.Items {
		dep := &legacy.Items[i]
		replicas := int32(1)
		if dep.Spec.Replicas != nil {
			replicas = *dep.Spec.Replicas
		}
		switch {
		case replicas != 0:
			// Drain: clean pod termination retires the standalone
			// registry entry.
			base := dep.DeepCopy()
			zero := int32(0)
			dep.Spec.Replicas = &zero
			if err := r.Patch(ctx, dep, client.MergeFrom(base)); err != nil {
				return false, fmt.Errorf("draining legacy server deployment %s: %w", dep.Name, err)
			}
			log.Info("draining legacy server deployment", "deployment", dep.Name)
			requeue = true
		case dep.Status.Replicas != 0:
			// Pods still terminating.
			requeue = true
		case stsReady:
			// StatefulSet serves traffic; the husk can go.
			if err := r.Delete(ctx, dep); err != nil {
				return false, fmt.Errorf("deleting legacy server deployment %s: %w", dep.Name, err)
			}
			log.Info("deleted legacy server deployment after statefulset takeover", "deployment", dep.Name)
		default:
			// Drained but StatefulSet not ready yet: keep the husk as
			// a rollback path. Do NOT requeue — the dequeued drain and
			// the missing StatefulSet must not block convergence; the
			// StatefulSet watch (Owns) fires the reconcile that deletes
			// the husk once it serves traffic.
		}
	}
	return requeue, nil
}

// maintainHARoles aligns every server pod's HA-role label with the ha_node
// registry (official hanode.get) and publishes the registry state on the
// status subresource. Node names equal pod names in operator-managed
// deployments (ZBX_AUTOHANODENAME=hostname); the empty-name registry entry
// of a transitioning standalone node matches no pod and is ignored.
// nodes may carry the already-fetched registry from the reconcile pass
// (nil fetches it here — the standalone unit-test path).
func (r *ZabbixReconciler) maintainHARoles(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus, nodes []zabbix.HANode) error {
	log := logf.FromContext(ctx)

	if nodes == nil {
		token, err := apiToken(ctx, r.Client, z)
		if err != nil {
			return err
		}
		nodes, err = zabbix.FetchHANodes(ctx, r.HTTP, zabbix.APIEndpoint(z), token)
		if err != nil {
			return err
		}
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(z.Namespace), client.MatchingLabels{
		zabbix.InstanceLabel:  z.Name,
		zabbix.ComponentLabel: zabbix.ComponentServer,
	}); err != nil {
		return fmt.Errorf("listing server pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		role, set := zabbix.DesiredHARoleLabel(pod.Name, nodes)
		orig := pod.DeepCopy()
		if set {
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			pod.Labels[zabbix.HARoleLabel] = role
		} else {
			delete(pod.Labels, zabbix.HARoleLabel)
		}
		if reflect.DeepEqual(orig.Labels, pod.Labels) {
			continue
		}
		if err := r.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
			return fmt.Errorf("patching HA role on pod %s: %w", pod.Name, err)
		}
		log.Info("updated server pod HA role", "pod", pod.Name, "role", role)
	}

	status.ServerHA = zabbix.ServerHAStatusFromRegistry(nodes)
	return nil
}

// apiToken reads the Zabbix API token from the Secret referenced in
// spec.api.tokenSecretRef. A referenced Zabbix resource without an api
// block (standalone server, no HA maintenance) has no token to read: the
// error surfaces through the caller's condition instead of a panic.
func apiToken(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix) (string, error) {
	if z.Spec.API == nil {
		return "", fmt.Errorf("referenced Zabbix resource %q has no api token configured", z.Name)
	}
	ref := z.Spec.API.TokenSecretRef
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: ref.Name}, &secret); err != nil {
		return "", fmt.Errorf("reading API token secret: %w", err)
	}
	token, ok := secret.Data[ref.Key]
	if !ok || len(token) == 0 {
		return "", fmt.Errorf("API token secret %q has an empty key %q", ref.Name, ref.Key)
	}
	return string(token), nil
}

// ensureClusterMetrics converges the cluster-metrics ingestion and
// reports readiness. Steps: cluster-scoped RBAC (both identities), the
// monitor token (the service-account token controller fills the Secret
// asynchronously), then the Zabbix-side host registration. Failures set
// the ClusterMetricsReady condition and return false so the next
// reconcile retries at a human pace.
func (r *ZabbixReconciler) ensureClusterMetrics(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) bool {
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
		setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"ClusterRBACFailed", fmt.Sprintf("reconciling ClusterRole %s: %v", role.Name, err), z.Generation)
		return false
	}
	binding := zabbix.ClusterMetricsClusterRoleBinding(z)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error { return nil }); err != nil {
		setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"ClusterRBACFailed", fmt.Sprintf("reconciling ClusterRoleBinding %s: %v", binding.Name, err), z.Generation)
		return false
	}

	// The monitor token: created as an empty service-account-token
	// Secret, filled asynchronously by the token controller.
	monitorTokenValue, err := monitorToken(ctx, r.Client, z)
	if err != nil {
		setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"MonitorTokenPending", err.Error(), z.Generation)
		return false
	}

	apiTokenValue, err := apiToken(ctx, r.Client, z)
	if err != nil {
		setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"APITokenPending", err.Error(), z.Generation)
		return false
	}

	hostID, created, err := zabbix.EnsureClusterHost(ctx, r.HTTP, zabbix.APIEndpoint(z), apiTokenValue,
		cm.ClusterName, monitorTokenValue, cm.TLSVerify)
	if err != nil {
		setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionFalse,
			"HostRegistrationFailed", fmt.Sprintf("registering cluster host %q: %v", cm.ClusterName, err), z.Generation)
		return false
	}
	if created {
		log.Info("Created cluster host", "host", cm.ClusterName, "hostid", hostID)
	}

	setCond(status, zabbixv1alpha1.ConditionClusterMetricsReady, metav1.ConditionTrue,
		"ClusterHostRegistered",
		fmt.Sprintf("cluster host %q registered with template %q (hostid %d)", cm.ClusterName, zabbix.ClusterTemplate, hostID),
		z.Generation)
	status.ClusterMetricsHostName = cm.ClusterName
	return true
}

// publishComponentReadiness checks the workloads' rollout state and
// publishes the per-component conditions. Returns the aggregate
// components readiness.
func (r *ZabbixReconciler) publishComponentReadiness(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) (bool, error) {
	serverReady, err := zabbix.StatefulSetIsReady(ctx, r.Client, z.Namespace,
		zabbix.ServerName(z.Name), zabbix.ServerReplicas(z))
	if err != nil {
		return false, fmt.Errorf("checking server readiness: %w", err)
	}
	setCond(status, zabbixv1alpha1.ConditionServerReady, readyCondition(serverReady), "ServerStatefulSet", statefulSetMessage("server", serverReady), z.Generation)

	frontendReady, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
		zabbix.FrontendName(z.Name), frontendReplicas(z))
	if err != nil {
		return false, fmt.Errorf("checking frontend readiness: %w", err)
	}
	setCond(status, zabbixv1alpha1.ConditionFrontendReady, readyCondition(frontendReady), "FrontendDeployment", deploymentMessage("frontend", frontendReady), z.Generation)

	componentsReady := serverReady && frontendReady

	if z.Spec.WebService != nil {
		webServiceReady, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
			zabbix.WebServiceName(z.Name), webServiceReplicas(z))
		if err != nil {
			return false, fmt.Errorf("checking web service readiness: %w", err)
		}
		setCond(status, zabbixv1alpha1.ConditionWebServiceReady, readyCondition(webServiceReady), "WebServiceDeployment", deploymentMessage("web service", webServiceReady), z.Generation)
		componentsReady = componentsReady && webServiceReady
	}
	return componentsReady, nil
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
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting cluster-metrics object %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

func statefulSetMessage(component string, ready bool) string {
	if ready {
		return fmt.Sprintf("%s statefulset is ready", component)
	}
	return fmt.Sprintf("%s statefulset is not ready yet", component)
}

// dropCondition removes one condition type from the status copy — used
// when a feature is disabled and its (stale) condition must not linger.
func dropCondition(status *zabbixv1alpha1.ZabbixStatus, t zabbixv1alpha1.ComponentConditionType) {
	kept := make([]metav1.Condition, 0, len(status.Conditions))
	for _, c := range status.Conditions {
		if c.Type != string(t) {
			kept = append(kept, c)
		}
	}
	status.Conditions = kept
}

func setCond(status *zabbixv1alpha1.ZabbixStatus, t zabbixv1alpha1.ComponentConditionType, s metav1.ConditionStatus, reason, message string, generation int64) {
	zabbix.SetCondition(&status.Conditions, string(t), s, reason, message, generation)
}

func readyCondition(ready bool) metav1.ConditionStatus {
	if ready {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func deploymentMessage(component string, ready bool) string {
	if ready {
		return fmt.Sprintf("%s deployment is ready", component)
	}
	return fmt.Sprintf("%s deployment is not ready yet", component)
}

func frontendReplicas(z *zabbixv1alpha1.Zabbix) int32 {
	if z.Spec.Frontend != nil && z.Spec.Frontend.Replicas != nil {
		return *z.Spec.Frontend.Replicas
	}
	return 1
}

func webServiceReplicas(z *zabbixv1alpha1.Zabbix) int32 {
	if z.Spec.WebService != nil && z.Spec.WebService.Replicas != nil {
		return *z.Spec.WebService.Replicas
	}
	return 1
}

// SetupWithManager sets up the controller with the Manager. Reconciles are
// triggered by the Zabbix resource and by changes to objects it owns.
func (r *ZabbixReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&zabbixv1alpha1.Zabbix{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("zabbix").
		Complete(r)
}
