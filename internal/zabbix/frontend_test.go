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
	"testing"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

func TestFrontendDeployment(t *testing.T) {
	z := testZabbix()
	z.Spec.Frontend = &zabbixv1alpha1.FrontendSpec{
		Replicas: ptr.To[int32](2),
		Env:      []corev1.EnvVar{{Name: "PHP_MAX_EXECUTION_TIME", Value: "300"}},
	}
	dep := FrontendDeployment(z, testDB())

	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *dep.Spec.Replicas)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "zabbix/zabbix-web-nginx-pgsql:ubuntu-8.0-latest" {
		t.Errorf("frontend image = %q", got)
	}

	env := dep.Spec.Template.Spec.Containers[0].Env
	if _, ok := envByName(env, "DB_SERVER_HOST"); !ok {
		t.Error("DB_SERVER_HOST missing from frontend env")
	}
	envServer, _ := envByName(env, "ZBX_SERVER_HOST")
	if envServer.Value != "test-zabbix-server-active" {
		t.Errorf("frontend must reach the active-node service, got %q", envServer.Value)
	}
	if extra, ok := envByName(env, "PHP_MAX_EXECUTION_TIME"); !ok || extra.Value != "300" {
		t.Error("user-provided frontend env not carried through")
	}
}

func TestFrontendService(t *testing.T) {
	svc := FrontendService(testZabbix())
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %q", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != int(FrontendPort) {
		t.Errorf("frontend service ports = %+v, want port 80 -> 8080", svc.Spec.Ports)
	}
	if len(svc.Annotations) != 0 {
		t.Errorf("annotations = %v, want none by default", svc.Annotations)
	}
}

func TestFrontendServiceExposure(t *testing.T) {
	z := testZabbix()
	z.Spec.Frontend = &zabbixv1alpha1.FrontendSpec{
		Service: &zabbixv1alpha1.FrontendServiceSpec{
			Type: ptr.To[corev1.ServiceType](corev1.ServiceTypeNodePort),
			Annotations: map[string]string{
				"example.com/lb-config": "value",
			},
		},
	}
	svc := FrontendService(z)

	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("service type = %q, want NodePort", svc.Spec.Type)
	}
	if svc.Annotations["example.com/lb-config"] != "value" {
		t.Errorf("annotations = %v, want the spec annotations", svc.Annotations)
	}
	// The NodePort is cluster-allocated: the builder must not pin it.
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].NodePort != 0 {
		t.Errorf("nodePort must not be pinned, got %+v", svc.Spec.Ports[0])
	}
}

func TestWebServiceDeploymentAndService(t *testing.T) {
	z := testZabbix()
	z.Spec.WebService = &zabbixv1alpha1.WebServiceSpec{}

	dep := WebServiceDeployment(z)
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "zabbix/zabbix-web-service:ubuntu-8.0-latest" {
		t.Errorf("web service image = %q", got)
	}
	ports := dep.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 1 || ports[0].ContainerPort != WebServicePort {
		t.Errorf("web service container ports = %+v, want %d", ports, WebServicePort)
	}

	svc := WebServiceService(z)
	if svc.Spec.Selector[ComponentLabel] != ComponentWebService {
		t.Errorf("web service selector = %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != WebServicePort {
		t.Errorf("web service ports = %+v", svc.Spec.Ports)
	}
}
