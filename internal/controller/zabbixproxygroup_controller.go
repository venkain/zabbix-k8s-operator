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
	"regexp"
	"slices"
	"strconv"
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

// ProxyGroupFinalizer guards server-side API cleanup: deleting the
// resource must delete the operator-managed group and its member proxy
// records on the Zabbix server, which owner references cannot reach (the
// server-side configuration lives in Zabbix's database, not Kubernetes).
const ProxyGroupFinalizer = "zabbix.venkain.io/proxygroup-cleanup"

// proxyGroupRetryDelay is the reconcile cadence while the deployment is
// live: cheap proxygroup.get/proxy.get reads keep the group state and
// membership aligned with the server at human pace.
const proxyGroupRetryDelay = 30 * time.Second

// proxyGroupRefRetryDelay is the requeue delay when the referenced Zabbix
// resource does not exist (yet) — same human-pace behaviour as the
// database retry of the Zabbix controller.
const proxyGroupRefRetryDelay = 30 * time.Second

// ZabbixProxyGroupReconciler reconciles a ZabbixProxyGroup object. A
// group deploys its member proxies (StatefulSet + per-ordinal headless
// DNS) and maintains the server-side configuration through the Zabbix
// API: the proxygroup object itself plus one proxy record per member, so
// the running daemons (which self-identify by pod name) register into
// the group.
type ZabbixProxyGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTP is the client used for Zabbix API calls (nil = default).
	HTTP *http.Client
}

// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixproxygroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixproxygroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixproxygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch

