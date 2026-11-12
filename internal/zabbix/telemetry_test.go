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
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Shared telemetry-test literals — constants so repeated strings satisfy
// goconst and intent stays explicit.
const (
	testTelemetryEnv0    = "ZBX_TELEMETRYPROVIDER_0"
	testHistoryEnv0      = "ZBX_HISTORYPROVIDER_0"
	testTypeDbl          = "dbl"
	testTypeUint         = "uint"
	testTLSSecretName    = "clickhouse-tls"
	testCredsSecretName  = "clickhouse-creds"
	testCredsMountSuffix = "/etc/zabbix/secrets/clickhouse/"
)

// testTelemetryZabbix returns a Zabbix resource with ClickHouse telemetry
// storage configured on version.
func testTelemetryZabbix(version string) *zabbixv1alpha1.Zabbix {
	z := testZabbix()
	z.Spec.Version = version
	z.Spec.TelemetryStorage = &zabbixv1alpha1.TelemetryStorageSpec{
		ClickHouse: zabbixv1alpha1.ClickHouseSpec{
			Endpoint: "http://clickhouse.monitoring.svc:8123",
			CredentialsSecretRef: zabbixv1alpha1.ClickHouseCredentials{
				SecretName: testCredsSecretName,
			},
		},
	}
	return z
}

