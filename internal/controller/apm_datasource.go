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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ensureAPMDataSource converges the global APM telemetry data source
// (the settings.apm_global_db object, "Administration → Data source")
// with the instance's ClickHouse telemetry storage: enabled with the
// storage's endpoint/database/credentials while the storage is
// configured, cleared when it is removed. Coordinates are memoized on
// the status so the clear path only ever touches a data source this
// instance ensured itself. Failures are non-fatal: the condition
// reports them and the delay keeps the reconcile retrying at a human
// pace.
func (r *ZabbixReconciler) ensureAPMDataSource(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) time.Duration {
	log := logf.FromContext(ctx)

	if z.Spec.TelemetryStorage == nil || !zabbix.SupportsTelemetry(z.Spec.Version) {
		if err := r.clearAPMDataSource(ctx, z, status); err != nil {
			setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionFalse,
				"DataSourceClearFailed", err.Error(), z.Generation)
			return apmDataSourceRetryDelay
		}
		// Nothing of ours is left to clear: without the telemetry
		// storage the data source is not an operator-owned concern.
		dropCondition(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady)
		return 0
	}

	if z.Spec.API == nil {
		// No API token: the global data source cannot be managed. The
		// frontend/server environment wiring keeps APM functional for
		// instances that configure a data source themselves.
		dropCondition(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady)
		return 0
	}

	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionFalse,
			"APITokenPending", err.Error(), z.Generation)
		return apmDataSourceRetryDelay
	}
	endpoint := zabbix.APIEndpoint(z)
	ds, err := zabbix.FetchAPMGlobalDB(ctx, r.HTTP, endpoint, token)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionFalse,
			"APIUnreachable", err.Error(), z.Generation)
		return apmDataSourceRetryDelay
	}

	username, password, err := clickHouseAPMCredentials(ctx, r.Client, z)
	if err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionFalse,
			"CredentialsUnresolved", err.Error(), z.Generation)
		return apmDataSourceRetryDelay
	}

	desired := zabbix.APMGlobalDB{
		Status:   zabbix.APMStatusEnabled,
		URL:      zabbix.ClickHouseEndpoint(z),
		Database: zabbix.ClickHouseDatabase(z),
		Username: username,
	}
	if ds.Status != desired.Status || ds.URL != desired.URL ||
		ds.Database != desired.Database || ds.Username != desired.Username {
		// Diff-based write: settings.get exposes every field except the
		// write-only password, so an unchanged data source is never
		// rewritten (and never re-encrypts the password server-side).
		if err := zabbix.UpdateAPMGlobalDB(ctx, r.HTTP, endpoint, token, password, desired); err != nil {
			setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionFalse,
				"DataSourceUpdateFailed", err.Error(), z.Generation)
			return apmDataSourceRetryDelay
		}
		log.Info("ensured the global APM data source", "url", desired.URL, "db", desired.Database)
	}
	status.APMDataSourceURL = desired.URL
	status.APMDataSourceDB = desired.Database
	setCond(&status.Conditions, zabbixv1alpha1.ConditionDataSourceReady, metav1.ConditionTrue,
		"DataSourceEnsured", fmt.Sprintf("global APM data source points at %s (%s)", desired.URL, desired.Database), z.Generation)
	return apmDataSourceRetryDelay
}

// clearAPMDataSource disables the global APM data source when it still
// carries the coordinates this instance last ensured. A data source
// pointing elsewhere (configured by hand in the frontend) is never
// touched.
func (r *ZabbixReconciler) clearAPMDataSource(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) error {
	if z.Spec.API == nil || status.APMDataSourceURL == "" {
		return nil
	}
	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		return err
	}
	ds, err := zabbix.FetchAPMGlobalDB(ctx, r.HTTP, zabbix.APIEndpoint(z), token)
	if err != nil {
		return err
	}
	if ds.Status != zabbix.APMStatusEnabled || ds.URL != status.APMDataSourceURL ||
		ds.Database != status.APMDataSourceDB {
		// Already disabled, or configured to coordinates this instance
		// never ensured — leave it alone.
		status.APMDataSourceURL = ""
		status.APMDataSourceDB = ""
		return nil
	}
	disabled := zabbix.APMGlobalDB{Status: zabbix.APMStatusDisabled}
	if err := zabbix.UpdateAPMGlobalDB(ctx, r.HTTP, zabbix.APIEndpoint(z), token, "", disabled); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("cleared the global APM data source", "url", status.APMDataSourceURL, "db", status.APMDataSourceDB)
	status.APMDataSourceURL = ""
	status.APMDataSourceDB = ""
	return nil
}

// clickHouseAPMCredentials resolves the ClickHouse login for the global
// APM data source from the effective credential Secret (the managed
// auth Secret, or the referenced credentials Secret in external mode).
// Empty key references fall back to the documented defaults
// username/password — the same defaults the credential file mounts use
// (secretMountFile in internal/zabbix).
func clickHouseAPMCredentials(ctx context.Context, c client.Client, z *zabbixv1alpha1.Zabbix) (string, string, error) {
	creds := zabbix.ClickHouseCreds(z)
	usernameKey, passwordKey := creds.UsernameKey, creds.PasswordKey
	if usernameKey == "" {
		usernameKey = "username"
	}
	if passwordKey == "" {
		passwordKey = "password"
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: creds.SecretName}, &secret); err != nil {
		return "", "", fmt.Errorf("reading ClickHouse credentials secret %q: %w", creds.SecretName, err)
	}
	username, ok := secret.Data[usernameKey]
	if !ok || len(username) == 0 {
		return "", "", fmt.Errorf("ClickHouse credentials secret %q has an empty key %q", creds.SecretName, usernameKey)
	}
	password, ok := secret.Data[passwordKey]
	if !ok || len(password) == 0 {
		return "", "", fmt.Errorf("ClickHouse credentials secret %q has an empty key %q", creds.SecretName, passwordKey)
	}
	return string(username), string(password), nil
}
