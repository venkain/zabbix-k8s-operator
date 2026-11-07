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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// databaseRetryDelay is the requeue delay when the database connection
// details cannot be resolved yet (Secret missing or incomplete). The
// database may still be provisioning (e.g. a CloudNativePG Cluster
// coming up), so retry at a human pace rather than error-spinning.
const databaseRetryDelay = 30 * time.Second

// ZabbixReconciler reconciles a Zabbix object. One resource owns one
// complete Zabbix deployment: server, frontend, web service, and their
// database wiring. All object construction lives in internal/zabbix as
// pure builders; this controller resolves inputs, converges the owned
// objects, and reports observed state on the status subresource.
type ZabbixReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=zabbix.venkain.io,resources=zabbixes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

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
		return ctrl.Result{}, nil
	}

	status := zabbixv1alpha1.ZabbixStatus{
		ObservedGeneration: z.Generation,
	}

	// 1. Resolve the database wiring. Without it nothing can run.
	db, err := zabbix.ResolveDatabase(ctx, r.Client, z.Namespace, z.Spec.Database)
	if err != nil {
		log.Error(err, "database resolution failed; requeueing")
		setCond(&status, zabbixv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse, "DatabaseUnresolved", err.Error(), z.Generation)
		return persistStatus(ctx, r.Client, &z, status, databaseRetryDelay)
	}
	setCond(&status, zabbixv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue, "DatabaseResolved",
		fmt.Sprintf("connection details resolved from secret %s", db.SecretName()), z.Generation)

	// 2. Converge every owned object.
	if err := zabbix.ConvergeAll(ctx, r.Client, &z, db); err != nil {
		return ctrl.Result{}, fmt.Errorf("converging Zabbix %s: %w", req.NamespacedName, err)
	}

	// 3. Publish component readiness.
	serverReady, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
		zabbix.ServerName(z.Name), zabbix.ServerReplicas(&z))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking server readiness: %w", err)
	}
	setCond(&status, zabbixv1alpha1.ConditionServerReady, readyCondition(serverReady), "ServerDeployment", deploymentMessage("server", serverReady), z.Generation)

	frontendReady, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
		zabbix.FrontendName(z.Name), frontendReplicas(&z))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking frontend readiness: %w", err)
	}
	setCond(&status, zabbixv1alpha1.ConditionFrontendReady, readyCondition(frontendReady), "FrontendDeployment", deploymentMessage("frontend", frontendReady), z.Generation)

	componentsReady := serverReady && frontendReady

	if z.Spec.WebService != nil {
		webServiceReady, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
			zabbix.WebServiceName(z.Name), webServiceReplicas(&z))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking web service readiness: %w", err)
		}
		setCond(&status, zabbixv1alpha1.ConditionWebServiceReady, readyCondition(webServiceReady), "WebServiceDeployment", deploymentMessage("web service", webServiceReady), z.Generation)
		componentsReady = componentsReady && webServiceReady
	}

	readyMsg := "all components ready"
	if !componentsReady {
		readyMsg = "waiting for components to become ready"
	}
	setCond(&status, zabbixv1alpha1.ConditionReady, readyCondition(componentsReady), "ComponentsReady", readyMsg, z.Generation)

	return persistStatus(ctx, r.Client, &z, status, 0)
}

// persistStatus writes the given status and returns the reconcile result.
func persistStatus(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix, status zabbixv1alpha1.ZabbixStatus, requeue time.Duration) (ctrl.Result, error) {
	z.Status = status
	if err := c.Status().Update(ctx, z); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

func setCond(status *zabbixv1alpha1.ZabbixStatus, t zabbixv1alpha1.ComponentConditionType, s metav1.ConditionStatus, reason, message string, generation int64) {
	zabbix.SetCondition(status, string(t), s, reason, message, generation)
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
		Owns(&corev1.Service{}).
		Named("zabbix").
		Complete(r)
}