func TestSupportsTelemetry(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"8.0", true},
		{"8.0.2", true},
		{"8.2", true},
		{"7.0", false},
		{"7.4", false},
		{"6.0", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		if got := supportsTelemetry(tc.version); got != tc.want {
			t.Errorf("supportsTelemetry(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestMajorVersion(t *testing.T) {
	if major, ok := majorVersion("8.0"); !ok || major != 8 {
		t.Errorf("majorVersion(8.0) = %d, %v", major, ok)
	}
	if _, ok := majorVersion("nonsense"); ok {
		t.Error("majorVersion(nonsense) must report not-ok")
	}
}

func TestServerTelemetryEnvs(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	envs := ServerTelemetryEnvs(z)

	var history, telemetry map[string]any
	for _, e := range envs {
		if e.Name == testHistoryEnv0 {
			if err := json.Unmarshal([]byte(e.Value), &history); err != nil {
				t.Fatalf("history provider JSON: %v", err)
			}
		}
		if e.Name == testTelemetryEnv0 {
			if err := json.Unmarshal([]byte(e.Value), &telemetry); err != nil {
				t.Fatalf("telemetry provider JSON: %v", err)
			}
		}
	}
	if history == nil || telemetry == nil {
		t.Fatalf("missing provider envs: %v", envs)
	}
	if history["provider"] != "clickhouse" || telemetry["provider"] != "clickhouse" {
		t.Errorf("provider = %v / %v", history["provider"], telemetry["provider"])
	}
	if history["url"] != "http://clickhouse.monitoring.svc:8123" {
		t.Errorf("history url = %v", history["url"])
	}
	if history["db"] != "zabbix" {
		t.Errorf("history db = %v, want default zabbix", history["db"])
	}
	// History carries all supported value types; telemetry has none.
	wantTypes := []any{testTypeUint, testTypeDbl, "str", "log", "text", "json"}
	if !reflect.DeepEqual(history["types"], wantTypes) {
		t.Errorf("history types = %v, want %v", history["types"], wantTypes)
	}
	if _, has := telemetry["types"]; has {
		t.Error("telemetry provider must not carry types")
	}
}

func TestServerTelemetryEnvsCredentialsAndSchema(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	envs := ServerTelemetryEnvs(z)

	// Credentials are delivered via Secret files, never values.
	for _, e := range envs {
		if e.Name == "ZBX_HISTORYPROVIDER_0_USERNAME" || e.Name == "ZBX_HISTORYPROVIDER_0_PASSWORD" {
			t.Errorf("%s must not be set (use the _FILE variants)", e.Name)
		}
	}
	if _, ok := envByName(envs, "ZBX_HISTORYPROVIDER_0_USERNAME_FILE"); !ok {
		t.Error("missing ZBX_HISTORYPROVIDER_0_USERNAME_FILE")
	}
	env, ok := envByName(envs, "ZBX_HISTORYPROVIDER_0_USERNAME_FILE")
	if ok && env.Value != testCredsMountSuffix+"username" {
		t.Errorf("username file = %q", env.Value)
	}
	if _, ok := envByName(envs, "ZBX_TELEMETRYPROVIDER_0_PASSWORD_FILE"); !ok {
		t.Error("missing ZBX_TELEMETRYPROVIDER_0_PASSWORD_FILE")
	}

	// Default: entrypoint creates the schema.
	if _, ok := envByName(envs, "ZBX_HISTORYPROVIDER_0_SCHEMA"); !ok {
		t.Error("missing ZBX_HISTORYPROVIDER_0_SCHEMA (default create)")
	}
	// Telemetry query pollers on by default when telemetry storage is set.
	env, ok = envByName(envs, "ZBX_STARTTELEMETRYQUERYPOLLERS")
	if !ok || env.Value != "1" {
		t.Errorf("ZBX_STARTTELEMETRYQUERYPOLLERS = %+v, want 1", env)
	}

	// createSchema=false suppresses the _SCHEMA envs.
	z.Spec.TelemetryStorage.ClickHouse.CreateSchema = new(false)
	envs = ServerTelemetryEnvs(z)
	for _, e := range envs {
		if e.Name == "ZBX_HISTORYPROVIDER_0_SCHEMA" || e.Name == "ZBX_TELEMETRYPROVIDER_0_SCHEMA" {
			t.Errorf("%s must be absent with createSchema=false", e.Name)
		}
	}
}

func TestTelemetryVersionGate(t *testing.T) {
	z := testTelemetryZabbix(testVersion70)
	if envs := ServerTelemetryEnvs(z); envs != nil {
		t.Errorf("7.0 server envs = %v, want nil", envs)
	}
	if envs := FrontendTelemetryEnvs(z); envs != nil {
		t.Errorf("7.0 frontend envs = %v, want nil", envs)
	}
	// No telemetryStorage at all: no-op everywhere.
	plain := testZabbix()
	if envs := ServerTelemetryEnvs(plain); envs != nil {
		t.Errorf("envs without telemetryStorage = %v, want nil", envs)
	}
}

func TestTelemetryValueTypesAndDatabaseOverride(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	z.Spec.TelemetryStorage.ClickHouse.ValueTypes = []string{testTypeUint, testTypeDbl}
	z.Spec.TelemetryStorage.ClickHouse.Database = "history"

	envs := ServerTelemetryEnvs(z)
	env, _ := envByName(envs, testHistoryEnv0)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(env.Value), &cfg); err != nil {
		t.Fatalf("history provider JSON: %v", err)
	}
	if !reflect.DeepEqual(cfg["types"], []any{testTypeUint, testTypeDbl}) {
		t.Errorf("types = %v, want [uint dbl]", cfg["types"])
	}
	if cfg["db"] != "history" {
		t.Errorf("db = %v, want history", cfg["db"])
	}
}

func TestTelemetryCredentialKeyDefaults(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	creds := &z.Spec.TelemetryStorage.ClickHouse.CredentialsSecretRef
	creds.UsernameKey = "user"
	creds.PasswordKey = "pass"

	envs := ServerTelemetryEnvs(z)
	env, _ := envByName(envs, "ZBX_HISTORYPROVIDER_0_USERNAME_FILE")
	if env.Value != testCredsMountSuffix+"user" {
		t.Errorf("username file = %q", env.Value)
	}
	env, _ = envByName(envs, "ZBX_HISTORYPROVIDER_0_PASSWORD_FILE")
	if env.Value != testCredsMountSuffix+"pass" {
		t.Errorf("password file = %q", env.Value)
	}
}

func TestClickHouseTLSFileEnvs(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	z.Spec.TelemetryStorage.ClickHouse.TLS = &zabbixv1alpha1.ClickHouseTLS{
		SecretName: testTLSSecretName,
	}

	// Without a CA key: cert + key only (no peer verification).
	envs := ClickHouseTLSFileEnvs(z, testHistoryEnv0)
	if len(envs) != 2 {
		t.Fatalf("TLS envs = %+v, want cert+key", envs)
	}
	if envs[0].Name != "ZBX_HISTORYPROVIDER_0_SSL_CERT_FILE" ||
		envs[0].Value != "/etc/zabbix/secrets/clickhouse-tls/tls.crt" {
		t.Errorf("cert env = %+v", envs[0])
	}
	if envs[1].Name != "ZBX_HISTORYPROVIDER_0_SSL_KEY_FILE" ||
		envs[1].Value != "/etc/zabbix/secrets/clickhouse-tls/tls.key" {
		t.Errorf("key env = %+v", envs[1])
	}

	// With a CA key: CA first (turns on peer verification).
	z.Spec.TelemetryStorage.ClickHouse.TLS.CAKey = "ca.crt"
	envs = ClickHouseTLSFileEnvs(z, testHistoryEnv0)
	if len(envs) != 3 || envs[0].Name != "ZBX_HISTORYPROVIDER_0_SSL_CA_FILE" {
		t.Fatalf("TLS envs with CA = %+v", envs)
	}
	if envs[0].Value != "/etc/zabbix/secrets/clickhouse-tls/ca.crt" {
		t.Errorf("CA file = %q", envs[0].Value)
	}
}

func TestTelemetrySecretVolumesAndMounts(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)

	volumes := TelemetrySecretVolumes(z)
	if len(volumes) != 1 || volumes[0].Name != volumeClickhouseCredentials {
		t.Fatalf("volumes = %+v", volumes)
	}
	if volumes[0].Secret.SecretName != testCredsSecretName {
		t.Errorf("secretName = %q", volumes[0].Secret.SecretName)
	}
	mounts := TelemetryVolumeMounts(z)
	if len(mounts) != 1 || mounts[0].MountPath != clickhouseCredentialsMount {
		t.Fatalf("mounts = %+v", mounts)
	}

	// TLS adds a second volume/mount.
	z.Spec.TelemetryStorage.ClickHouse.TLS = &zabbixv1alpha1.ClickHouseTLS{SecretName: testTLSSecretName}
	volumes = TelemetrySecretVolumes(z)
	if len(volumes) != 2 || volumes[1].Secret.SecretName != "clickhouse-tls" {
		t.Errorf("volumes with TLS = %+v", volumes)
	}
	mounts = TelemetryVolumeMounts(z)
	if len(mounts) != 2 || mounts[1].MountPath != clickhouseTLSMount {
		t.Errorf("mounts with TLS = %+v", mounts)
	}
}

