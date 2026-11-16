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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

const (
	portNameMCP = "mcp"

	// mcpServerUID is the numeric uid the official zabbix-mcp-server
	// image declares (USER 1997). Pinned so runAsNonRoot is verifiable.
	mcpServerUID = int64(1997)
)

// SupportsAgentIntegration reports whether the Zabbix release ships an
// MCP server (8.0+; there is no MCP server in 7.0).
func SupportsAgentIntegration(version string) bool {
	major, ok := majorVersion(version)
	return ok && major >= 8
}

// MCPEnabled reports whether the agent-integration MCP server is
// requested by the spec (the presence of the optional block enables it).
func MCPEnabled(z *zabbixv1alpha1.Zabbix) bool {
	return z.Spec.AgentIntegration != nil && z.Spec.AgentIntegration.MCP != nil
}

// MCPImage returns the MCP server container image (official
// zabbix/zabbix-mcp-server, tagged with the instance's spec.version).
func MCPImage(z *zabbixv1alpha1.Zabbix) string {
	spec := z.Spec.AgentIntegration.MCP
	return ImageFor("mcp-server", z.Spec.Version, spec.Image)
}

// defaultMCPDenyTools is the tool access policy the official image
// ships (the docker-entrypoint env_vars reference them as built into
// the default configuration). Setting ANY indexed tool-rule environment
// variable replaces that default policy entirely, so the operator
// re-renders these denies behind the user's rules — see mcpToolEnvs.
var defaultMCPDenyTools = []string{
	"item_get", "lld_get", "interface_get", "macro_get", "history_get",
}

// MCPDeployment builds the MCP server Deployment: a single stateless
// replica whose only wiring is the frontend URL and the user-supplied
// access policy — the server authenticates callers through the API
// token each request carries, so no Secret is provisioned.
func MCPDeployment(z *zabbixv1alpha1.Zabbix) *appsv1.Deployment {
	spec := z.Spec.AgentIntegration.MCP

	env := []corev1.EnvVar{
		// The MCP server reaches the Zabbix API through the instance's
		// own frontend Service (same always-own-frontend pattern as the
		// API client and the web service; the endpoint URL must end in
		// api_jsonrpc.php).
		{Name: "ZBX_FRONTEND_URL", Value: APIEndpoint(z)},
	}
	if ips := spec.AllowedIPs; len(ips) > 0 {
		env = append(env, corev1.EnvVar{Name: "ZBX_ALLOWEDIP", Value: strings.Join(ips, ",")})
	}
	env = append(env, mcpToolEnvs(spec)...)
	if spec.ToolPrefix != "" {
		env = append(env, corev1.EnvVar{Name: "ZBX_TOOL_PREFIX", Value: spec.ToolPrefix})
	}

	return deployment(workloadScaffold{
		name:       MCPServerName(z.Name),
		namespace:  z.Namespace,
		prefix:     z.Name,
		component:  ComponentMCPServer,
		replicas:   1,
		containers: mcpContainers(z, env),
	})
}

// mcpContainers assembles the MCP server container: the official image
// runs as a numeric non-root user, so the full restricted container
// context applies (the e2e suite is the gate).
func mcpContainers(z *zabbixv1alpha1.Zabbix, env []corev1.EnvVar) []corev1.Container {
	image := MCPImage(z)
	return []corev1.Container{{
		Name:            "zabbix-mcp-server",
		Image:           image,
		ImagePullPolicy: pullPolicy(z.Spec.AgentIntegration.MCP.Image),
		SecurityContext: RestrictedSecurityContext(mcpServerUID),
		Env:             env,
		Ports: []corev1.ContainerPort{
			{Name: portNameMCP, ContainerPort: MCPPort},
		},
		// The transport answers 401 without a token (and 403 for
		// sources outside AllowedIP), not 200 — a TCP probe is the
		// only reliable liveness signal without holding a token.
		ReadinessProbe: tcpProbe(MCPPort),
		LivenessProbe:  tcpProbe(MCPPort),
		Resources:      z.Spec.AgentIntegration.MCP.Resources,
	}}
}

// MCPService builds the MCP server Service (ClusterIP by default; the
// exposure type is an explicit opt-in like the frontend's).
func MCPService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	var svcType corev1.ServiceType
	var annotations map[string]string
	if spec := z.Spec.AgentIntegration.MCP.Service; spec != nil {
		if spec.Type != nil {
			svcType = *spec.Type
		}
		annotations = spec.Annotations
	}

	return service(serviceScaffold{
		name:        MCPServerName(z.Name),
		namespace:   z.Namespace,
		prefix:      z.Name,
		component:   ComponentMCPServer,
		svcType:     svcType,
		annotations: annotations,
		ports: []corev1.ServicePort{{
			Name:       portNameMCP,
			Port:       MCPPort,
			TargetPort: intstr.FromInt32(MCPPort),
		}},
	})
}

// mcpToolEnvs renders the tool access policy as the image's indexed
// environment variables. Semantics (official env_vars contract): setting
// ANY indexed rule variable replaces the image's default policy
// entirely, so the operator renders the default deny list itself.
// Rendering order = evaluation order (first match wins):
//
//  1. user allow rules — let an explicit allow beat the default deny
//     of the same tool (e.g. un-deny history_get);
//  2. user deny rules;
//  3. the image's default deny list, minus tools the user's rules
//     already decide (an allowed tool must not re-match a deny; a
//     re-denied tool is redundant).
//
// An empty policy renders nothing: the image's own default config stays
// in effect. The tool prefix is independent of the rules and is wired
// by MCPDeployment.
func mcpToolEnvs(spec *zabbixv1alpha1.MCPSpec) []corev1.EnvVar {
	if len(spec.AllowTools) == 0 && len(spec.DenyTools) == 0 {
		return nil
	}

	decided := map[string]bool{}
	for _, t := range spec.AllowTools {
		decided[t] = true
	}
	for _, t := range spec.DenyTools {
		decided[t] = true
	}

	var envs []corev1.EnvVar
	index := 0
	for _, tool := range spec.AllowTools {
		envs = append(envs, corev1.EnvVar{Name: "ZBX_ALLOWTOOL_" + strconv.Itoa(index), Value: tool})
		index++
	}
	for _, tool := range spec.DenyTools {
		envs = append(envs, corev1.EnvVar{Name: "ZBX_DENYTOOL_" + strconv.Itoa(index), Value: tool})
		index++
	}
	for _, tool := range defaultMCPDenyTools {
		if decided[tool] {
			continue
		}
		envs = append(envs, corev1.EnvVar{Name: "ZBX_DENYTOOL_" + strconv.Itoa(index), Value: tool})
		index++
	}

	return envs
}
