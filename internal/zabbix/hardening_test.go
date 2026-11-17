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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

const (
	// Shared test fixture names (goconst): the instance name and the
	// JSON-RPC methods the hardening endpoints use.
	testZbxName    = "zbx"
	testMethodGet  = "mfa.get"
	testAuthGet    = "authentication.get"
	testAuthUpdate = "authentication.update"

	methodMFACreate = "mfa.create"
	methodMFADelete = "mfa.delete"
)

// hardeningResource builds a Zabbix resource with the given hardening
// spec pointer (nil = absent block = hardened defaults).
func hardeningResource(h *zabbixv1alpha1.HardeningSpec) *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: testZbxName, Namespace: "monitoring"},
		Spec: zabbixv1alpha1.ZabbixSpec{
			Hardening: h,
		},
	}
}

func TestPasswordPolicyDefaults(t *testing.T) {
	for name, z := range map[string]*zabbixv1alpha1.Zabbix{
		"absent block": hardeningResource(nil),
		"empty block":  hardeningResource(&zabbixv1alpha1.HardeningSpec{}),
		"nil policy":   hardeningResource(&zabbixv1alpha1.HardeningSpec{PasswordPolicy: nil}),
	} {
		if !PasswordPolicyEnabled(z) {
			t.Errorf("%s: policy should be enabled by default", name)
		}
		want := PasswordPolicy{MinLength: 12, CheckRules: 15}
		if got := EffectivePasswordPolicy(z); got != want {
			t.Errorf("%s: policy = %+v, want %+v", name, got, want)
		}
	}
}

func TestPasswordPolicyOptOuts(t *testing.T) {
	disabled := false
	z := hardeningResource(&zabbixv1alpha1.HardeningSpec{
		PasswordPolicy: &zabbixv1alpha1.PasswordPolicySpec{
			Enabled:    &disabled,
			MinLength:  20,
			CheckRules: []string{"case", "digits"},
		},
	})
	if PasswordPolicyEnabled(z) {
		t.Error("explicit enabled=false must disable the policy")
	}
	if HardeningNeedsFinalizer(z) {
		t.Error("no API-driven part active: no finalizer needed")
	}

	enabled := true
	z2 := hardeningResource(&zabbixv1alpha1.HardeningSpec{
		PasswordPolicy: &zabbixv1alpha1.PasswordPolicySpec{
			Enabled:    &enabled,
			CheckRules: []string{"case", "digits", "special", "commonPasswords"},
		},
	})
	if got := EffectivePasswordPolicy(z2); got.MinLength != 12 || got.CheckRules != 15 {
		t.Errorf("all rules explicit = %+v, want 12/15", got)
	}
	if !HardeningNeedsFinalizer(z2) {
		t.Error("password policy active: finalizer needed")
	}
}

func TestMFAOptInResolution(t *testing.T) {
	z := hardeningResource(nil)
	if MFARequested(z) {
		t.Error("absent mfa block must not request MFA")
	}
	// Password-only default hardening IS finalizer-worthy (the
	// password policy is on); fully opted out is not.
	if !HardeningNeedsFinalizer(z) {
		t.Error("password-only default hardening must be finalizer-worthy")
	}
	policyOff := false
	off := hardeningResource(&zabbixv1alpha1.HardeningSpec{
		PasswordPolicy: &zabbixv1alpha1.PasswordPolicySpec{Enabled: &policyOff},
		NetworkPolicy:  &policyOff,
	})
	if HardeningNeedsFinalizer(off) {
		t.Error("fully opted-out hardening needs no finalizer")
	}

	z2 := hardeningResource(&zabbixv1alpha1.HardeningSpec{
		MFA: &zabbixv1alpha1.MFASpec{},
	})
	if !MFARequested(z2) {
		t.Error("presence of the mfa block enables MFA")
	}
	if got := MFAHashFunction(z2); got != 3 {
		t.Errorf("default hash function = %d, want 3 (sha512)", got)
	}
	if got := MFACodeLength(z2); got != 8 {
		t.Errorf("default code length = %d, want 8", got)
	}
	if got := MFAMethodName(z2.Name); got != testZbxName+"-totp" {
		t.Errorf("method name = %q, want %q", got, testZbxName+"-totp")
	}

	z3 := hardeningResource(&zabbixv1alpha1.HardeningSpec{
		MFA: &zabbixv1alpha1.MFASpec{HashFunction: "sha256", CodeLength: 6},
	})
	if got := MFAHashFunction(z3); got != 2 {
		t.Errorf("sha256 = %d, want 2", got)
	}
	if got := MFACodeLength(z3); got != 6 {
		t.Errorf("code length = %d, want 6", got)
	}
}

