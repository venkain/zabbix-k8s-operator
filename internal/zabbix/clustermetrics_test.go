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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Recorded wire constants for the fake API: the lab template id
// (quoted in JSON) and the instance parameters used across cases.
const (
	cmTestClusterName     = "kind-lab"
	cmTestTLSNone         = "none"
	cmTestMonitorToken    = "monitor-token"
	cmTestAPIToken        = "api-token"
	cmTestTemplateID      = "10788"
	cmTestMacroTokenValue = "tok"
	cmTestHostID          = "10818"
	cmTestHostIDNum       = 10818

	methodHostGroupGet    = "hostgroup.get"
	methodHostGroupCreate = "hostgroup.create"
	methodTemplateGet     = "template.get"
	methodHostGet         = "host.get"
	methodHostCreate      = "host.create"
	methodHostUpdate      = "host.update"
	methodHostDelete      = "host.delete"

	paramHostIDs         = "hostids"
	paramGroupIDs        = "groupids"
	paramParentTemplates = "parentTemplates"
	paramMacrosField     = "macros"
	paramInterfaces      = "interfaces"
)

// testZabbixForMetrics is the instance the builder tests run against.
func testZabbixForMetrics() *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		Spec: zabbixv1alpha1.ZabbixSpec{
			ClusterMetrics: &zabbixv1alpha1.ClusterMetricsSpec{
				ClusterName: cmTestClusterName,
			},
		},
	}
}

