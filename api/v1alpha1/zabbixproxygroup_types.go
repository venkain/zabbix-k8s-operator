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
	Resources ResourceRequirementsSpec `json:"resources,omitempty"`
}

// ResourceRequirementsSpec mirrors corev1.ResourceRequirements to avoid
// leaking the full core type into the spec surface.
// +kubebuilder:object:generate=true
type ResourceRequirementsSpec struct {
	// +optional
	Requests map[string]string `json:"requests,omitempty"`

	// +optional
	Limits map[string]string `json:"limits,omitempty"`
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName={zbxpg}
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupName`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
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

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ZabbixProxyGroup{}, &ZabbixProxyGroupList{})
		return nil
	})
}