func TestSupportsMFA(t *testing.T) {
	cases := map[string]bool{
		"7.0.0":  true,
		"8.0.10": true,
		"6.4.5":  false,
		"6.0.1":  false,
	}
	for version, want := range cases {
		if got := SupportsMFA(version); got != want {
			t.Errorf("SupportsMFA(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestNetworkPolicyHardeningDefaults(t *testing.T) {
	z := hardeningResource(nil)
	if !NetworkPolicyHardeningEnabled(z) {
		t.Error("NetworkPolicy lockdown must be on by default")
	}
	disabled := false
	z2 := hardeningResource(&zabbixv1alpha1.HardeningSpec{NetworkPolicy: &disabled})
	if NetworkPolicyHardeningEnabled(z2) {
		t.Error("explicit networkPolicy=false must disable the lockdown")
	}
}

func TestDescribeHardeningMentionsParts(t *testing.T) {
	z := hardeningResource(&zabbixv1alpha1.HardeningSpec{MFA: &zabbixv1alpha1.MFASpec{}})
	d := DescribeHardening(z)
	for _, part := range []string{"12", "TOTP", "NetworkPolicy"} {
		if !strings.Contains(d, part) {
			t.Errorf("summary %q missing %q", d, part)
		}
	}
}

func TestKSMNetworkPolicyShape(t *testing.T) {
	z := &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: testZbxName, Namespace: "default"},
		Spec: zabbixv1alpha1.ZabbixSpec{
			ClusterMetrics: &zabbixv1alpha1.ClusterMetricsSpec{},
		},
	}
	np := KSMNetworkPolicy(z)
	if np.Name != KubeStateMetricsName(z.Name)+"-egress" {
		t.Errorf("name = %q", np.Name)
	}
	if np.Namespace != clusterMetricsNamespaceOf(z) {
		t.Errorf("namespace = %q, want the cluster-metrics placement namespace", np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Egress" {
		t.Errorf("policy types = %+v, want egress-only (ingress stays open: the scrape goes through the API proxy)", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (DNS + API-server HTTPS)", len(np.Spec.Egress))
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].PodSelector == nil ||
		dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("dns rule peers = %+v", dns.To)
	}
	ports := dns.Ports
	if len(ports) != 2 || ports[0].Port.IntValue() != 53 || ports[1].Port.IntValue() != 53 {
		t.Errorf("dns rule ports = %+v", ports)
	}
	api := np.Spec.Egress[1]
	if len(api.To) != 1 || api.To[0].IPBlock == nil || api.To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("api rule peers = %+v", api.To)
	}
	// Both lanes: the service port (pre-DNAT matching agents) and the
	// backend port (post-DNAT matching: kind's Kindnet sees the
	// host-networked API server's 6443).
	if len(api.Ports) != 2 ||
		api.Ports[0].Port.IntValue() != 443 || api.Ports[1].Port.IntValue() != 6443 {
		t.Errorf("api rule ports = %+v, want 443 + 6443", api.Ports)
	}
}

func TestMCPNetworkPolicyShape(t *testing.T) {
	z := hardeningResource(nil)
	np := MCPNetworkPolicy(z)
	if np.Name != MCPServerName(z.Name)+"-policy" {
		t.Errorf("name = %q", np.Name)
	}
	if np.Namespace != z.Namespace {
		t.Errorf("namespace = %q, want the resource namespace", np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Fatalf("policy types = %+v, want ingress + egress", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 ||
		np.Spec.Ingress[0].Ports[0].Port.IntValue() != int(MCPPort) {
		t.Errorf("ingress = %+v, want the MCP port only", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (DNS + frontend)", len(np.Spec.Egress))
	}
	fe := np.Spec.Egress[1]
	if len(fe.To) != 1 || fe.To[0].PodSelector == nil ||
		fe.To[0].PodSelector.MatchLabels[ComponentLabel] != ComponentFrontend {
		t.Errorf("frontend egress peers = %+v", fe.To)
	}
	if len(fe.Ports) != 1 || fe.Ports[0].Port.IntValue() != int(FrontendPort) {
		t.Errorf("frontend egress ports = %+v", fe.Ports)
	}
}

// mfaRequestShape captures the raw request params for wire-shape checks.
type mfaRequestShape struct {
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	ID     int              `json:"id"`
	Result *json.RawMessage `json:"-"`
}

func TestEnsureTOTPMFAWireContract(t *testing.T) {
	// Recorded from the kind lab (Zabbix 8.0): mfa.create needs
	// type/name/hash_function/code_length and returns QUOTED ids;
	// authentication fields arrive as quoted integers; mfa.delete
	// takes a BARE ARRAY and returns the ids UNQUOTED.
	var createSeen, authSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequestShape
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case testMethodGet:
			if createSeen {
				// After creation the method is visible.
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","result":[{"mfaid":"1","type":"1","name":"%s-totp","hash_function":"3","code_length":"8","api_hostname":"","clientid":""}],"id":1}`, testZbxName)
			} else {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[],"id":1}`))
			}
		case methodMFACreate:
			createSeen = true
			var params map[string]any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("mfa.create params: %v", err)
			}
			for k, want := range map[string]any{"type": float64(1), "name": testZbxName + "-totp", "hash_function": float64(3), "code_length": float64(8)} {
				if got := params[k]; got != want {
					t.Errorf("mfa.create %s = %v, want %v", k, got, want)
				}
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfaids":["1"]},"id":2}`))
		case testAuthGet:
			// First run: MFA off (the factory state). After the
			// operator's update the global authentication is on and
			// linked to the operator's method (the idempotency state).
			if authSeen {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfa_status":"1","mfaid":"1"},"id":3}`))
			} else {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfa_status":"0","mfaid":"0"},"id":3}`))
			}
		case testAuthUpdate:
			authSeen = true
			var params map[string]any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("authentication.update params: %v", err)
			}
			if params["mfa_status"] != float64(1) {
				t.Errorf("mfa_status = %v, want 1", params["mfa_status"])
			}
			// 8.0 accepts the id quoted or unquoted; the operator sends
			// the pre-quoted form intToStringID produces.
			if s, ok := params["mfaid"].(string); !ok || s != "1" {
				t.Errorf("mfaid = %v, want quoted \"1\"", params["mfaid"])
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":["mfa_status","mfaid"],"id":4}`))
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	z := hardeningResource(&zabbixv1alpha1.HardeningSpec{MFA: &zabbixv1alpha1.MFASpec{}})
	id, changed, err := EnsureTOTPMFA(context.Background(), server.Client(), server.URL, "tok", z)
	if err != nil {
		t.Fatalf("EnsureTOTPMFA: %v", err)
	}
	if id != 1 || !changed || !createSeen || !authSeen {
		t.Errorf("EnsureTOTPMFA = (%d, %v); createSeen=%v authSeen=%v", id, changed, createSeen, authSeen)
	}

	// Second run: nothing to change (idempotent).
	id, changed, err = EnsureTOTPMFA(context.Background(), server.Client(), server.URL, "tok", z)
	if err != nil {
		t.Fatalf("EnsureTOTPMFA second run: %v", err)
	}
	if id != 1 || changed {
		t.Errorf("idempotent run = (%d, %v), want (1, false)", id, changed)
	}
}

func TestDisableMFAWireContract(t *testing.T) {
	// The lab shape: MFA is on and linked to the operator's method;
	// the disable must switch the authentication off first and delete
	// the method with a bare-array mfa.delete.
	var deleteSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequestShape
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case testAuthGet:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfa_status":"1","mfaid":"1"},"id":1}`))
		case testMethodGet:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","result":[{"mfaid":"1","type":"1","name":"%s-totp","hash_function":"3","code_length":"8"}],"id":2}`, testZbxName)
		case testAuthUpdate:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":["mfa_status","mfaid"],"id":3}`))
		case methodMFADelete:
			deleteSeen = true
			var params []any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("mfa.delete params must be a BARE ARRAY: %v", err)
			}
			if len(params) != 1 {
				t.Errorf("mfa.delete params = %v, want one id", params)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfaids":[1]},"id":4}`))
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	if err := DisableMFA(context.Background(), server.Client(), server.URL, "tok", testZbxName); err != nil {
		t.Fatalf("DisableMFA: %v", err)
	}
	if !deleteSeen {
		t.Error("mfa.delete was never called")
	}
}

func TestDisableMFALeavesForeignMethod(t *testing.T) {
	// MFA linked to a USER-configured method (not the operator's):
	// the authentication link stays; only the operator's own method
	// (absent here) would be deleted — nothing at all happens.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequestShape
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case testAuthGet:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mfa_status":"1","mfaid":"7"},"id":1}`))
		case testMethodGet:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[{"mfaid":"7","type":"1","name":"corporate-ldap-totp","hash_function":"2","code_length":"6"}],"id":2}`))
		default:
			t.Errorf("unexpected method %q — a foreign MFA link must not be touched", req.Method)
		}
	}))
	defer server.Close()

	if err := DisableMFA(context.Background(), server.Client(), server.URL, "tok", testZbxName); err != nil {
		t.Fatalf("DisableMFA: %v", err)
	}
}

func TestEnsurePasswordPolicyDiffBased(t *testing.T) {
	var updates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequestShape
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case testAuthGet:
			// Lab values: 12/15 already applied.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"passwd_min_length":"12","passwd_check_rules":"15"},"id":1}`))
		case testAuthUpdate:
			updates++
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":["passwd_min_length"],"id":2}`))
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	desired := PasswordPolicy{MinLength: 12, CheckRules: 15}
	applied, err := EnsurePasswordPolicy(context.Background(), server.Client(), server.URL, "tok", desired)
	if err != nil {
		t.Fatalf("EnsurePasswordPolicy: %v", err)
	}
	if applied || updates != 0 {
		t.Errorf("already-converged settings must not be rewritten (applied=%v updates=%d)", applied, updates)
	}

	desired.MinLength = 20
	applied, err = EnsurePasswordPolicy(context.Background(), server.Client(), server.URL, "tok", desired)
	if err != nil {
		t.Fatalf("EnsurePasswordPolicy: %v", err)
	}
	if !applied || updates != 1 {
		t.Errorf("diverged settings must be rewritten (applied=%v updates=%d)", applied, updates)
	}
}

func TestEnsurePasswordPolicyStock(t *testing.T) {
	// The reset fires ONLY when the live policy still carries the
	// operator's recorded values: a hand-tuned policy (or one that
	// drifted between the check and the call) is never touched, and an
	// already-stock policy is a no-op.
	var updates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequestShape
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case testAuthGet:
			// A hand-tuned policy: neither stock nor operator values.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"passwd_min_length":"16","passwd_check_rules":"7"},"id":1}`))
		case testAuthUpdate:
			updates++
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[],"id":2}`))
		}
	}))
	defer server.Close()

	recorded := PasswordPolicy{MinLength: 12, CheckRules: 15}
	applied, err := EnsurePasswordPolicyStock(context.Background(), server.Client(), server.URL, "tok", recorded)
	if err != nil {
		t.Fatalf("EnsurePasswordPolicyStock: %v", err)
	}
	if applied || updates != 0 {
		t.Errorf("a hand-tuned policy must NEVER be reset (applied=%v updates=%d)", applied, updates)
	}
}
