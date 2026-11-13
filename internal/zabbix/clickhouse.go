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

package zabbix

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// The managed ClickHouse backend is a single-node
// clickhouse/clickhouse-server StatefulSet fronted by a ClusterIP
// Service. The Zabbix 8.0 requirements list ClickHouse 26.3.0.0–26.9.X.X
// (telemetry query items additionally require ≥ 26.8.17.4), so the
// default tag pins the 26.8 line: inside the supported range and
// telemetry-capable. The entrypoint creates CLICKHOUSE_DB and
// CLICKHOUSE_USER (with CLICKHOUSE_PASSWORD) on first start, so no
// bootstrap job is needed.
const (
	clickhouseImageRepo   = "clickhouse/clickhouse-server"
	clickhouseDefaultTag  = "26.8"
	clickhouseHTTPPort    = int32(8123)
	clickhouseNativePort  = int32(9000)
	portNameClickhouseSQL = "http"
	portNameClickhouseNAT = "native"

	// clickhouseAuthUsername is the fixed Zabbix login the managed
	// backend provisions (the password is operator-generated).
	clickhouseAuthUsername = "zabbix"
	clickhouseUsernameKey  = "username"
	clickhousePasswordKey  = "password"

	// clickhouseDataVolume is the volumeClaimTemplate holding the
	// ClickHouse data directory (/var/lib/clickhouse).
	clickhouseDataVolume = "data"
)

// ManagedClickHouseEnabled reports whether the operator should deploy
// the managed backend: spec.telemetryStorage.clickhouse.managed set on
// a Zabbix 8.0+ instance.
func ManagedClickHouseEnabled(z *zabbixv1alpha1.Zabbix) bool {
	return z.Spec.TelemetryStorage != nil &&
		z.Spec.TelemetryStorage.ClickHouse.Managed != nil &&
		SupportsTelemetry(z.Spec.Version)
}

// ClickHouseEndpoint returns the effective ClickHouse HTTP URL: the
// managed Service address in managed mode, the spec endpoint otherwise.
func ClickHouseEndpoint(z *zabbixv1alpha1.Zabbix) string {
	if ManagedClickHouseEnabled(z) {
		return fmt.Sprintf("http://%s.%s.svc:%d",
			ClickHouseName(z.Name), z.Namespace, clickhouseHTTPPort)
	}
	return z.Spec.TelemetryStorage.ClickHouse.Endpoint
}

// ClickHouseCredentials carries the effective credential source.
type ClickHouseCredentials struct {
	SecretName   string
	UsernameKey  string
	PasswordKey  string
	UsernameFile string
	PasswordFile string
}

// ClickHouseCreds resolves the effective credential source: the
// operator-generated auth Secret in managed mode, the referenced Secret
// in external mode. The _FILE paths assume the standard mount.
func ClickHouseCreds(z *zabbixv1alpha1.Zabbix) ClickHouseCredentials {
	if ManagedClickHouseEnabled(z) {
		secret := ClickHouseName(z.Name) + "-auth"
		return ClickHouseCredentials{
			SecretName:   secret,
			UsernameKey:  clickhouseUsernameKey,
			PasswordKey:  clickhousePasswordKey,
			UsernameFile: secretMountFile(clickhouseCredentialsMount, "", clickhouseUsernameKey),
			PasswordFile: secretMountFile(clickhouseCredentialsMount, "", clickhousePasswordKey),
		}
	}
	creds := z.Spec.TelemetryStorage.ClickHouse.CredentialsSecretRef
	return ClickHouseCredentials{
		SecretName:   creds.SecretName,
		UsernameKey:  creds.UsernameKey,
		PasswordKey:  creds.PasswordKey,
		UsernameFile: secretMountFile(clickhouseCredentialsMount, creds.UsernameKey, clickhouseUsernameKey),
		PasswordFile: secretMountFile(clickhouseCredentialsMount, creds.PasswordKey, clickhousePasswordKey),
	}
}

// ClickHouseAuthSecretName returns the name of the operator-generated
// credentials Secret for the managed backend.
func ClickHouseAuthSecretName(instance string) string {
	return ClickHouseName(instance) + "-auth"
}