func TestProxyTelemetryEnvs(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80

	envs := ProxyTelemetryEnvs(pg, z)
	if _, ok := envByName(envs, testTelemetryEnv0); !ok {
		t.Fatalf("missing ZBX_TELEMETRYPROVIDER_0 in %v", envs)
	}
	// Proxies get telemetry only — never history (SQL backend only).
	if _, ok := envByName(envs, testHistoryEnv0); ok {
		t.Error("proxy must not receive a history provider (not supported)")
	}
	if _, ok := envByName(envs, "ZBX_TELEMETRYPROVIDER_0_PASSWORD_FILE"); !ok {
		t.Error("missing ZBX_TELEMETRYPROVIDER_0_PASSWORD_FILE")
	}
}

func TestProxyTelemetryEnvsVersionGate(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	pg := testProxyGroup() // 7.0 proxy
	if envs := ProxyTelemetryEnvs(pg, z); envs != nil {
		t.Errorf("7.0 proxy envs = %v, want nil", envs)
	}
}

func TestProxyAPMEnvs(t *testing.T) {
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{}

	envs := ProxyAPMEnvs(pg)
	env, ok := envByName(envs, "ZBX_STARTAPMCOLLECTOR")
	if !ok || env.Value != "1" {
		t.Fatalf("ZBX_STARTAPMCOLLECTOR = %+v", env)
	}
	env, _ = envByName(envs, "ZBX_APMLISTENPORT")
	if env.Value != "4317" {
		t.Errorf("ZBX_APMLISTENPORT = %q, want 4317", env.Value)
	}
	for _, name := range []string{"ZBX_APMTLSCAFILE", "ZBX_APMTLSCERTFILE", "ZBX_APMTLSKEYFILE"} {
		if _, ok := envByName(envs, name); ok {
			t.Errorf("%s must be absent without tlsSecretRef", name)
		}
	}

	// Custom port + TLS.
	pg.Spec.APM.ListenPort = ptr.To[int32](14317)
	pg.Spec.APM.TLSSecretRef = &zabbixv1alpha1.ProxyAPMTLS{SecretName: volumeAPMTLS}
	envs = ProxyAPMEnvs(pg)
	env, _ = envByName(envs, "ZBX_APMLISTENPORT")
	if env.Value != "14317" {
		t.Errorf("custom ZBX_APMLISTENPORT = %q", env.Value)
	}
	env, _ = envByName(envs, "ZBX_APMTLSCAFILE")
	if env.Value != "/etc/zabbix/secrets/apm-tls/ca.crt" {
		t.Errorf("ZBX_APMTLSCAFILE = %q", env.Value)
	}
}