// TestKubeStateMetricsDiscoveryContract pins the two wire constraints
// the "Kubernetes Cluster by HTTP" template imposes on the endpoint
// (docs/requirements.md, OQ5): the Service NAME ends with
// "kube-state-metrics" and the port is named "http".
func TestKubeStateMetricsDiscoveryContract(t *testing.T) {
	z := testZabbixForMetrics()

	svc := KubeStateMetricsService(z)
	if !strings.HasSuffix(svc.Name, "kube-state-metrics") {
		t.Errorf("service name %q must end with kube-state-metrics (template discovery regex)", svc.Name)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Name != portNameClickhouseSQL {
		t.Fatalf("service ports = %+v, want one port named http", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != KubeStateMetricsPort {
		t.Errorf("service port = %d, want %d", svc.Spec.Ports[0].Port, KubeStateMetricsPort)
	}

	dep := KubeStateMetricsDeployment(z)
	if dep.Name != svc.Name {
		t.Errorf("deployment name %q does not match service name %q (selector would break)", dep.Name, svc.Name)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != dep.Name {
		t.Errorf("KSM service account = %q, want the per-instance account", dep.Spec.Template.Spec.ServiceAccountName)
	}
	port := dep.Spec.Template.Spec.Containers[0].Ports[0]
	if port.Name != "http" || port.ContainerPort != KubeStateMetricsPort {
		t.Errorf("container port = %+v, want http/%d", port, KubeStateMetricsPort)
	}
	if img := dep.Spec.Template.Spec.Containers[0].Image; !strings.Contains(img, "kube-state-metrics:v") {
		t.Errorf("default image = %q, want the pinned kube-state-metrics image", img)
	}
}

// TestKubeStateMetricsImageOverride checks the ImageSpec override path.
func TestKubeStateMetricsImageOverride(t *testing.T) {
	z := testZabbixForMetrics()
	z.Spec.ClusterMetrics.KubeStateMetrics = &zabbixv1alpha1.KubeStateMetricsSpec{
		Image: &zabbixv1alpha1.ImageSpec{Repository: "mirror.example.com/ksm", Tag: "v3.0.0"},
	}
	dep := KubeStateMetricsDeployment(z)
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "mirror.example.com/ksm:v3.0.0" {
		t.Errorf("image = %q, want the overridden pair", got)
	}
}

// TestMonitorIdentity pins the token plumbing: the monitor SA does not
// automount (nothing runs under it), the token Secret is the legacy
// service-account-token type pointing at the account through the
// annotation, and the SA token controller fills data["token"].
func TestMonitorIdentity(t *testing.T) {
	z := testZabbixForMetrics()

	sa := MonitorServiceAccount(z)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("monitor SA must not automount: no pod runs under it")
	}

	secret := MonitorTokenSecret(z)
	if secret.Type != corev1.SecretTypeServiceAccountToken {
		t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeServiceAccountToken)
	}
	if got := secret.Annotations[corev1.ServiceAccountNameKey]; got != sa.Name {
		t.Errorf("annotation = %q, want the monitor SA name %q", got, sa.Name)
	}
	if len(secret.Data) != 0 {
		t.Errorf("secret must start empty (token controller fills it), data = %v", secret.Data)
	}
}

// TestClusterMetricsRBAC checks that the shared ClusterRole carries the
// API-proxy subresources the template scrapes through (services/proxy,
// nodes/proxy) and that the binding covers both identities.
func TestClusterMetricsRBAC(t *testing.T) {
	z := testZabbixForMetrics()

	role := ClusterMetricsClusterRole(z)
	if role.Name != z.Namespace+"-"+KubeStateMetricsName(z.Name) {
		t.Errorf("cluster role name = %q, want the namespace-embedded name", role.Name)
	}
	foundProxy := false
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "services/proxy" || res == "nodes/proxy" {
				foundProxy = true
			}
		}
	}
	if !foundProxy {
		t.Error("ClusterRole lacks services/proxy or nodes/proxy: the template scrapes through the API proxy")
	}

	binding := ClusterMetricsClusterRoleBinding(z)
	if binding.RoleRef.Name != role.Name {
		t.Errorf("binding roleRef = %q, want %q", binding.RoleRef.Name, role.Name)
	}
	subjects := map[string]bool{}
	for _, s := range binding.Subjects {
		subjects[s.Name] = true
	}
	if !subjects[KubeStateMetricsName(z.Name)] || !subjects[MonitorServiceAccountName(z.Name)] {
		t.Errorf("binding subjects = %v, want both the KSM pod identity and the monitor account", subjects)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.APIGroup != rbacv1.GroupName {
		t.Errorf("roleRef = %+v", binding.RoleRef)
	}
}

// TestMacrosEqual covers the drift detection over operator-managed
// macros only.
func TestMacrosEqual(t *testing.T) {
	desired := clusterMacros(cmTestMacroTokenValue, cmTestTLSNone)
	if !macrosEqual(desired, []Macro{{Macro: MacroKubeAPIToken, Value: cmTestMacroTokenValue}, {Macro: MacroHTTPTLSVerify, Value: cmTestTLSNone}}) {
		t.Error("equal macros reported as drift")
	}
	if macrosEqual(desired, []Macro{{Macro: MacroKubeAPIToken, Value: cmTestMacroTokenValue}}) {
		t.Error("missing TLS macro must be reported as drift")
	}
	if macrosEqual(desired, []Macro{{Macro: MacroKubeAPIToken, Value: cmTestMacroTokenValue}, {Macro: MacroHTTPTLSVerify, Value: "full"}}) {
		t.Error("changed TLS value must be reported as drift")
	}
	// Extra user macros on the live host are not drift: the operator
	// manages only its own two.
	if !macrosEqual(desired, append(desired, Macro{Macro: "{$USER.MACRO}", Value: "x"})) {
		t.Error("foreign macros must not count as drift")
	}
}

// recordedRequest is one captured JSON-RPC call.
type recordedRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fakeZabbixAPI answers the host-management method sequence and records
// every call for assertions.
type fakeZabbixAPI struct {
	t       *testing.T
	handler func(t *testing.T, req recordedRequest) (result any)
	calls   []recordedRequest
}

func (f *fakeZabbixAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var env struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		f.t.Fatalf("decoding request: %v", err)
	}
	f.calls = append(f.calls, recordedRequest{Method: env.Method, Params: env.Params})
	result := f.handler(f.t, recordedRequest{Method: env.Method, Params: env.Params})
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": result, "id": env.ID})
	_, _ = w.Write(raw)
}

func (f *fakeZabbixAPI) methodCalls(method string) []recordedRequest {
	var found []recordedRequest
	for _, c := range f.calls {
		if c.Method == method {
			found = append(found, c)
		}
	}
	return found
}

