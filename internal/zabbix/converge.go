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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Converge creates or updates obj so it matches the built (desired) state,
// with the Zabbix resource as owner. It reports whether anything changed.
// Every owned object flows through here — the single place enforcing the
// create-or-update discipline (no drift, no orphans): the built object is
// the authoritative template and its spec/labels are re-applied on every
// reconcile; status is a subresource and never touched.
func Converge(ctx context.Context, c client.Client, owner *zabbixv1alpha1.Zabbix, desired client.Object) (controllerutil.OperationResult, error) {
	log := log.FromContext(ctx).WithValues("object", fmt.Sprintf("%T", desired), "name", desired.GetName())

	// Ownership must be set before CreateOrUpdate: on the create path the
	// mutator is not invoked, so the object is created as built.
	if err := controllerutil.SetControllerReference(owner, desired, c.Scheme()); err != nil {
		return "", fmt.Errorf("setting controller reference: %w", err)
	}

	// Preserve the built desired state; CreateOrUpdate replaces the passed
	// object with the live one before mutating.
	template := desired.DeepCopyObject().(client.Object)

	result, err := controllerutil.CreateOrUpdate(ctx, c, desired, func() error {
		switch tmpl := template.(type) {
		case *appsv1.Deployment:
			live := desired.(*appsv1.Deployment)
			live.Spec = tmpl.Spec
			live.Labels = tmpl.Labels
			live.Annotations = tmpl.Annotations
			live.OwnerReferences = tmpl.OwnerReferences
		case *corev1.Service:
			live := desired.(*corev1.Service)
			// Keep cluster-allocated values: the API rejects a Service
			// update that clears ClusterIP, and a dropped NodePort would
			// cause reallocation churn.
			clusterIP := live.Spec.ClusterIP
			clusterIPs := live.Spec.ClusterIPs
			nodePorts := serviceNodePorts(live)
			live.Spec = tmpl.Spec
			live.Spec.ClusterIP = clusterIP
			live.Spec.ClusterIPs = clusterIPs
			restoreNodePorts(live, nodePorts)
			live.Labels = tmpl.Labels
			live.Annotations = tmpl.Annotations
			live.OwnerReferences = tmpl.OwnerReferences
		default:
			return fmt.Errorf("unsupported owned object type %T", template)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if result != controllerutil.OperationResultNone {
		log.Info("converged object", "operation", result)
	}
	return result, nil
}

// ConvergeAll creates/updates every object the Zabbix resource owns.
func ConvergeAll(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix, db *DatabaseEnv) error {
	objects := []client.Object{
		ServerDeployment(z, db),
		ServerService(z),
		ActiveServerService(z),
		FrontendDeployment(z, db),
		FrontendService(z),
	}

	if z.Spec.WebService != nil {
		objects = append(objects,
			WebServiceDeployment(z),
			WebServiceService(z),
		)
	}

	for _, obj := range objects {
		if _, err := Converge(ctx, c, z, obj); err != nil {
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

// SetCondition applies a condition on the Zabbix status.
func SetCondition(status *zabbixv1alpha1.ZabbixStatus, condType string, condStatus metav1.ConditionStatus, reason, message string, generation int64) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}

// serviceNodePorts captures the allocated node ports by port name (empty
// name = first port).
func serviceNodePorts(svc *corev1.Service) map[string]int32 {
	ports := make(map[string]int32, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		if p.NodePort != 0 {
			key := p.Name
			if key == "" {
				key = fmt.Sprintf("#%d", i)
			}
			ports[key] = p.NodePort
		}
	}
	return ports
}

// restoreNodePorts reapplies captured node ports onto the converged spec.
func restoreNodePorts(svc *corev1.Service, nodePorts map[string]int32) {
	if len(nodePorts) == 0 {
		return
	}
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		key := p.Name
		if key == "" {
			key = fmt.Sprintf("#%d", i)
		}
		if np, ok := nodePorts[key]; ok {
			p.NodePort = np
		}
	}
}
