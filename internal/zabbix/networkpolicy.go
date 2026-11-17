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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// NetworkPolicy hardening (spec.hardening.networkPolicy, default on):
// default-deny egress for the auxiliary workloads the operator deploys
// with an allowlist of exactly the connections they legitimately make.
// The data-plane components (server, frontend, proxies, ClickHouse) are
// deliberately NOT policy-restricted here: their traffic graph includes
// user-defined targets (proxies reach arbitrary agents, the server
// reaches arbitrary monitored endpoints), so a generated policy would
// either break monitoring or be too wide to be worth having.

const (
	// KSMNetworkPolicySuffix names the kube-state-metrics egress
	// policy (<ksm workload name> + suffix).
	KSMNetworkPolicySuffix = "-egress"

	// MCPNetworkPolicySuffix names the MCP server policy
	// (<mcp workload name> + suffix).
	MCPNetworkPolicySuffix = "-policy"

	// kubeDNSNamespaceSelector and kubeDNSPodSelector select the
	// cluster DNS backend (CoreDNS/kube-dns carry k8s-app=kube-dns on
	// kube-system in every conforming cluster; the namespaceSelector
	// uses the standard automatic metadata.name label).
	kubeDNSNamespace = "kube-system"
	kubeDNSPodLabel  = "k8s-app"
	kubeDNSPodValue  = "kube-dns"

	// apiServerEgressCIDR is the egress allowance covering the
	// Kubernetes API-server path. NetworkPolicies cannot select the
	// API server by service/pod labels (it is not a pod), and the
	// operator does not know the cluster's service CIDR — so the
	// HTTPS egress lane stays destination-open. Port-restricted.
	// BOTH ports are required by evaluation semantics: policy agents
	// see the packet AFTER the service-proxy DNAT, so the port is the
	// BACKEND's — 443 when the API server runs as a normal pod behind
	// the kubernetes Service, 6443 where it is host-networked (kind/
	// kubeadm static pods). Verified against kind's Kindnet: 443-only
	// times out, 443+6443 connects.
	apiServerEgressCIDR = "0.0.0.0/0"

	apiServerEgressPortService = 443
	apiServerEgressPortBackend = 6443
)

// KSMNetworkPolicyName returns the kube-state-metrics egress policy
// name for an instance name prefix.
func KSMNetworkPolicyName(instance string) string {
	return KubeStateMetricsName(instance) + KSMNetworkPolicySuffix
}

// MCPNetworkPolicyName returns the MCP server policy name.
func MCPNetworkPolicyName(instance string) string {
	return MCPServerName(instance) + MCPNetworkPolicySuffix
}

// dnsEgressRule allows DNS to the cluster DNS backend.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	port53 := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": kubeDNSNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{kubeDNSPodLabel: kubeDNSPodValue},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: new(corev1.ProtocolUDP), Port: &port53},
			{Protocol: new(corev1.ProtocolTCP), Port: &port53},
		},
	}
}

// KSMNetworkPolicy builds the kube-state-metrics egress policy. KSM's
// only legitimate connections are the cluster DNS and the Kubernetes
// API server (it watches cluster state through the API): everything
// else is denied. The API-server lane is destination-open — see
// apiServerEgressCIDR. Ingress is left unrestricted (the Zabbix server
// scrapes the metrics endpoint THROUGH the Kubernetes API proxy, so
// the source of the scrape traffic is the API server, not a
// selectable pod; an ingress rule would break the scrape path).
func KSMNetworkPolicy(z *zabbixv1alpha1.Zabbix) *networkingv1.NetworkPolicy {
	prefix := clusterMetricsNamePrefixOf(z)
	portService := intstr.FromInt32(apiServerEgressPortService)
	portBackend := intstr.FromInt32(apiServerEgressPortBackend)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KSMNetworkPolicyName(prefix),
			Namespace: clusterMetricsNamespaceOf(z),
			Labels:    commonLabels(prefix, ComponentClusterMetrics),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(prefix, ComponentClusterMetrics),
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: apiServerEgressCIDR},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: new(corev1.ProtocolTCP), Port: &portService},
						{Protocol: new(corev1.ProtocolTCP), Port: &portBackend},
					},
				},
			},
		},
	}
}

// MCPNetworkPolicy builds the MCP server policy: ingress on the MCP
// port only (agent clients may come from anywhere in the cluster),
// egress restricted to the cluster DNS and the instance's own frontend
// pods (the server's only outbound dependency — the Zabbix API it
// serves calls). Everything else is denied in both directions.
func MCPNetworkPolicy(z *zabbixv1alpha1.Zabbix) *networkingv1.NetworkPolicy {
	mcpPort := intstr.FromInt32(MCPPort)
	frontendPort := intstr.FromInt32(FrontendPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MCPNetworkPolicyName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentMCPServer),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: selectorLabels(z.Name, ComponentMCPServer),
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: new(corev1.ProtocolTCP), Port: &mcpPort},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: selectorLabels(z.Name, ComponentFrontend),
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: new(corev1.ProtocolTCP), Port: &frontendPort},
					},
				},
			},
		},
	}
}