// jsonParams decodes a recorded params blob.
func jsonParams(t *testing.T, req recordedRequest, into any) {
	t.Helper()
	if err := json.Unmarshal(req.Params, into); err != nil {
		t.Fatalf("decoding %s params: %v", req.Method, err)
	}
}

// TestEnsureClusterHostCreates covers the create path: missing group is
// created, the template is resolved by NAME, and host.create carries
// the template linkage, the dummy interface and the two macros.
func TestEnsureClusterHostCreates(t *testing.T) {
	fake := &fakeZabbixAPI{t: t}
	fake.handler = func(t *testing.T, req recordedRequest) any {
		switch req.Method {
		case methodHostGroupGet:
			return []map[string]any{}
		case methodHostGroupCreate:
			return map[string]any{paramGroupIDs: []string{"22"}}
		case methodTemplateGet:
			return []map[string]any{{paramTemplateID: cmTestTemplateID, paramName: ClusterTemplate}}
		case methodHostGet:
			return []map[string]any{}
		case methodHostCreate:
			return map[string]any{paramHostIDs: []string{"10818"}}
		default:
			t.Fatalf("unexpected method %q", req.Method)
			return nil
		}
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	id, created, err := EnsureClusterHost(context.Background(), server.Client(), server.URL, cmTestAPIToken,
		cmTestClusterName, cmTestMonitorToken, cmTestTLSNone)
	if err != nil {
		t.Fatalf("EnsureClusterHost: %v", err)
	}
	if !created || id != cmTestHostIDNum {
		t.Fatalf("created = %v, id = %d", created, id)
	}

	if got := len(fake.methodCalls(methodHostGroupGet)); got != 1 {
		t.Errorf("hostgroup.get called %d times, want 1", got)
	}
	var createParams struct {
		Host       string           `json:"host"`
		Groups     []map[string]any `json:"groups"`
		Templates  []map[string]any `json:"templates"`
		Interfaces []map[string]any `json:"interfaces"`
		Macros     []Macro          `json:"macros"`
	}
	jsonParams(t, fake.methodCalls(methodHostCreate)[0], &createParams)
	if createParams.Host != cmTestClusterName {
		t.Errorf("host = %q", createParams.Host)
	}
	if len(createParams.Templates) != 1 || createParams.Templates[0][paramTemplateID] != cmTestTemplateID {
		t.Errorf("templates = %v, want the name-resolved template", createParams.Templates)
	}
	if len(createParams.Interfaces) != 1 || createParams.Interfaces[0]["ip"] != "127.0.0.1" {
		t.Errorf("interfaces = %v, want the dummy agent interface", createParams.Interfaces)
	}
	if len(createParams.Macros) != 2 {
		t.Fatalf("macros = %v, want exactly the two operator-managed macros", createParams.Macros)
	}
	if createParams.Macros[0] != (Macro{Macro: MacroKubeAPIToken, Value: cmTestMonitorToken}) ||
		createParams.Macros[1] != (Macro{Macro: MacroHTTPTLSVerify, Value: cmTestTLSNone}) {
		t.Errorf("macros = %+v", createParams.Macros)
	}
}

// TestEnsureClusterHostRealigns covers the update path: an existing
// host whose TLS macro drifted gets a host.update with the corrected
// macro set and the template linkage.
func TestEnsureClusterHostRealigns(t *testing.T) {
	fake := &fakeZabbixAPI{t: t}
	fake.handler = func(t *testing.T, req recordedRequest) any {
		switch req.Method {
		case methodHostGroupGet:
			return []map[string]any{{"groupid": "22", paramName: ClusterHostGroup}}
		case methodTemplateGet:
			return []map[string]any{{paramTemplateID: cmTestTemplateID, paramName: ClusterTemplate}}
		case methodHostGet:
			return []map[string]any{{
				paramHostID: "10818", paramHost: cmTestClusterName,
				paramParentTemplates: []map[string]any{{paramTemplateID: cmTestTemplateID}},
				paramMacrosField:     []map[string]any{{paramMacro: MacroHTTPTLSVerify, paramValue: "full"}},
			}}
		case methodHostUpdate:
			return map[string]any{paramHostIDs: []string{"10818"}}
		default:
			t.Fatalf("unexpected method %q", req.Method)
			return nil
		}
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	id, created, err := EnsureClusterHost(context.Background(), server.Client(), server.URL, cmTestAPIToken,
		cmTestClusterName, cmTestMonitorToken, cmTestTLSNone)
	if err != nil {
		t.Fatalf("EnsureClusterHost: %v", err)
	}
	if created || id != cmTestHostIDNum {
		t.Fatalf("created = %v, id = %d; existing host must only be updated", created, id)
	}
	if got := len(fake.methodCalls(methodHostUpdate)); got != 1 {
		t.Fatalf("host.update called %d times, want 1 (drifted macro)", got)
	}
	var updateParams struct {
		HostID    string           `json:"hostid"`
		Templates []map[string]any `json:"templates"`
		Macros    []Macro          `json:"macros"`
	}
	jsonParams(t, fake.methodCalls(methodHostUpdate)[0], &updateParams)
	if updateParams.HostID != cmTestHostID {
		t.Errorf("hostid = %q", updateParams.HostID)
	}
	if !strings.Contains(updateParams.Macros[1].Value, cmTestTLSNone) {
		t.Errorf("TLS macro value = %q, want the desired none", updateParams.Macros[1].Value)
	}
}

// TestEnsureClusterHostSteadyStateIsNoWrite: an existing host with the
// right template and macros must not trigger a host.update.
func TestEnsureClusterHostSteadyStateIsNoWrite(t *testing.T) {
	fake := &fakeZabbixAPI{t: t}
	fake.handler = func(t *testing.T, req recordedRequest) any {
		switch req.Method {
		case methodHostGroupGet:
			return []map[string]any{{"groupid": "22", paramName: ClusterHostGroup}}
		case methodTemplateGet:
			return []map[string]any{{paramTemplateID: cmTestTemplateID, paramName: ClusterTemplate}}
		case methodHostGet:
			return []map[string]any{{
				paramHostID: "10818", paramHost: cmTestClusterName,
				paramParentTemplates: []map[string]any{{paramTemplateID: cmTestTemplateID}},
				paramMacrosField: []map[string]any{
					{paramMacro: MacroKubeAPIToken, paramValue: cmTestMonitorToken},
					{paramMacro: MacroHTTPTLSVerify, paramValue: cmTestTLSNone},
				},
			}}
		default:
			t.Fatalf("unexpected method %q: steady state must not mutate", req.Method)
			return nil
		}
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	if _, created, err := EnsureClusterHost(context.Background(), server.Client(), server.URL, cmTestAPIToken,
		cmTestClusterName, cmTestMonitorToken, cmTestTLSNone); err != nil || created {
		t.Fatalf("err = %v, created = %v", err, created)
	}
}

// TestDeleteClusterHost: the finalizer cleanup deletes by id and treats
// an already-gone host as success.
func TestDeleteClusterHost(t *testing.T) {
	fake := &fakeZabbixAPI{t: t}
	fake.handler = func(t *testing.T, req recordedRequest) any {
		switch req.Method {
		case methodHostGet:
			return []map[string]any{{paramHostID: "10818", paramHost: cmTestClusterName}}
		case methodHostDelete:
			return map[string]any{paramHostIDs: []string{"10818"}}
		default:
			t.Fatalf("unexpected method %q", req.Method)
			return nil
		}
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	if err := DeleteClusterHost(context.Background(), server.Client(), server.URL, cmTestAPIToken, cmTestClusterName); err != nil {
		t.Fatalf("DeleteClusterHost: %v", err)
	}

	// Idempotent second pass: host.get now returns nothing.
	fake.handler = func(t *testing.T, req recordedRequest) any {
		if req.Method != methodHostGet {
			t.Fatalf("unexpected method %q: deleting a missing host must be a no-op", req.Method)
		}
		return []map[string]any{}
	}
	if err := DeleteClusterHost(context.Background(), server.Client(), server.URL, cmTestAPIToken, cmTestClusterName); err != nil {
		t.Fatalf("DeleteClusterHost (missing host): %v", err)
	}
}
