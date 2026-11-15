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
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// workloadScaffold is the shared shape of every component workload the
// operator deploys; the per-component builders only assemble containers
// and component-specific knobs, while deployment/statefulSet wire the
// identical scaffold (meta, selector, pod template, pod security
// baseline).
type workloadScaffold struct {
	name      string
	namespace string
	// prefix is the label/selector prefix (the instance, proxy group, or
	// name-prefix override the workload belongs to).
	prefix    string
	component string

	replicas int32

	// serviceName is the StatefulSet's governing Service (ignored for
	// Deployments).
	serviceName string

	// extraLabels are merged into the object and pod-template labels
	// only (never the selector — e.g. the HA role stamp the operator
	// maintains per pod).
	extraLabels map[string]string

	// serviceAccountName is the pod's service account (set when the
	// workload runs under a non-default identity).
	serviceAccountName string

	containers []corev1.Container
	volumes    []corev1.Volume
	// claims is the StatefulSet's VolumeClaimTemplates (nil for
	// Deployments and stateless StatefulSets).
	claims []corev1.PersistentVolumeClaim
}

// workloadLabels merges the component's common labels with the
// scaffold's extra labels (object + pod template only, never selector).
func workloadLabels(w workloadScaffold) map[string]string {
	labels := commonLabels(w.prefix, w.component)
	maps.Copy(labels, w.extraLabels)
	return labels
}

// deployment builds the Deployment scaffold for the given workload.
func deployment(w workloadScaffold) *appsv1.Deployment {
	labels := workloadLabels(w)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.name,
			Namespace: w.namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &w.replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(w.prefix, w.component)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext:    PodSecurityContext(),
					ServiceAccountName: w.serviceAccountName,
					Containers:         w.containers,
					Volumes:            w.volumes,
				},
			},
		},
	}
}

// statefulSet builds the StatefulSet scaffold for the given workload.
func statefulSet(w workloadScaffold) *appsv1.StatefulSet {
	labels := workloadLabels(w)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.name,
			Namespace: w.namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &w.replicas,
			ServiceName: w.serviceName,
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(w.prefix, w.component)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: PodSecurityContext(),
					Containers:      w.containers,
					Volumes:         w.volumes,
				},
			},
			VolumeClaimTemplates: w.claims,
		},
	}
}

// serviceScaffold is the shared shape of every Service the operator
// deploys.
type serviceScaffold struct {
	name        string
	namespace   string
	prefix      string
	component   string
	ports       []corev1.ServicePort
	annotations map[string]string
	// extraLabels are merged into both the object labels and the pod
	// selector (e.g. the HA active-node role pin).
	extraLabels map[string]string
	// headless selects the ClusterIP: None governing service of a
	// StatefulSet; otherwise the Service is ClusterIP (or the explicit
	// exposure type).
	headless bool
	svcType  corev1.ServiceType // optional exposure override (NodePort, LoadBalancer)
}

// service builds the Service scaffold for the given descriptor.
func service(s serviceScaffold) *corev1.Service {
	labels := commonLabels(s.prefix, s.component)
	selector := selectorLabels(s.prefix, s.component)
	maps.Copy(labels, s.extraLabels)
	maps.Copy(selector, s.extraLabels)
	svcType := s.svcType
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	spec := corev1.ServiceSpec{
		Type:     svcType,
		Selector: selector,
		Ports:    s.ports,
	}
	if s.headless {
		spec.ClusterIP = "None"
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        s.name,
			Namespace:   s.namespace,
			Labels:      labels,
			Annotations: s.annotations,
		},
		Spec: spec,
	}
}