// ClickHouseStatefulSet builds the managed single-node ClickHouse
// backend. Single-node by design (Zabbix supports one ClickHouse URL;
// clusters need a load balancer in front) — no PDB either: with one
// replica a minAvailable=1 budget would block node drains entirely.
func ClickHouseStatefulSet(z *zabbixv1alpha1.Zabbix) *appsv1.StatefulSet {
	managed := z.Spec.TelemetryStorage.ClickHouse.Managed
	podLabels := commonLabels(z.Name, ComponentClickHouse)
	replicas := int32(1) // single node by design (see the doc comment)

	image := clickhouseImageRepo + ":" + clickhouseDefaultTag
	if managed.Image != nil {
		image = managed.Image.Repository + ":" + managed.Image.Tag
	}

	// The +kubebuilder:default applies at the API server; the builder
	// mirrors it so the desired object is complete on its own.
	storage := managed.Storage
	if storage.IsZero() {
		storage = resource.MustParse("10Gi")
	}

	container := corev1.Container{
		Name:            "clickhouse",
		Image:           image,
		ImagePullPolicy: pullPolicy(managed.Image),
		Env: []corev1.EnvVar{
			// The entrypoint creates the database and the Zabbix login
			// (with password) on first start — no bootstrap job.
			{Name: "CLICKHOUSE_DB", Value: ClickHouseDatabase(z)},
			{Name: "CLICKHOUSE_USER", Value: clickhouseAuthUsername},
			{Name: "CLICKHOUSE_PASSWORD", ValueFrom: secretKeyRef(
				ClickHouseAuthSecretName(z.Name), clickhousePasswordKey)},
			{Name: "CLICKHOUSE_DEFAULT_USER_PASSWORD", ValueFrom: secretKeyRef(
				ClickHouseAuthSecretName(z.Name), clickhousePasswordKey)},
		},
		Ports: []corev1.ContainerPort{
			{Name: portNameClickhouseSQL, ContainerPort: clickhouseHTTPPort},
			{Name: portNameClickhouseNAT, ContainerPort: clickhouseNativePort},
		},
		// /ping answers "Ok." on the HTTP interface once the server
		// accepts queries.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/ping", Port: intstr.FromInt32(clickhouseHTTPPort)},
			},
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/ping", Port: intstr.FromInt32(clickhouseHTTPPort)},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       20,
		},
		Resources: managed.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: clickhouseDataVolume, MountPath: "/var/lib/clickhouse"},
		},
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClickHouseName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: ClickHouseName(z.Name),
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentClickHouse)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: clickhouseDataVolume},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: storage,
						},
					},
					StorageClassName: strPtrIfSet(managed.StorageClassName),
				},
			}},
		},
	}
}

// ClickHouseService builds the ClusterIP Service fronting the managed
// backend: the HTTP interface (8123) is what the provider JSON URL
// points at, the native port (9000) is exposed for ad-hoc
// clickhouse-client access from inside the cluster.
func ClickHouseService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClickHouseName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClickHouse),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(z.Name, ComponentClickHouse),
			Ports: []corev1.ServicePort{
				{Name: portNameClickhouseSQL, Port: clickhouseHTTPPort, TargetPort: intstr.FromInt32(clickhouseHTTPPort)},
				{Name: portNameClickhouseNAT, Port: clickhouseNativePort, TargetPort: intstr.FromInt32(clickhouseNativePort)},
			},
		},
	}
}

// ClickHouseAuthSecret builds the operator-generated credentials Secret
// for the managed backend. The controller creates it once with a random
// password and never rewrites it (generation is get-or-create only).
func ClickHouseAuthSecret(z *zabbixv1alpha1.Zabbix, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClickHouseAuthSecretName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClickHouse),
		},
		Data: map[string][]byte{
			clickhouseUsernameKey: []byte(clickhouseAuthUsername),
			clickhousePasswordKey: []byte(password),
		},
	}
}

// GenerateClickHousePassword returns a random password (32 hex chars).
// ClickHouse accepts it as-is.
func GenerateClickHousePassword() (string, error) {
	raw, err := randomBytes(16)
	if err != nil {
		return "", fmt.Errorf("generating clickhouse password: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) ([]byte, error) {
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return out, nil
}

// strPtrIfSet returns a pointer to s, or nil when empty (maps an
// optional string to an omitted spec field).
func strPtrIfSet(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// secretKeyRef builds a Secret env source.
func secretKeyRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  key,
		},
	}
}