// Reconcile moves the cluster and the referenced Zabbix server toward
// the state described by the ZabbixProxyGroup resource.
func (r *ZabbixProxyGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pg zabbixv1alpha1.ZabbixProxyGroup
	if err := r.Get(ctx, req.NamespacedName, &pg); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted: workload objects were garbage-collected through
			// owner references; the server-side cleanup happened under
			// the finalizer below.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ZabbixProxyGroup %s: %w", req.NamespacedName, err)
	}

	if !pg.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &pg)
	}

	status := pg.Status.DeepCopy()
	status.ObservedGeneration = pg.Generation

	// 1. Resolve the referenced Zabbix resource: it carries the server
	// wiring (the active-node Service the proxies connect to) and the
	// API token secret of the server side.
	var z zabbixv1alpha1.Zabbix
	if err := r.Get(ctx, client.ObjectKey{Namespace: pg.Namespace, Name: pg.Spec.ServerRef}, &z); err != nil {
		if apierrors.IsNotFound(err) {
			setPGCond(status, zabbixv1alpha1.ConditionProxyGroupServerResolved, "False",
				"ServerRefUnresolved", fmt.Sprintf("referenced Zabbix resource %q does not exist (yet)", pg.Spec.ServerRef), pg.Generation)
			return persistProxyGroupStatus(ctx, r.Client, &pg, *status, proxyGroupRefRetryDelay)
		}
		return ctrl.Result{}, fmt.Errorf("getting referenced Zabbix %q: %w", pg.Spec.ServerRef, err)
	}
	setPGCond(status, zabbixv1alpha1.ConditionProxyGroupServerResolved, "True",
		"ServerRefResolved", fmt.Sprintf("referenced Zabbix resource %q resolved", pg.Spec.ServerRef), pg.Generation)

	// 2. Finalizer comes first: once the API maintenance below has
	// created records server-side, deletion must be able to clean them
	// up. Idempotent (no-op when already present).
	if !controllerutil.ContainsFinalizer(&pg, ProxyGroupFinalizer) {
		controllerutil.AddFinalizer(&pg, ProxyGroupFinalizer)
		if err := r.Update(ctx, &pg); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding cleanup finalizer: %w", err)
		}
	}

	// 3. Converge the member workload (StatefulSet + headless Service).
	replicas := zabbix.DesiredProxyReplicas(&pg)
	if err := zabbix.Converge(ctx, r.Client, &pg, zabbix.ProxyStatefulSet(&pg, replicas, &z)); err != nil {
		return ctrl.Result{}, fmt.Errorf("converging proxy statefulset: %w", err)
	}
	if err := zabbix.Converge(ctx, r.Client, &pg, zabbix.ProxyHeadlessService(&pg)); err != nil {
		return ctrl.Result{}, fmt.Errorf("converging proxy headless service: %w", err)
	}
	// Voluntary-disruption guard: draining a node takes down at most one
	// member proxy at a time (etched by DisruptionBudget).
	if err := zabbix.Converge(ctx, r.Client, &pg, zabbix.ProxyDisruptionBudget(&pg)); err != nil {
		return ctrl.Result{}, fmt.Errorf("converging proxy disruption budget: %w", err)
	}

	// 3.5. Un-wedge the rollout: a member pod that crash-loops on an
	// older template revision starves its own replacement (it counts
	// against the RollingUpdate budget and is never Ready, so the
	// StatefulSet controller cannot make progress). Recycling such pods
	// lets the controller recreate them from the converged template.
	// No-op in steady state: only stale-revision pods that are not
	// Ready (and not already terminating) are removed.
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: pg.Namespace, Name: zabbix.ProxyName(pg.Name)}, &sts); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting proxy statefulset: %w", err)
	}
	if err := zabbix.RecycleStalePods(ctx, r.Client, &sts); err != nil {
		return ctrl.Result{}, fmt.Errorf("recycling stale member pods: %w", err)
	}

	// 4. Deploy readiness.
	ready, err := zabbix.StatefulSetIsReady(ctx, r.Client, pg.Namespace, zabbix.ProxyName(pg.Name), replicas)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking proxy group readiness: %w", err)
	}
	readyMsg := "proxy group statefulset is ready"
	if !ready {
		readyMsg = "proxy group statefulset is not ready yet"
	}
	setPGCond(status, zabbixv1alpha1.ConditionProxyGroupReady, readyCondition(ready),
		"ProxyStatefulSet", readyMsg, pg.Generation)

	// 5. API maintenance: group object and per-member proxy records must
	// exist server-side for the daemons to register into the group. The
	// error requeues with backoff; the converged workload keeps running.
	if err := r.maintainProxyGroup(ctx, &pg, status, &z); err != nil {
		return ctrl.Result{}, fmt.Errorf("maintaining proxy group: %w", err)
	}

	return persistProxyGroupStatus(ctx, r.Client, &pg, *status, proxyGroupRetryDelay)
}

// maintainProxyGroup aligns the server-side configuration: the proxygroup
// object (created with the spec settings, updated when they drift) and
// the per-member proxy records (created for missing members, corrected
// for drifted local addresses, deleted for scaled-down ordinals). It
// publishes the observed group state on the status.
func (r *ZabbixProxyGroupReconciler) maintainProxyGroup(ctx context.Context, pg *zabbixv1alpha1.ZabbixProxyGroup, status *zabbixv1alpha1.ZabbixProxyGroupStatus, z *zabbixv1alpha1.Zabbix) error {
	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
			"TokenUnresolved", err.Error(), pg.Generation)
		return err
	}
	endpoint := zabbix.APIEndpoint(z)

	group, err := zabbix.FetchProxyGroup(ctx, r.HTTP, endpoint, token, pg.Spec.GroupName)
	if err != nil {
		setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
			"APIUnreachable", err.Error(), pg.Generation)
		return err
	}

	failoverDelay, minOnline := zabbix.DesiredProxyGroupSettings(pg)
	switch {
	case group == nil:
		id, err := zabbix.CreateProxyGroup(ctx, r.HTTP, endpoint, token, pg.Spec.GroupName, failoverDelay, minOnline)
		if err != nil {
			setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
				"GroupCreateFailed", err.Error(), pg.Generation)
			return err
		}
		group = &zabbix.ProxyGroup{ID: id, Name: pg.Spec.GroupName}
	case group.FailoverDelay != failoverDelay || group.MinOnline != minOnline:
		if err := zabbix.UpdateProxyGroup(ctx, r.HTTP, endpoint, token, group.ID, failoverDelay, minOnline); err != nil {
			setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
				"GroupUpdateFailed", err.Error(), pg.Generation)
			return err
		}
	}

	members, err := zabbix.FetchGroupProxies(ctx, r.HTTP, endpoint, token, group.ID)
	if err != nil {
		setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
			"APIUnreachable", err.Error(), pg.Generation)
		return err
	}

	syncErr := syncGroupMembers(ctx, r, endpoint, token, group.ID, pg, members)
	if syncErr != nil {
		setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "False",
			"MembersNotSynced", syncErr.Error(), pg.Generation)
		return syncErr
	}

	state := string(group.State)
	setPGCond(status, zabbixv1alpha1.ConditionProxyGroupAPIReady, "True",
		"GroupInSync", fmt.Sprintf("group %q managed (state %s)", pg.Spec.GroupName, state), pg.Generation)
	status.GroupState = state
	return nil
}