func TestProxyAPMDisabled(t *testing.T) {
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80
	if envs := ProxyAPMEnvs(pg); envs != nil {
		t.Errorf("APM envs without spec.apm = %v, want nil", envs)
	}
	if got := APMListenPortIfEnabled(pg); got != 0 {
		t.Errorf("APMListenPortIfEnabled = %d, want 0", got)
	}
	// APM set but pre-8.0 proxy: still disabled.
	pg.Spec.Version = testVersion70
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{}
	if got := APMListenPortIfEnabled(pg); got != 0 {
		t.Errorf("7.0 APMListenPortIfEnabled = %d, want 0", got)
	}
}

func TestAPMSecretVolumesAndMounts(t *testing.T) {
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{
		TLSSecretRef: &zabbixv1alpha1.ProxyAPMTLS{SecretName: volumeAPMTLS},
	}

	volumes := APMSecretVolumes(pg)
	if len(volumes) != 1 || volumes[0].Name != volumeAPMTLS ||
		volumes[0].Secret.SecretName != volumeAPMTLS {
		t.Fatalf("volumes = %+v", volumes)
	}
	mounts := APMVolumeMounts(pg)
	if len(mounts) != 1 || mounts[0].MountPath != apmTLSMount {
		t.Fatalf("mounts = %+v", mounts)
	}

	// Disabled: nothing mounted.
	plain := testProxyGroup()
	plain.Spec.Version = testVersion80
	if vols := APMSecretVolumes(plain); vols != nil {
		t.Errorf("volumes without APM = %v, want nil", vols)
	}
}

