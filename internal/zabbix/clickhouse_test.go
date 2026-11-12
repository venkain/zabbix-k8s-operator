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
	"encoding/hex"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Shared clickhouse-test literals — constants so repeated strings
// satisfy goconst and intent stays explicit.
const (
	testManagedEndpoint  = "http://test-zabbix-clickhouse.test-ns.svc:8123"
	testExternalEndpoint = "http://clickhouse.monitoring.svc:8123"
	testCHName           = "test-zabbix-clickhouse"
	testCHAuthSecret     = "test-zabbix-clickhouse-auth"
	testCHDBName         = "zbx_hist"
)

// testManagedTelemetryZabbix returns a Zabbix resource with managed
// ClickHouse enabled.
func testManagedTelemetryZabbix(version string) *zabbixv1alpha1.Zabbix {
	z := testZabbix()
	z.Spec.Version = version
	z.Spec.TelemetryStorage = &zabbixv1alpha1.TelemetryStorageSpec{
		ClickHouse: zabbixv1alpha1.ClickHouseSpec{
			Managed: &zabbixv1alpha1.ClickHouseManagedSpec{},
		},
	}
	return z
}

func TestManagedClickHouseEnabled(t *testing.T) {
	if !ManagedClickHouseEnabled(testManagedTelemetryZabbix(testVersion80)) {
		t.Error("managed mode on 8.0 must be enabled")
	}
	if ManagedClickHouseEnabled(testManagedTelemetryZabbix(testVersion70)) {
		t.Error("managed mode on 7.0 must be gated off (no provider interface)")
	}
	z := testZabbix() // no telemetryStorage at all
	if ManagedClickHouseEnabled(z) {
		t.Error("managed mode without telemetryStorage must be off")
	}
}

func TestClickHouseEndpoint(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	want := testManagedEndpoint
	if got := ClickHouseEndpoint(z); got != want {
		t.Errorf("managed endpoint = %q, want %q", got, want)
	}

	// External mode: the spec endpoint wins.
	ext := testTelemetryZabbix(testVersion80)
	if got := ClickHouseEndpoint(ext); got != testExternalEndpoint {
		t.Errorf("external endpoint = %q", got)
	}
}

func TestClickHouseCreds(t *testing.T) {
	// Managed mode: the operator-generated Secret with fixed keys.
	creds := ClickHouseCreds(testManagedTelemetryZabbix(testVersion80))
	if creds.SecretName != testCHAuthSecret {
		t.Errorf("managed secret = %q", creds.SecretName)
	}
	if creds.UsernameFile != clickhouseCredentialsMount+"/username" ||
		creds.PasswordFile != clickhouseCredentialsMount+"/password" {
		t.Errorf("managed files = %q / %q", creds.UsernameFile, creds.PasswordFile)
	}

	// External mode: the referenced Secret with the configured keys.
	ext := testTelemetryZabbix(testVersion80)
	creds = ClickHouseCreds(ext)
	if creds.SecretName != testCredsSecretName {
		t.Errorf("external secret = %q", creds.SecretName)
	}
}

func TestServerTelemetryEnvsManaged(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	envs := ServerTelemetryEnvs(z)

	env, ok := envByName(envs, testHistoryEnv0)
	if !ok {
		t.Fatalf("missing history provider env in %v", envs)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(env.Value), &cfg); err != nil {
		t.Fatalf("history provider JSON: %v", err)
	}
	if cfg["url"] != testManagedEndpoint {
		t.Errorf("managed history url = %v", cfg["url"])
	}
	env, _ = envByName(envs, testHistoryEnv0+"_USERNAME_FILE")
	if env.Value != "/etc/zabbix/secrets/clickhouse/username" {
		t.Errorf("managed username file = %q", env.Value)
	}

	// The managed backend mounts the operator-generated auth Secret.
	volumes := TelemetrySecretVolumes(z)
	if len(volumes) != 1 || volumes[0].Secret.SecretName != testCHAuthSecret {
		t.Errorf("managed volumes = %+v", volumes)
	}
}