// syncGroupMembers converges the member proxy records. Desired members
// are ordinals 0..replicas-1 of this instance; only records named after
// this instance's pods are ever mutated or deleted, so user-created
// proxies in a shared group are left alone.
func syncGroupMembers(ctx context.Context, r *ZabbixProxyGroupReconciler, endpoint, token string, groupID int64, pg *zabbixv1alpha1.ZabbixProxyGroup, members []zabbix.ProxyMember) error {
	replicas := zabbix.DesiredProxyReplicas(pg)
	desiredHosts := make(map[string]string, replicas)
	for i := range replicas {
		desiredHosts[zabbix.ProxyRecordName(pg.Name, i)] = zabbix.ProxyRecordAddress(pg.Name, i, pg.Namespace)
	}

	liveByID := make(map[string]zabbix.ProxyMember, len(members))
	for _, m := range members {
		liveByID[m.Name] = m
	}

	// Deletes first: scaled-down or renamed members that still exist
	// (sorted for determinism only).
	var stale []string
	for host, m := range liveByID {
		if _, wanted := desiredHosts[host]; !wanted && ownedProxyHost(pg.Name, host) {
			stale = append(stale, strconv.FormatInt(m.ID, 10))
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		if err := zabbix.DeleteProxyMembers(ctx, r.HTTP, endpoint, token, stale); err != nil {
			return fmt.Errorf("deleting stale proxy members: %w", err)
		}
	}

	for host, address := range desiredHosts {
		live, exists := liveByID[host]
		switch {
		case !exists:
			if _, err := zabbix.CreateProxyMember(ctx, r.HTTP, endpoint, token, groupID, host, address, strconv.Itoa(int(zabbix.ProxyPort))); err != nil {
				return fmt.Errorf("creating proxy member %s: %w", host, err)
			}
		case live.LocalAddress != address || live.LocalPort != strconv.Itoa(int(zabbix.ProxyPort)):
			if err := zabbix.UpdateProxyMember(ctx, r.HTTP, endpoint, token, live.ID, address, strconv.Itoa(int(zabbix.ProxyPort))); err != nil {
				return fmt.Errorf("updating proxy member %s: %w", host, err)
			}
		}
	}
	return nil
}

// ownedProxyHostPattern matches members this instance created
// (<pg.Name>-proxy-<ordinal>); anything else in the group belongs to
// someone else and is never touched.
var ownedProxyHostPattern = regexp.MustCompile(`-proxy-[0-9]+$`)

func ownedProxyHost(instance, host string) bool {
	return len(host) > len(instance) &&
		host[:len(instance)] == instance &&
		ownedProxyHostPattern.MatchString(host[len(instance):])
}

// finalize deletes the operator-managed server-side configuration before
// the resource (and its Kubernetes objects) disappear. The referenced
// Zabbix resource may already be gone — then there is nothing reachable
// to clean up and the finalizer is released immediately.
func (r *ZabbixProxyGroupReconciler) finalize(ctx context.Context, pg *zabbixv1alpha1.ZabbixProxyGroup) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(pg, ProxyGroupFinalizer) {
		return ctrl.Result{}, nil
	}

	var z zabbixv1alpha1.Zabbix
	if err := r.Get(ctx, client.ObjectKey{Namespace: pg.Namespace, Name: pg.Spec.ServerRef}, &z); err == nil {
		if err := r.deleteServerSideGroup(ctx, pg, &z); err != nil {
			// Cleanup retried through requeues: the finalizer holds the
			// deletion until the API is reachable again.
			return ctrl.Result{}, fmt.Errorf("cleaning up proxy group on the server: %w", err)
		}
		log.Info("deleted proxy group and members on the server", "group", pg.Spec.GroupName)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("getting referenced Zabbix during cleanup: %w", err)
	}

	controllerutil.RemoveFinalizer(pg, ProxyGroupFinalizer)
	if err := r.Update(ctx, pg); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing cleanup finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// deleteServerSideGroup removes the member proxy records this instance
// created, then the group object itself (members of a deleted group are
// unassigned server-side, not deleted).
func (r *ZabbixProxyGroupReconciler) deleteServerSideGroup(ctx context.Context, pg *zabbixv1alpha1.ZabbixProxyGroup, z *zabbixv1alpha1.Zabbix) error {
	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		return err
	}
	endpoint := zabbix.APIEndpoint(z)

	group, err := zabbix.FetchProxyGroup(ctx, r.HTTP, endpoint, token, pg.Spec.GroupName)
	if err != nil {
		return err
	}
	if group == nil {
		return nil
	}

	members, err := zabbix.FetchGroupProxies(ctx, r.HTTP, endpoint, token, group.ID)
	if err != nil {
		return err
	}
	var memberIDs []string
	for _, m := range members {
		if ownedProxyHost(pg.Name, m.Name) {
			memberIDs = append(memberIDs, strconv.FormatInt(m.ID, 10))
		}
	}
	if len(memberIDs) > 0 {
		if err := zabbix.DeleteProxyMembers(ctx, r.HTTP, endpoint, token, memberIDs); err != nil {
			return err
		}
	}
	return zabbix.DeleteProxyGroup(ctx, r.HTTP, endpoint, token, group.ID)
}

