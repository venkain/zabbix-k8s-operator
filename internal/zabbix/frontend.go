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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// portNameHTTP names the frontend and web service HTTP ports (container
// and service port lists).
const portNameHTTP = "http"

// FrontendDeployment builds the Zabbix frontend (nginx) Deployment. The
// frontend is stateless; it talks to the database directly and to the
// server through the active-node Service.
func FrontendDeployment(z *zabbixv1alpha1.Zabbix, db *DatabaseEnv) *appsv1.Deployment {
	spec := z.Spec.Frontend
	replicas := int32(1)
	if spec != nil && spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	podLabels := commonLabels(z.Name, ComponentFrontend)

	env := append(db.FrontendEnvVars(), ServerEnvVarsForFrontend(z)...)
	if spec != nil {
		env = append(env, spec.Env...)
	}
	// ClickHouse history/telemetry wiring for the frontend (Zabbix 8.0+;
	// no-op when unset or on older versions). See telemetry.go.
	env = append(env, FrontendTelemetryEnvs(z)...)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      FrontendName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentFrontend)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "zabbix-frontend",
						Image:           ImageFor("web-nginx-pgsql", z.Spec.Version, frontendImage(spec)),
						ImagePullPolicy: pullPolicy(frontendImage(spec)),
						Env:             env,
						Ports: []corev1.ContainerPort{
							{Name: portNameHTTP, ContainerPort: FrontendPort},
						},
						ReadinessProbe: httpProbe("/"),
						LivenessProbe:  httpProbe("/"),
						Resources:      frontendResources(spec),
						VolumeMounts:   TelemetryVolumeMounts(z),
					}},
					Volumes: TelemetrySecretVolumes(z),
				},
			},
		},
	}
}

// FrontendService builds the frontend Service (the user-facing entry).
// ClusterIP by default; the spec may opt into NodePort/LoadBalancer
// exposure and add Service annotations (e.g. cloud LB settings). Ingress
// routing on top stays user-managed.
func FrontendService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	svcType := corev1.ServiceTypeClusterIP
	var annotations map[string]string
	if spec := z.Spec.Frontend; spec != nil && spec.Service != nil {
		if spec.Service.Type != nil {
			svcType = *spec.Service.Type
		}
		annotations = spec.Service.Annotations
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        FrontendName(z.Name),
			Namespace:   z.Namespace,
			Labels:      commonLabels(z.Name, ComponentFrontend),
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: selectorLabels(z.Name, ComponentFrontend),
			Ports: []corev1.ServicePort{{
				Name:       portNameHTTP,
				Port:       80,
				TargetPort: intstr.FromInt32(FrontendPort),
			}},
		},
	}
}

// WebServiceDeployment builds the Zabbix web service Deployment (used by
// the server for scheduled reports). The server reaches it via the
// WebServiceURL config fragment built by WebServiceConfigFragment.
func WebServiceDeployment(z *zabbixv1alpha1.Zabbix) *appsv1.Deployment {
	spec := z.Spec.WebService
	replicas := int32(1)
	if spec != nil && spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	podLabels := commonLabels(z.Name, ComponentWebService)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebServiceName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentWebService)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "zabbix-web-service",
						Image:           ImageFor("web-service", z.Spec.Version, webServiceImage(spec)),
						ImagePullPolicy: pullPolicy(webServiceImage(spec)),
						Ports: []corev1.ContainerPort{
							{Name: portNameHTTP, ContainerPort: WebServicePort},
						},
						ReadinessProbe: tcpProbe(WebServicePort),
						Resources:      webServiceResources(spec),
					}},
				},
			},
		},
	}
}

// WebServiceService builds the Service fronting the web service.
func WebServiceService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebServiceName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentWebService),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(z.Name, ComponentWebService),
			Ports: []corev1.ServicePort{{
				Name:       portNameHTTP,
				Port:       WebServicePort,
				TargetPort: intstr.FromInt32(WebServicePort),
			}},
		},
	}
}

func frontendImage(spec *zabbixv1alpha1.FrontendSpec) *zabbixv1alpha1.ImageSpec {
	if spec == nil {
		return nil
	}
	return spec.Image
}

func frontendResources(spec *zabbixv1alpha1.FrontendSpec) corev1.ResourceRequirements {
	if spec == nil {
		return corev1.ResourceRequirements{}
	}
	return spec.Resources
}

func webServiceImage(spec *zabbixv1alpha1.WebServiceSpec) *zabbixv1alpha1.ImageSpec {
	if spec == nil {
		return nil
	}
	return spec.Image
}

func webServiceResources(spec *zabbixv1alpha1.WebServiceSpec) corev1.ResourceRequirements {
	if spec == nil {
		return corev1.ResourceRequirements{}
	}
	return spec.Resources
}

func httpProbe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(FrontendPort)},
		},
		InitialDelaySeconds: 20,
		PeriodSeconds:       10,
	}
}