func TestProxyAPMService(t *testing.T) {
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{}

	svc := ProxyAPMService(pg)
	if svc.Name != testProxyName+"-apm" || svc.Namespace != testPgNamespace {
		t.Errorf("service = %s/%s", svc.Namespace, svc.Name)
	}
	if svc.Spec.Type != "ClusterIP" {
		t.Errorf("type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	port := svc.Spec.Ports[0]
	if port.Name != "otlp-grpc" || port.Port != 4317 ||
		port.TargetPort.IntValue() != 4317 {
		t.Errorf("port = %+v", port)
	}
	if svc.Spec.Selector[ComponentLabel] != ComponentProxy {
		t.Errorf("selector = %v, want the proxy component selector", svc.Spec.Selector)
	}
}

// --- integration: the builders actually wire the env/volumes ---

func TestServerStatefulSetTelemetryWiring(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	sts := ServerStatefulSet(z, testDB())

	server := sts.Spec.Template.Spec.Containers[0]
	if _, ok := envByName(server.Env, testHistoryEnv0); !ok {
		t.Error("server container missing ZBX_HISTORYPROVIDER_0")
	}
	if len(server.VolumeMounts) == 0 ||
		server.VolumeMounts[0].MountPath != clickhouseCredentialsMount {
		t.Errorf("server volumeMounts = %+v", server.VolumeMounts)
	}
	podVolumes := sts.Spec.Template.Spec.Volumes
	if len(podVolumes) == 0 || podVolumes[0].Name != volumeClickhouseCredentials {
		t.Errorf("server pod volumes = %+v", podVolumes)
	}
}

func TestFrontendDeploymentTelemetryWiring(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	dep := FrontendDeployment(z, testDB())

	fe := dep.Spec.Template.Spec.Containers[0]
	if _, ok := envByName(fe.Env, testTelemetryEnv0); !ok {
		t.Error("frontend container missing ZBX_TELEMETRYPROVIDER_0")
	}
	if len(fe.VolumeMounts) == 0 ||
		fe.VolumeMounts[0].MountPath != clickhouseCredentialsMount {
		t.Errorf("frontend volumeMounts = %+v", fe.VolumeMounts)
	}
}

func TestServerStatefulSetNoTelemetryByDefault(t *testing.T) {
	z := testZabbix()
	sts := ServerStatefulSet(z, testDB())

	server := sts.Spec.Template.Spec.Containers[0]
	for _, e := range server.Env {
		if e.Name == testHistoryEnv0 || e.Name == testTelemetryEnv0 {
			t.Errorf("unexpected %s without telemetryStorage", e.Name)
		}
	}
	if len(sts.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("volumes = %+v, want none", sts.Spec.Template.Spec.Volumes)
	}
}

func TestProxyStatefulSetAPMWiring(t *testing.T) {
	z := testTelemetryZabbix(testVersion80)
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{}

	sts := ProxyStatefulSet(pg, 2, z)
	proxy := sts.Spec.Template.Spec.Containers[0]
	if _, ok := envByName(proxy.Env, "ZBX_STARTAPMCOLLECTOR"); !ok {
		t.Error("proxy container missing ZBX_STARTAPMCOLLECTOR")
	}
	if _, ok := envByName(proxy.Env, testTelemetryEnv0); !ok {
		t.Error("proxy container missing ZBX_TELEMETRYPROVIDER_0")
	}
	foundAPMPort := false
	for _, p := range proxy.Ports {
		if p.Name == "otlp-grpc" && p.ContainerPort == 4317 {
			foundAPMPort = true
		}
	}
	if !foundAPMPort {
		t.Errorf("ports = %+v, want an otlp-grpc 4317 port", proxy.Ports)
	}
}

func TestProxyStatefulSetNoAPMByDefault(t *testing.T) {
	z := testServerRefResource()
	pg := testProxyGroup()

	sts := ProxyStatefulSet(pg, 1, z)
	proxy := sts.Spec.Template.Spec.Containers[0]
	for _, e := range proxy.Env {
		if e.Name == "ZBX_STARTAPMCOLLECTOR" || e.Name == testTelemetryEnv0 {
			t.Errorf("unexpected %s without apm/telemetryStorage", e.Name)
		}
	}
	if len(proxy.Ports) != 1 || proxy.Ports[0].Name != portNameProxyTrapper {
		t.Errorf("ports = %+v, want only the trapper port", proxy.Ports)
	}
}
