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
// with the Zabbix resource as owner. Every owned object flows through here.
//
// Convergence uses server-side apply: the API server merges and compares
// only the fields present in the built object, so API defaulting (e.g.
// revisionHistoryLimit, progressDeadlineSeconds, protocol) never shows up
// as a diff. A repeated apply of an unchanged configuration is a server
// -side no-op — no update, no watch event, no reconcile loop. This is why
// Converge does not use controllerutil.CreateOrUpdate with wholesale spec
// reapplication: the client-side deep-equal there sees defaulted live
// fields as drift and updates every object on every reconcile (a storm).
func Converge(ctx context.Context, c client.Client, owner *zabbixv1alpha1.Zabbix, desired client.Object) error {
	logger := log.FromContext(ctx).WithValues("object", fmt.Sprintf("%T", desired), "name", desired.GetName())

	// Ownership must be part of the applied configuration so the created
	// object is owned (and garbage-collected) from birth.
	if err := controllerutil.SetControllerReference(owner, desired, c.Scheme()); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	// Apply configurations carry their GVK explicitly.
	var gvk schema.GroupVersionKind
	switch desired.(type) {
	case *appsv1.Deployment:
		gvk = appsv1.SchemeGroupVersion.WithKind("Deployment")
	case *corev1.Service:
		gvk = corev1.SchemeGroupVersion.WithKind("Service")
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
