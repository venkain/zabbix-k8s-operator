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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// reconcileManagedClickHouse drives the managed backend lifecycle:
// ensure-or-remove the auth Secret and workload objects, publish the
// TelemetryStorageReady condition, and return the readiness requeue
// delay while the pod is still starting.
func (r *ZabbixReconciler) reconcileManagedClickHouse(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) (time.Duration, error) {
	if !zabbix.ManagedClickHouseEnabled(z) {
		// Never enabled, or switched to an external endpoint: remove
		// the managed objects (the auth Secret has an owner reference,
		// but the resource itself stays alive on a mode switch — GC
		// cannot help there).
		if err := r.removeManagedClickHouse(ctx, z); err != nil {
			return 0, fmt.Errorf("removing managed clickhouse backend: %w", err)
		}
		dropCondition(status, zabbixv1alpha1.ConditionTelemetryStorageReady)
		return 0, nil
	}
	// The auth Secret must exist before the server pods (which mount
	// it) are converged.
	if err := r.ensureClickHouseAuth(ctx, z); err != nil {
		return 0, fmt.Errorf("ensuring clickhouse credentials: %w", err)
	}
	ready, err := zabbix.StatefulSetIsReady(ctx, r.Client, z.Namespace,
		zabbix.ClickHouseName(z.Name), 1)
	if err != nil {
		return 0, fmt.Errorf("checking clickhouse readiness: %w", err)
	}
	setCond(status, zabbixv1alpha1.ConditionTelemetryStorageReady, readyCondition(ready),
		"ClickHouseStatefulSet", statefulSetMessage("clickhouse", ready), z.Generation)
	if !ready {
		return telemetryRetryDelay, nil
	}
	return 0, nil
}

// ensureClickHouseAuth creates the managed backend's credentials Secret
// once (get-or-create: the password must NEVER rotate behind the
// ClickHouse pod's back — the user was already provisioned with it) and
// fails with a clear error when an existing Secret lost its keys.
func (r *ZabbixReconciler) ensureClickHouseAuth(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	key := client.ObjectKey{Namespace: z.Namespace, Name: zabbix.ClickHouseAuthSecretName(z.Name)}
	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		password, err := zabbix.GenerateClickHousePassword()
		if err != nil {
			return err
		}
		secret := zabbix.ClickHouseAuthSecret(z, password)
		if err := controllerutil.SetControllerReference(z, secret, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("creating clickhouse auth secret: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("getting clickhouse auth secret: %w", err)
	}
	if existing.Data == nil || len(existing.Data["password"]) == 0 || len(existing.Data["username"]) == 0 {
		return fmt.Errorf("clickhouse auth Secret %q is missing its username/password keys; restore them or delete the Secret (a fresh password only applies to a wiped ClickHouse data volume)", key.Name)
	}
	return nil
}

// removeManagedClickHouse deletes the managed backend objects when
// managed mode is off (never enabled, or switched to an external
// endpoint). Already-gone objects are not an error. The auth Secret is
// deleted too — its password is meaningless without the backend, and
// keeping it would block a later re-enable from re-seeding fresh
// credentials.
func (r *ZabbixReconciler) removeManagedClickHouse(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	// Name-only stubs: the full builders dereference the (unset)
	// telemetryStorage spec; deletion only needs the object names.
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: zabbix.ClickHouseName(z.Name), Namespace: z.Namespace,
	}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: zabbix.ClickHouseName(z.Name), Namespace: z.Namespace,
	}}
	for _, obj := range []client.Object{sts, svc} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting managed clickhouse object %s: %w", obj.GetName(), err)
		}
	}
	auth := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: zabbix.ClickHouseAuthSecretName(z.Name), Namespace: z.Namespace,
	}}
	if err := r.Delete(ctx, auth); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting managed clickhouse auth secret: %w", err)
	}
	return nil
}