func setPGCond(status *zabbixv1alpha1.ZabbixProxyGroupStatus, t zabbixv1alpha1.ProxyGroupConditionType, s metav1.ConditionStatus, reason, message string, generation int64) {
	zabbix.SetCondition(&status.Conditions, string(t), s, reason, message, generation)
}

// persistProxyGroupStatus writes the given status only when it differs
// from the stored one (no-op writes would hot-loop the controller), with
// one conflict-retry on a fresh read.
func persistProxyGroupStatus(ctx context.Context, c client.Client, pg *zabbixv1alpha1.ZabbixProxyGroup, status zabbixv1alpha1.ZabbixProxyGroupStatus, requeue time.Duration) (ctrl.Result, error) {
	if reflect.DeepEqual(pg.Status, status) {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	pg.Status = status
	for attempt := range 2 {
		if attempt > 0 {
			fresh := &zabbixv1alpha1.ZabbixProxyGroup{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pg), fresh); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-reading for status update: %w", err)
			}
			fresh.Status = status
			pg = fresh
		}
		if err := c.Status().Update(ctx, pg); err == nil {
			return ctrl.Result{RequeueAfter: requeue}, nil
		} else if !apierrors.IsConflict(err) {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// SetupWithManager sets up the controller with the Manager. Reconciles
// are triggered by the ZabbixProxyGroup resource and by objects it owns.
func (r *ZabbixProxyGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&zabbixv1alpha1.ZabbixProxyGroup{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Named("zabbixproxygroup").
		Complete(r)
}
