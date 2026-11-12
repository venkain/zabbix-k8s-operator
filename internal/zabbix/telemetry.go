/*
Copyright 2026 Venkain.

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
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Telemetry storage (ClickHouse history provider + APM telemetry) is a
// Zabbix 8.0 feature: the image entrypoints expose the provider
// configuration as indexed ZBX_HISTORYPROVIDER_<N> / ZBX_TELEMETRYPROVIDER_<N>
// JSON environments since 8.0.0 (verified against zabbix-docker 8.0:
// templates/entrypoints/internal/storageprovider + the Docker Hub env
// docs). Credentials and TLS material are delivered through Secret-backed
// files (_FILE / SSL_* variables), never as plain env values.
const (
	envHistoryProviderPrefix   = "ZBX_HISTORYPROVIDER"
	envTelemetryProviderPrefix = "ZBX_TELEMETRYPROVIDER"
	envStartTelemetryQuery     = "ZBX_STARTTELEMETRYQUERYPOLLERS"
	envStartAPMCollector       = "ZBX_STARTAPMCOLLECTOR"
	envAPMListenPort           = "ZBX_APMLISTENPORT"
	envAPMTLSCAFile            = "ZBX_APMTLSCAFILE"
	envAPMTLSCertFile          = "ZBX_APMTLSCERTFILE"
	envAPMTLSKeyFile           = "ZBX_APMTLSKEYFILE"

	clickhouseCredentialsMount = "/etc/zabbix/secrets/clickhouse"
	clickhouseTLSMount         = "/etc/zabbix/secrets/clickhouse-tls"
	apmTLSMount                = "/etc/zabbix/secrets/apm-tls"

	volumeClickhouseCredentials = "clickhouse-credentials"
	volumeClickhouseTLS         = "clickhouse-tls"
	volumeAPMTLS                = "apm-tls"

	// otlpPortName labels the OTLP/gRPC listener port on the proxy
	// containers and the group APM Service.
	otlpPortName = "otlp-grpc"

	defaultAPMPort int32 = 4317

	// schemaCreateJSON is the official _SCHEMA payload telling the image
	// entrypoint to create the provider tables on startup.
	schemaCreateJSON = `{"create":true}`
)

// supportsTelemetry reports whether the Zabbix release exposes the
// provider/APM entrypoint interface (8.0+).
func supportsTelemetry(version string) bool {
	major, ok := majorVersion(version)
	return ok && major >= 8
}

// clickhouseDatabase returns the configured database name ("zabbix" by
// default, matching the official setup guide).
func clickhouseDatabase(z *zabbixv1alpha1.Zabbix) string {
	if db := z.Spec.TelemetryStorage.ClickHouse.Database; db != "" {
		return db
	}
	return "zabbix"
}

// jsonValue quotes a string as a JSON string literal (also correct for
// non-ASCII UTF-8, which JSON allows raw).
func jsonValue(s string) string {
	return strconv.Quote(s)
}

// clickhouseProviderJSON renders the shared provider JSON object used by
// both the history and the telemetry provider environments. valueTypes
// is included only for the history provider (telemetry has no types).
func clickhouseProviderJSON(z *zabbixv1alpha1.Zabbix, valueTypes []string) string {
	var b strings.Builder
	b.WriteString(`{"provider":"clickhouse","url":`)
	b.WriteString(jsonValue(ClickHouseEndpoint(z)))
	b.WriteString(`,"db":`)
	b.WriteString(jsonValue(clickhouseDatabase(z)))
	if len(valueTypes) > 0 {
		b.WriteString(`,"types":[`)
		for i, t := range valueTypes {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(jsonValue(t))
		}
		b.WriteString("]")
	}
	b.WriteString("}")
	return b.String()
}

// historyValueTypes resolves the effective value-type list: the spec
// selection or every type ClickHouse supports (official docs table:
// uint, dbl, str, log, text, json; binary stays SQL-only).
func historyValueTypes(z *zabbixv1alpha1.Zabbix) []string {
	if types := z.Spec.TelemetryStorage.ClickHouse.ValueTypes; len(types) > 0 {
		return types
	}
	return []string{"uint", "dbl", "str", "log", "text", "json"}
}

// schemaCreateEnabled reports whether the entrypoint should create the
// provider tables (default true, mirroring the official _SCHEMA default
// for a fresh ClickHouse; set false for a migrated database).
func schemaCreateEnabled(z *zabbixv1alpha1.Zabbix) bool {
	return z.Spec.TelemetryStorage.ClickHouse.CreateSchema == nil ||
		*z.Spec.TelemetryStorage.ClickHouse.CreateSchema
}

// clickhouseCredentialsFileEnv builds the _FILE credential environments
// for one provider: the entrypoint reads username/password from files
// instead of process environment values. Paths are absolute inside the
// read-only Secret mount.
func clickhouseCredentialsFileEnv(prefix, usernameFile, passwordFile string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: prefix + "_USERNAME_FILE", Value: usernameFile},
		{Name: prefix + "_PASSWORD_FILE", Value: passwordFile},
	}
}

// ServerTelemetryEnvs returns the server container environments that
// wire ClickHouse history storage and the telemetry data source:
// ZBX_HISTORYPROVIDER_0 (history), ZBX_TELEMETRYPROVIDER_0 (telemetry
// query items) and the telemetry query pollers.
func ServerTelemetryEnvs(z *zabbixv1alpha1.Zabbix) []corev1.EnvVar {
	if z.Spec.TelemetryStorage == nil || !supportsTelemetry(z.Spec.Version) {
		return nil
	}
	creds := ClickHouseCreds(z)
	usernameFile := creds.UsernameFile
	passwordFile := creds.PasswordFile

	historyPrefix := envHistoryProviderPrefix + "_0"
	telemetryPrefix := envTelemetryProviderPrefix + "_0"

	env := []corev1.EnvVar{
		{Name: historyPrefix, Value: clickhouseProviderJSON(z, historyValueTypes(z))},
		{Name: telemetryPrefix, Value: clickhouseProviderJSON(z, nil)},
		// Telemetry query items need server-side pollers; the default
		// (0) means telemetry data is never queried.
		{Name: envStartTelemetryQuery, Value: "1"},
	}
	env = append(env, clickhouseCredentialsFileEnv(historyPrefix, usernameFile, passwordFile)...)
	env = append(env, clickhouseCredentialsFileEnv(telemetryPrefix, usernameFile, passwordFile)...)
	env = append(env, ClickHouseTLSFileEnvs(z, historyPrefix)...)
	env = append(env, ClickHouseTLSFileEnvs(z, telemetryPrefix)...)

	if schemaCreateEnabled(z) {
		env = append(env,
			corev1.EnvVar{Name: historyPrefix + "_SCHEMA", Value: schemaCreateJSON},
			corev1.EnvVar{Name: telemetryPrefix + "_SCHEMA", Value: schemaCreateJSON},
		)
	}
	return env
}

// FrontendTelemetryEnvs wires the frontend to the same ClickHouse
// providers (history reads + the APM menu's telemetry data source).
func FrontendTelemetryEnvs(z *zabbixv1alpha1.Zabbix) []corev1.EnvVar {
	if z.Spec.TelemetryStorage == nil || !supportsTelemetry(z.Spec.Version) {
		return nil
	}
	creds := ClickHouseCreds(z)
	usernameFile := creds.UsernameFile
	passwordFile := creds.PasswordFile

	historyPrefix := envHistoryProviderPrefix + "_0"
	telemetryPrefix := envTelemetryProviderPrefix + "_0"

	env := make([]corev1.EnvVar, 0, 8)
	env = append(env,
		corev1.EnvVar{Name: historyPrefix, Value: clickhouseProviderJSON(z, historyValueTypes(z))},
		corev1.EnvVar{Name: telemetryPrefix, Value: clickhouseProviderJSON(z, nil)},
	)
	env = append(env, clickhouseCredentialsFileEnv(historyPrefix, usernameFile, passwordFile)...)
	env = append(env, clickhouseCredentialsFileEnv(telemetryPrefix, usernameFile, passwordFile)...)
	env = append(env, ClickHouseTLSFileEnvs(z, historyPrefix)...)
	env = append(env, ClickHouseTLSFileEnvs(z, telemetryPrefix)...)
	return env
}

// ProxyTelemetryEnvs wires a proxy-group member to the telemetry
// provider of the referenced Zabbix resource: APM data collected by the
// members must land in the same ClickHouse the server reads from.
// Gated on the proxy's own version (the 8.0 proxy image understands the
// variable; the provider JSON itself is version-independent).
func ProxyTelemetryEnvs(pg *zabbixv1alpha1.ZabbixProxyGroup, z *zabbixv1alpha1.Zabbix) []corev1.EnvVar {
	if z.Spec.TelemetryStorage == nil || !supportsTelemetry(pg.Spec.Version) {
		return nil
	}
	creds := ClickHouseCreds(z)
	usernameFile := creds.UsernameFile
	passwordFile := creds.PasswordFile

	telemetryPrefix := envTelemetryProviderPrefix + "_0"
	env := []corev1.EnvVar{
		{Name: telemetryPrefix, Value: clickhouseProviderJSON(z, nil)},
	}
	env = append(env, clickhouseCredentialsFileEnv(telemetryPrefix, usernameFile, passwordFile)...)
	env = append(env, ClickHouseTLSFileEnvs(z, telemetryPrefix)...)
	if schemaCreateEnabled(z) {
		env = append(env, corev1.EnvVar{
			Name: telemetryPrefix + "_SCHEMA", Value: schemaCreateJSON})
	}
	return env
}

// ProxyAPMEnvs enables the OTLP/gRPC APM collector on the member and
// wires the listener TLS material when configured.
func ProxyAPMEnvs(pg *zabbixv1alpha1.ZabbixProxyGroup) []corev1.EnvVar {
	if pg.Spec.APM == nil || !supportsTelemetry(pg.Spec.Version) {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: envStartAPMCollector, Value: "1"},
		{Name: envAPMListenPort, Value: fmt.Sprintf("%d", APMListenPort(pg))},
	}
	if tls := pg.Spec.APM.TLSSecretRef; tls != nil {
		env = append(env,
			corev1.EnvVar{Name: envAPMTLSCAFile, Value: secretMountFile(apmTLSMount, tls.CAKey, "ca.crt")},
			corev1.EnvVar{Name: envAPMTLSCertFile, Value: secretMountFile(apmTLSMount, tls.CertKey, "tls.crt")},
			corev1.EnvVar{Name: envAPMTLSKeyFile, Value: secretMountFile(apmTLSMount, tls.KeyKey, "tls.key")},
		)
	}
	return env
}

// APMListenPort returns the configured OTLP/gRPC listener port (4317 by
// default, the OpenTelemetry receiver default).
func APMListenPort(pg *zabbixv1alpha1.ZabbixProxyGroup) int32 {
	if pg.Spec.APM != nil && pg.Spec.APM.ListenPort != nil {
		return *pg.Spec.APM.ListenPort
	}
	return defaultAPMPort
}

// APMListenPortIfEnabled returns the OTLP/gRPC listener port when the
// APM collector is enabled (Zabbix 8.0+), or 0 when disabled — used by
// callers that add the container port conditionally.
func APMListenPortIfEnabled(pg *zabbixv1alpha1.ZabbixProxyGroup) int32 {
	if pg.Spec.APM == nil || !supportsTelemetry(pg.Spec.Version) {
		return 0
	}
	return APMListenPort(pg)
}

// TelemetrySecretVolumes returns the read-only Secret volumes backing
// the ClickHouse credential/TLS file mounts for the Zabbix workloads.
func TelemetrySecretVolumes(z *zabbixv1alpha1.Zabbix) []corev1.Volume {
	if z.Spec.TelemetryStorage == nil {
		return nil
	}
	volumes := []corev1.Volume{{
		Name: volumeClickhouseCredentials,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: ClickHouseCreds(z).SecretName,
			},
		},
	}}
	if z.Spec.TelemetryStorage.ClickHouse.TLS != nil {
		volumes = append(volumes, corev1.Volume{
			Name: volumeClickhouseTLS,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: z.Spec.TelemetryStorage.ClickHouse.TLS.SecretName,
				},
			},
		})
	}
	return volumes
}

// TelemetryVolumeMounts mounts the ClickHouse Secret material into the
// server/frontend containers.
func TelemetryVolumeMounts(z *zabbixv1alpha1.Zabbix) []corev1.VolumeMount {
	if z.Spec.TelemetryStorage == nil {
		return nil
	}
	mounts := []corev1.VolumeMount{{
		Name:      volumeClickhouseCredentials,
		ReadOnly:  true,
		MountPath: clickhouseCredentialsMount,
	}}
	if z.Spec.TelemetryStorage.ClickHouse.TLS != nil {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeClickhouseTLS,
			ReadOnly:  true,
			MountPath: clickhouseTLSMount,
		})
	}
	return mounts
}

// ClickHouseTLSFileEnvs returns the provider TLS file environments when
// ClickHouse TLS is configured (SSL_CA / SSL_CERT / SSL_KEY accept
// absolute paths in their _FILE variants). A CA certificate turns on
// peer verification; it is optional.
func ClickHouseTLSFileEnvs(z *zabbixv1alpha1.Zabbix, provider string) []corev1.EnvVar {
	if z.Spec.TelemetryStorage == nil || z.Spec.TelemetryStorage.ClickHouse.TLS == nil {
		return nil
	}
	tls := z.Spec.TelemetryStorage.ClickHouse.TLS
	cert := secretMountFile(clickhouseTLSMount, tls.CertKey, "tls.crt")
	key := secretMountFile(clickhouseTLSMount, tls.KeyKey, "tls.key")
	env := []corev1.EnvVar{
		{Name: provider + "_SSL_CERT_FILE", Value: cert},
		{Name: provider + "_SSL_KEY_FILE", Value: key},
	}
	if tls.CAKey != "" {
		env = append([]corev1.EnvVar{{
			Name:  provider + "_SSL_CA_FILE",
			Value: secretMountFile(clickhouseTLSMount, tls.CAKey, "ca.crt"),
		}}, env...)
	}
	return env
}

// APMSecretVolumes returns the read-only Secret volume backing the APM
// listener TLS mount.
func APMSecretVolumes(pg *zabbixv1alpha1.ZabbixProxyGroup) []corev1.Volume {
	if pg.Spec.APM == nil || pg.Spec.APM.TLSSecretRef == nil {
		return nil
	}
	return []corev1.Volume{{
		Name: volumeAPMTLS,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: pg.Spec.APM.TLSSecretRef.SecretName,
			},
		},
	}}
}

// APMVolumeMounts mounts the listener TLS Secret into the proxy
// containers.
func APMVolumeMounts(pg *zabbixv1alpha1.ZabbixProxyGroup) []corev1.VolumeMount {
	if pg.Spec.APM == nil || pg.Spec.APM.TLSSecretRef == nil {
		return nil
	}
	return []corev1.VolumeMount{{
		Name:      volumeAPMTLS,
		ReadOnly:  true,
		MountPath: apmTLSMount,
	}}
}

// ProxyAPMService builds the Service that load-balances OTLP/gRPC
// submissions across the group members (applications point their OTLP
// exporters at this address).
func ProxyAPMService(pg *zabbixv1alpha1.ZabbixProxyGroup) *corev1.Service {
	port := APMListenPort(pg)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyName(pg.Name) + "-apm",
			Namespace: pg.Namespace,
			Labels:    commonLabels(pg.Name, ComponentProxy),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(pg.Name, ComponentProxy),
			Ports: []corev1.ServicePort{{
				Name:       otlpPortName,
				Port:       port,
				TargetPort: intstr.FromInt32(port),
			}},
		},
	}
}

// secretMountFile resolves a Secret key inside a mounted secret volume,
// applying the documented key defaults.
func secretMountFile(mount, key, defaultKey string) string {
	if key == "" {
		key = defaultKey
	}
	return mount + "/" + key
}
