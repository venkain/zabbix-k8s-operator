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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// TestSupportsAgentIntegration verifies the 8.0+ version gate: there is
// no MCP server in 7.0.
func TestSupportsAgentIntegration(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"7.0", false},
		{"7.4", false},
		{testVersion80, true},
		{"8.0.10", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := SupportsAgentIntegration(tc.version); got != tc.want {
			t.Errorf("SupportsAgentIntegration(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// testZabbixWithMCP returns an instance with the MCP feature enabled.
func testZabbixWithMCP() *zabbixv1alpha1.Zabbix {
	z := testZabbix()
	z.Spec.AgentIntegration = &zabbixv1alpha1.AgentIntegrationSpec{
		MCP: &zabbixv1alpha1.MCPSpec{},
	}
	return z
}

// TestMCPEnabled verifies the enable contract (presence of the optional
// block) and that nil blocks read as disabled.
func TestMCPEnabled(t *testing.T) {
	if MCPEnabled(testZabbix()) {
		t.Error("nil agentIntegration read as enabled")
	}
	z := testZabbix()
	z.Spec.AgentIntegration = &zabbixv1alpha1.AgentIntegrationSpec{}
	if MCPEnabled(z) {
		t.Error("agentIntegration without mcp read as enabled")
	}
	if !MCPEnabled(testZabbixWithMCP()) {
		t.Error("agentIntegration.mcp read as disabled")
	}
}

// TestMCPImage verifies the image derives from the instance version and
// honors overrides (official repo zabbix/zabbix-mcp-server).
func TestMCPImage(t *testing.T) {
	z := testZabbixWithMCP()
	if got, want := MCPImage(z), "zabbix/zabbix-mcp-server:ubuntu-8.0-latest"; got != want {
		t.Errorf("MCPImage = %q, want %q", got, want)
	}
	z.Spec.AgentIntegration.MCP.Image = &zabbixv1alpha1.ImageSpec{Tag: "alpine-8.0.10"}
	if got, want := MCPImage(z), "zabbix/zabbix-mcp-server:alpine-8.0.10"; got != want {
		t.Errorf("MCPImage override = %q, want %q", got, want)
	}
}

// TestMCPDeploymentWiring verifies the frontend URL wiring and the
// restricted container context (the image's numeric uid).
func TestMCPDeploymentWiring(t *testing.T) {
	d := MCPDeployment(testZabbixWithMCP())

	if got, want := d.Name, MCPServerName(testInstance); got != want {
		t.Errorf("deployment name = %q, want %q", got, want)
	}
	if *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1 (stateless by design)", *d.Spec.Replicas)
	}

	c := d.Spec.Template.Spec.Containers[0]
	idx := envIndex(c.Env, "ZBX_FRONTEND_URL")
	if idx < 0 {
		t.Fatalf("ZBX_FRONTEND_URL missing from env %v", c.Env)
	}
	want := "http://" + FrontendName(testInstance) + "." + testNamespace + ".svc:80/api_jsonrpc.php"
	if got := c.Env[idx].Value; got != want {
		t.Errorf("ZBX_FRONTEND_URL = %q, want %q (instance's own frontend Service)", got, want)
	}

	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != mcpServerUID {
		t.Errorf("container security context %+v, want restricted with runAsUser %d", c.SecurityContext, mcpServerUID)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil {
		t.Errorf("readiness probe %v, want TCP (the transport answers 401 without a token)", c.ReadinessProbe)
	}
}

// TestMCPEnvPolicy verifies the optional env wiring: AllowedIPs
// (comma-joined), the tool prefix, and that an empty spec renders no
// optional env (the image defaults stay in effect).
func TestMCPEnvPolicy(t *testing.T) {
	z := testZabbixWithMCP()
	z.Spec.AgentIntegration.MCP.AllowedIPs = []string{"10.0.0.0/8", "mcp-client.example.com"}
	z.Spec.AgentIntegration.MCP.ToolPrefix = "zbx_"

	env := MCPDeployment(z).Spec.Template.Spec.Containers[0].Env
	idx := envIndex(env, "ZBX_ALLOWEDIP")
	if idx < 0 || env[idx].Value != "10.0.0.0/8,mcp-client.example.com" {
		t.Errorf("ZBX_ALLOWEDIP = %v, want comma-joined list", env)
	}
	idx = envIndex(env, "ZBX_TOOL_PREFIX")
	if idx < 0 || env[idx].Value != "zbx_" {
		t.Errorf("ZBX_TOOL_PREFIX = %v, want zbx_", env)
	}
}

// unDenyHistoryGet is the default-denied tool the takeover case
// explicitly un-denies.
const unDenyHistoryGet = "history_get"

// TestMCPToolPolicyTakeover pins the rendering contract of the image's
// tool rules: setting ANY indexed variable replaces the default policy
// entirely, so the operator renders its own deny list. Order is
// evaluation order (first match wins): user allows first, then user
// denies, then the default denies minus the tools the user's rules
// already decide.
func TestMCPToolPolicyTakeover(t *testing.T) {
	// No rules at all: no policy envs — the image default config stays.
	if envs := mcpToolEnvs(&zabbixv1alpha1.MCPSpec{}); len(envs) != 0 {
		t.Errorf("empty policy rendered %v, want none (image defaults)", envs)
	}

	// Un-deny a default-denied tool: the allow must come BEFORE the
	// default denies (and history_get must not be denied again).
	spec := &zabbixv1alpha1.MCPSpec{AllowTools: []string{unDenyHistoryGet}}
	envs := mcpToolEnvs(spec)
	if got := envs[0]; got.Name != "ZBX_ALLOWTOOL_0" || got.Value != unDenyHistoryGet {
		t.Errorf("first rule = %s=%s, want ZBX_ALLOWTOOL_0=%s", got.Name, got.Value, unDenyHistoryGet)
	}
	denied := map[string]bool{}
	for _, e := range envs {
		if strings.HasPrefix(e.Name, "ZBX_DENYTOOL_") {
			denied[e.Value] = true
		}
	}
	if denied[unDenyHistoryGet] {
		t.Error("history_get denied although explicitly allowed (first-match-wins order violated)")
	}
	for _, def := range defaultMCPDenyTools {
		if def == unDenyHistoryGet {
			continue
		}
		if !denied[def] {
			t.Errorf("default deny %s lost in policy takeover", def)
		}
	}

	// Extra denies: user denies and defaults all present, no dupes.
	spec = &zabbixv1alpha1.MCPSpec{DenyTools: []string{"host_get", "macro_get"}}
	envs = mcpToolEnvs(spec)
	counts := map[string]int{}
	for _, e := range envs {
		if strings.HasPrefix(e.Name, "ZBX_DENYTOOL_") {
			counts[e.Value]++
		}
	}
	for _, tool := range append(defaultMCPDenyTools, "host_get") {
		if counts[tool] != 1 {
			t.Errorf("tool %s denied %d times, want exactly once", tool, counts[tool])
		}
	}
	if counts["host_get"] != 1 {
		t.Error("user deny host_get missing")
	}
}

// TestMCPServiceContract verifies the default ClusterIP exposure and
// the port shape.
func TestMCPServiceContract(t *testing.T) {
	svc := MCPService(testZabbixWithMCP())
	if svc.Spec.Type != corev1.ServiceTypeClusterIP && svc.Spec.Type != "" {
		t.Errorf("service type %q, want empty/ClusterIP by default", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != MCPPort {
		t.Errorf("service ports %+v, want a single MCP port %d", svc.Spec.Ports, MCPPort)
	}
	nodePort := corev1.ServiceTypeNodePort
	z := testZabbixWithMCP()
	z.Spec.AgentIntegration.MCP.Service = &zabbixv1alpha1.FrontendServiceSpec{Type: &nodePort}
	if got := MCPService(z).Spec.Type; got != nodePort {
		t.Errorf("service type override = %q, want %q", got, nodePort)
	}
}
