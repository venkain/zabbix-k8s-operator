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
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// portNameHTTP names every plain-HTTP service port (frontend, web
// service, KSM, ClickHouse SQL) in container and service port lists.
const portNameHTTP = "http"

// FrontendDeployment builds the Zabbix frontend (nginx) Deployment. The
// frontend is stateless; it talks to the database directly and to the
// server through the active-node Service.
func FrontendDeployment(z *zabbixv1alpha1.Zabbix, db *DatabaseEnv) *appsv1.Deployment {
	spec := z.Spec.Frontend
	replicas := FrontendReplicas(z)

	env := append(db.FrontendEnvVars(), ServerEnvVarsForFrontend(z)...)
	if spec != nil {
		env = append(env, spec.Env...)
	}
	// ClickHouse history/telemetry wiring for the frontend (Zabbix 8.0+;
	// no-op when unset or on older versions). See telemetry.go.
	env = append(env, FrontendTelemetryEnvs(z)...)

	return deployment(workloadScaffold{
		name:      FrontendName(z.Name),
		namespace: z.Namespace,
		prefix:    z.Name,
		component: ComponentFrontend,
		replicas:  replicas,
		containers: []corev1.Container{{
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
		volumes: TelemetrySecretVolumes(z),
	})
}

// FrontendService builds the frontend Service (the user-facing entry).
// ClusterIP by default; the spec may opt into NodePort/LoadBalancer
// exposure and add Service annotations (e.g. cloud LB settings). Ingress
// routing on top stays user-managed.
func FrontendService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	var svcType corev1.ServiceType
	var annotations map[string]string
	if spec := z.Spec.Frontend; spec != nil && spec.Service != nil {
		if spec.Service.Type != nil {
			svcType = *spec.Service.Type
		}
		annotations = spec.Service.Annotations
	}

	return service(serviceScaffold{
		name:        FrontendName(z.Name),
		namespace:   z.Namespace,
		prefix:      z.Name,
		component:   ComponentFrontend,
		svcType:     svcType,
		annotations: annotations,
		ports: []corev1.ServicePort{{
			Name:       portNameHTTP,
			Port:       frontendServicePort,
			TargetPort: intstr.FromInt32(FrontendPort),
		}},
	})
}

// WebServiceDeployment builds the Zabbix web service Deployment (used by
// the server for scheduled reports). The server reaches it via the
// WebServiceURL config fragment built by WebServiceConfigFragment.
func WebServiceDeployment(z *zabbixv1alpha1.Zabbix) *appsv1.Deployment {
	spec := z.Spec.WebService
	replicas := WebServiceReplicas(z)

	return deployment(workloadScaffold{
		name:      WebServiceName(z.Name),
		namespace: z.Namespace,
		prefix:    z.Name,
		component: ComponentWebService,
		replicas:  replicas,
		containers: []corev1.Container{{
			Name:            "zabbix-web-service",
			Image:           ImageFor("web-service", z.Spec.Version, webServiceImage(spec)),
			ImagePullPolicy: pullPolicy(webServiceImage(spec)),
			Ports: []corev1.ContainerPort{
				{Name: portNameHTTP, ContainerPort: WebServicePort},
			},
			ReadinessProbe: tcpProbe(WebServicePort),
			Resources:      webServiceResources(spec),
		}},
	})
}

// WebServiceService builds the Service fronting the web service.
func WebServiceService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	return service(serviceScaffold{
		name:      WebServiceName(z.Name),
		namespace: z.Namespace,
		prefix:    z.Name,
		component: ComponentWebService,
		ports: []corev1.ServicePort{{
			Name:       portNameHTTP,
			Port:       WebServicePort,
			TargetPort: intstr.FromInt32(WebServicePort),
		}},
	})
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