func TestClickHouseStatefulSet(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	sts := ClickHouseStatefulSet(z)

	if sts.Name != testCHName || sts.Namespace != testNamespace {
		t.Errorf("statefulset = %s/%s", sts.Namespace, sts.Name)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1 (single-node backend)", *sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != testCHName {
		t.Errorf("serviceName = %q", sts.Spec.ServiceName)
	}
	if sts.Spec.Selector.MatchLabels[ComponentLabel] != ComponentClickHouse {
		t.Errorf("selector = %v", sts.Spec.Selector)
	}

	c := sts.Spec.Template.Spec.Containers[0]
	if c.Image != "clickhouse/clickhouse-server:26.8" {
		t.Errorf("image = %q", c.Image)
	}
	if env, ok := envByName(c.Env, "CLICKHOUSE_DB"); !ok || env.Value != "zabbix" {
		t.Errorf("CLICKHOUSE_DB = %+v", env)
	}
	if env, ok := envByName(c.Env, "CLICKHOUSE_USER"); !ok || env.Value != clickhouseAuthUsername {
		t.Errorf("CLICKHOUSE_USER = %+v", env)
	}
	env, ok := envByName(c.Env, "CLICKHOUSE_PASSWORD")
	if !ok || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil ||
		env.ValueFrom.SecretKeyRef.Name != testCHAuthSecret ||
		env.ValueFrom.SecretKeyRef.Key != "password" {
		t.Errorf("CLICKHOUSE_PASSWORD = %+v, want a secret keyRef to the auth secret", env)
	}
	// The password must never be a plain env value.
	for _, e := range c.Env {
		if e.Name == "CLICKHOUSE_PASSWORD" && e.Value != "" {
			t.Error("CLICKHOUSE_PASSWORD must not carry a plain value")
		}
	}

	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports[portNameClickhouseSQL] != 8123 || ports[portNameClickhouseNAT] != 9000 {
		t.Errorf("ports = %v", ports)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil ||
		c.ReadinessProbe.HTTPGet.Path != "/ping" {
		t.Errorf("readiness probe = %+v", c.ReadinessProbe)
	}

	// Data PVC template.
	vcts := sts.Spec.VolumeClaimTemplates
	if len(vcts) != 1 || vcts[0].Name != clickhouseDataVolume {
		t.Fatalf("volumeClaimTemplates = %+v", vcts)
	}
	size, ok := vcts[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || size.String() != "10Gi" {
		t.Errorf("PVC size = %v, want default 10Gi", size)
	}

}

func TestClickHouseStatefulSetOverrides(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	z.Spec.TelemetryStorage.ClickHouse.Managed.Storage = resource.MustParse("100Gi")
	z.Spec.TelemetryStorage.ClickHouse.Managed.StorageClassName = "fast-ssd"
	z.Spec.TelemetryStorage.ClickHouse.Managed.Image = &zabbixv1alpha1.ImageSpec{
		Repository: "mirror.example.com/clickhouse-server", Tag: "26.9.5",
	}
	sts := ClickHouseStatefulSet(z)
	if sts.Spec.Template.Spec.Containers[0].Image != "mirror.example.com/clickhouse-server:26.9.5" {
		t.Errorf("overridden image = %q", sts.Spec.Template.Spec.Containers[0].Image)
	}
	vcts := sts.Spec.VolumeClaimTemplates
	if got := vcts[0].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "100Gi" {
		t.Errorf("overridden PVC size = %v", got)
	}
	if sc := vcts[0].Spec.StorageClassName; sc == nil || *sc != "fast-ssd" {
		t.Errorf("storageClassName = %v", sc)
	}
}

func TestClickHouseStatefulSetDatabaseOverride(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	z.Spec.TelemetryStorage.ClickHouse.Database = testCHDBName

	sts := ClickHouseStatefulSet(z)
	if env, ok := envByName(sts.Spec.Template.Spec.Containers[0].Env, "CLICKHOUSE_DB"); !ok || env.Value != testCHDBName {
		t.Errorf("CLICKHOUSE_DB = %+v, want the spec database", env)
	}
	// And the provider JSON must target the same database.
	envs := ServerTelemetryEnvs(z)
	env, _ := envByName(envs, testHistoryEnv0)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(env.Value), &cfg); err != nil {
		t.Fatalf("history provider JSON: %v", err)
	}
	if cfg["db"] != testCHDBName {
		t.Errorf("provider db = %v, want zbx_hist", cfg["db"])
	}
}

func TestClickHouseService(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	svc := ClickHouseService(z)

	if svc.Name != testCHName || svc.Namespace != testNamespace {
		t.Errorf("service = %s/%s", svc.Namespace, svc.Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("type = %q", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Name != portNameClickhouseSQL || svc.Spec.Ports[0].Port != 8123 {
		t.Errorf("http port = %+v", svc.Spec.Ports[0])
	}
	if svc.Spec.Ports[1].Name != "native" || svc.Spec.Ports[1].Port != 9000 {
		t.Errorf("native port = %+v", svc.Spec.Ports[1])
	}
	if svc.Spec.Selector[ComponentLabel] != ComponentClickHouse {
		t.Errorf("selector = %v", svc.Spec.Selector)
	}
}

func TestClickHouseAuthSecret(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	secret := ClickHouseAuthSecret(z, "s3cret")

	if secret.Name != testCHAuthSecret || secret.Namespace != testNamespace {
		t.Errorf("secret = %s/%s", secret.Namespace, secret.Name)
	}
	if string(secret.Data["username"]) != clickhouseAuthUsername || string(secret.Data["password"]) != "s3cret" {
		t.Errorf("data = %v", secret.Data)
	}
	if secret.Labels[ComponentLabel] != ComponentClickHouse {
		t.Errorf("labels = %v", secret.Labels)
	}
}

func TestGenerateClickHousePassword(t *testing.T) {
	p1, err := GenerateClickHousePassword()
	if err != nil {
		t.Fatalf("GenerateClickHousePassword: %v", err)
	}
	if len(p1) != 32 {
		t.Errorf("password length = %d, want 32", len(p1))
	}
	if _, err := hex.DecodeString(p1); err != nil {
		t.Errorf("password %q is not hex: %v", p1, err)
	}
	p2, _ := GenerateClickHousePassword()
	if p1 == p2 {
		t.Error("consecutive passwords must differ")
	}
}

// Managed mode end-to-end through the builders: the proxies must also
// resolve the managed endpoint and auth secret.
func TestProxyTelemetryEnvsManaged(t *testing.T) {
	z := testManagedTelemetryZabbix(testVersion80)
	pg := testProxyGroup()
	pg.Spec.Version = testVersion80

	envs := ProxyTelemetryEnvs(pg, z)
	env, ok := envByName(envs, testTelemetryEnv0)
	if !ok {
		t.Fatalf("missing telemetry provider env in %v", envs)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(env.Value), &cfg); err != nil {
		t.Fatalf("telemetry provider JSON: %v", err)
	}
	if cfg["url"] != testManagedEndpoint {
		t.Errorf("managed telemetry url = %v", cfg["url"])
	}
	env, _ = envByName(envs, testTelemetryEnv0+"_PASSWORD_FILE")
	if env.Value != "/etc/zabbix/secrets/clickhouse/password" {
		t.Errorf("managed password file = %q", env.Value)
	}
}

func TestConvergeAllIncludesClickHouseWhenManaged(t *testing.T) {
	// Pure sanity on the object list assembly is covered by the
	// controller envtest suite; here only the enabling predicate.
	z := testManagedTelemetryZabbix(testVersion80)
	if !ManagedClickHouseEnabled(z) {
		t.Fatal("predicate regression")
	}
	z.Spec.TelemetryStorage.ClickHouse.Managed = nil
	if ManagedClickHouseEnabled(z) {
		t.Fatal("predicate regression (nil managed)")
	}
}
