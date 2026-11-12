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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ZabbixProxyGroupSpec defines the desired state of ZabbixProxyGroup.
// A ZabbixProxyGroup deploys a group of Zabbix proxies registered in a
// Zabbix server's proxy group (load balancing and high availability of
// proxying, per the Zabbix distributed monitoring model). Proxies in a
// group use the embedded SQLite3 database and register themselves with
// the server addressed by serverRef.
type ZabbixProxyGroupSpec struct {
	// version is the Zabbix release to deploy (e.g. "8.0" or "7.0"),
	// normally matching the Zabbix server it registers with.
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+$`
	// +required
	Version string `json:"version"`

	// groupName is the Zabbix proxy group these proxies join. The group
	// must exist in the server's configuration (managed via the Zabbix
	// API by the operator when the proxy group is referenced by a Zabbix
	// resource, or created manually otherwise).
	// +required
	GroupName string `json:"groupName"`

	// serverRef selects the Zabbix resource (in the same namespace) whose
	// server these proxies register with.
	// +required
	ServerRef string `json:"serverRef"`

	// replicas is the number of proxies in the group.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// image overrides the proxy container image.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// resources for each proxy container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// failoverDelay is the period during which a group member must
	// communicate with the Zabbix server to be considered online (see the
	// official proxy_group failover_delay documentation). Time suffixes
	// (s, m, h) are supported; empty uses the server default (1m).
	// +kubebuilder:validation:Pattern=`^[0-9]+[smh]$`
	// +optional
	FailoverDelay string `json:"failoverDelay,omitempty"`

	// minOnline is the minimum number of online proxies required to keep
	// the proxy group online (see the official proxy_group min_online
	// documentation). Zero uses the server default (1).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +optional
	MinOnline int32 `json:"minOnline,omitempty"`

	// apm enables the OpenTelemetry (APM) collector on every group
	// member: proxies with APM support accept OTLP/gRPC traces, metrics
	// and logs and write them to the telemetry provider configured on
	// the referenced Zabbix resource (spec.telemetryStorage). Zabbix 8.0+
	// only; ignored on older versions.
	// +optional
	APM *ProxyAPMSpec `json:"apm,omitempty"`
}

// ProxyAPMSpec configures the OTLP/gRPC listener of the group members.
type ProxyAPMSpec struct {
	// listenPort is the OTLP/gRPC port the members listen on. Defaults
	// to 4317 (the OpenTelemetry default receiver port).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ListenPort *int32 `json:"listenPort,omitempty"`

	// tlsSecretRef optionally names a Secret with the listener TLS
	// material (keys ca.crt, tls.crt, tls.key by default; see the caKey,
	// certKey, keyKey overrides). The mounted files are passed through
	// the official ZBX_APMTLS* entrypoint interface.
	// +optional
	TLSSecretRef *ProxyAPMTLS `json:"tlsSecretRef,omitempty"`
}

// ProxyAPMTLS points at the listener TLS material inside one Secret.
type ProxyAPMTLS struct {
	// secretName is the Secret carrying the TLS material.
	// +required
	SecretName string `json:"secretName"`

	// caKey is the Secret key holding the CA certificate. Defaults to
	// "ca.crt". Omit (empty) for a non-TLS listener.
	// +optional
	CAKey string `json:"caKey,omitempty"`

	// certKey is the Secret key holding the listener certificate.
	// Defaults to "tls.crt".
	// +optional
	CertKey string `json:"certKey,omitempty"`

	// keyKey is the Secret key holding the listener private key.
	// Defaults to "tls.key".
	// +optional
	KeyKey string `json:"keyKey,omitempty"`
}

// ZabbixProxyGroupStatus defines the observed state of ZabbixProxyGroup.
type ZabbixProxyGroupStatus struct {
	// observedGeneration is the generation most recently observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the ZabbixProxyGroup
	// resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// readyReplicas is the number of proxies currently ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// onlineReplicas is the number of the operator-managed member
	// proxies the Zabbix server currently counts as online: their last
	// contact with the server (proxy.lastaccess) is inside the group's
	// failover window — the official per-member availability datum,
	// since proxy objects expose no dedicated online flag.
	// +optional
	OnlineReplicas int32 `json:"onlineReplicas,omitempty"`

	// groupState is the observed state of the Zabbix proxy group as
	// reported by the server (unknown/offline/recovering/online/degrading,
	// per the official proxygroup state documentation). Empty while the
	// group has not been resolved through the API yet.
	// +optional
	GroupState string `json:"groupState,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName={zbxpg}
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupName`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Online",type=integer,JSONPath=`.status.onlineReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZabbixProxyGroup is the Schema for the zabbixproxygroups API.
type ZabbixProxyGroup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ZabbixProxyGroup
	// +required
	Spec ZabbixProxyGroupSpec `json:"spec"`

	// status defines the observed state of ZabbixProxyGroup
	// +optional
	Status ZabbixProxyGroupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ZabbixProxyGroupList contains a list of ZabbixProxyGroup
type ZabbixProxyGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ZabbixProxyGroup `json:"items"`
}

// ProxyGroupConditionType is a custom condition type on the
// ZabbixProxyGroup resource.
type ProxyGroupConditionType string

const (
	// ConditionProxyGroupReady indicates the member proxies are deployed
	// and the group is registered server-side through the API.
	ConditionProxyGroupReady ProxyGroupConditionType = "Ready"

	// ConditionProxyGroupServerResolved indicates the referenced Zabbix
	// resource exists and provides the server wiring.
	ConditionProxyGroupServerResolved ProxyGroupConditionType = "ServerResolved"

	// ConditionProxyGroupAPIReady indicates the API-maintained server
	// side of the group (group object and member proxy records) is in
	// sync.
	ConditionProxyGroupAPIReady ProxyGroupConditionType = "APIReady"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ZabbixProxyGroup{}, &ZabbixProxyGroupList{})
		return nil
	})
}
