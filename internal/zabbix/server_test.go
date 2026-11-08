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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

func envByName(envs []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func envIndex(envs []corev1.EnvVar, name string) int {
	for i, e := range envs {
		if e.Name == name {
			return i
		}
	}
	return -1
}

func testZabbix() *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: "test-zabbix", Namespace: "test-ns", UID: "uid-1"},
		Spec: zabbixv1alpha1.ZabbixSpec{
			Version: testVersion80,
			Database: zabbixv1alpha1.DatabaseSpec{
				CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: testClusterName},
			},
		},
	}
}

func testDB() *DatabaseEnv {
	return &DatabaseEnv{
		Host: "zabbix-db-rw", Port: "5432", User: testZabbixUser, Password: testPassword, DBName: testZabbixUser,
	}
}

func TestServerStatefulSetStandalone(t *testing.T) {
	z := testZabbix()
	sts := ServerStatefulSet(z, testDB())

	if *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != ServerName(z.Name) {
		t.Errorf("serviceName = %q, want %q (governing service)", sts.Spec.ServiceName, ServerName(z.Name))
	}
	if sts.Spec.Selector.MatchLabels[ComponentLabel] != ComponentServer {
		t.Errorf("selector = %v, want the component selector", sts.Spec.Selector)
	}

	podLabels := sts.Spec.Template.Labels
	if podLabels[HARoleLabel] != HARoleActive {
		t.Errorf("standalone pod labels must mark the pod %s=%s, got %v", HARoleLabel, HARoleActive, podLabels)
	}

	server := sts.Spec.Template.Spec.Containers[0]
	for _, ha := range []string{"ZBX_AUTOHANODENAME", "ZBX_AUTONODEADDRESS", "ZBX_NODEADDRESSPORT"} {
		if _, ok := envByName(server.Env, ha); ok {
			t.Errorf("standalone server must not set %s", ha)
		}
	}
	if got := server.Image; got != "zabbix/zabbix-server-pgsql:ubuntu-8.0-latest" {
		t.Errorf("server image = %q", got)
	}

	// Agent 2 sidecar on by default.
	if len(sts.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("container count = %d, want 2 (server + agent2)", len(sts.Spec.Template.Spec.Containers))
	}
}

func TestServerStatefulSetHA(t *testing.T) {
	z := testZabbix()
	z.Spec.Server.Replicas = ptr.To[int32](3)
	sts := ServerStatefulSet(z, testDB())

	if *sts.Spec.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", *sts.Spec.Replicas)
	}
	if sts.Spec.Template.Labels[HARoleLabel] != "" {
		t.Errorf("HA pod template must not carry the HA-role label (it is maintained per pod), got %v", sts.Spec.Template.Labels)
	}

	server := sts.Spec.Template.Spec.Containers[0]
	env, _ := envByName(server.Env, "ZBX_AUTOHANODENAME")
	if env.Value != "hostname" {
		t.Errorf("ZBX_AUTOHANODENAME = %q, want hostname", env.Value)
	}
	env, _ = envByName(server.Env, "ZBX_AUTONODEADDRESS")
	if env.Value != "hostname" {
		t.Errorf("ZBX_AUTONODEADDRESS = %q, want hostname", env.Value)
	}
	env, _ = envByName(server.Env, "ZBX_NODEADDRESSPORT")
	if env.Value != "10051" {
		t.Errorf("ZBX_NODEADDRESSPORT = %q, want 10051", env.Value)
	}
}

func TestServerAgentDisabled(t *testing.T) {
	z := testZabbix()
	z.Spec.Server.Agent = &zabbixv1alpha1.AgentSidecarSpec{Enabled: new(false)}
	sts := ServerStatefulSet(z, testDB())
	if len(sts.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("container count = %d, want 1 with agent disabled", len(sts.Spec.Template.Spec.Containers))
	}
}

func TestAgentEnvOrdering(t *testing.T) {
	// $(POD_NAME) expansion only sees variables defined earlier, so
	// POD_NAME must precede ZBX_HOSTNAME.
	z := testZabbix()
	sts := ServerStatefulSet(z, testDB())
	var agent *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == agentContainerName {
			agent = &sts.Spec.Template.Spec.Containers[i]
		}
	}
	if agent == nil {
		t.Fatal("agent2 container missing")
	}
	podIdx := envIndex(agent.Env, "POD_NAME")
	hostIdx := envIndex(agent.Env, "ZBX_HOSTNAME")
	if podIdx == -1 || hostIdx == -1 || podIdx > hostIdx {
		t.Errorf("POD_NAME must be defined before ZBX_HOSTNAME, got order: %+v", agent.Env)
	}

	// Regression: Kubernetes injects ZBX_SERVER_PORT=tcp://<ip>:<port>
	// for a Service named <instance>-server namespace-wide; the agent
	// entrypoint composes ServerActive from it, so the container must
	// define it explicitly.
	portEnv, ok := envByName(agent.Env, "ZBX_SERVER_PORT")
	if !ok || portEnv.Value != "10051" {
		t.Errorf("agent must define ZBX_SERVER_PORT explicitly, got %+v", portEnv)
	}
}

func TestWebServiceEnv(t *testing.T) {
	z := testZabbix()
	z.Spec.WebService = &zabbixv1alpha1.WebServiceSpec{}

	sts := ServerStatefulSet(z, testDB())
	server := sts.Spec.Template.Spec.Containers[0]
	env, _ := envByName(server.Env, "ZBX_WEBSERVICEURL")
	if env.Value != "http://test-zabbix-web-service:10053/report" {
		t.Errorf("ZBX_WEBSERVICEURL = %q, want http://test-zabbix-web-service:10053/report", env.Value)
	}
	if _, ok := envByName(server.Env, "ZBX_STARTREPORTWRITERS"); !ok {
		t.Error("ZBX_STARTREPORTWRITERS missing when web service is deployed")
	}

	noWS := ServerStatefulSet(testZabbix(), testDB())
	if _, ok := envByName(noWS.Spec.Template.Spec.Containers[0].Env, "ZBX_WEBSERVICEURL"); ok {
		t.Error("ZBX_WEBSERVICEURL must not be set when no web service is configured")
	}
}

func TestServerServices(t *testing.T) {
	z := testZabbix()

	svc := ServerService(z)
	if svc.Name != "test-zabbix-server" {
		t.Errorf("server service name = %q", svc.Name)
	}
	if svc.Spec.Selector[ComponentLabel] != ComponentServer || svc.Spec.Selector[InstanceLabel] != z.Name {
		t.Errorf("server service selector = %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != ServerPort {
		t.Errorf("server service ports = %+v", svc.Spec.Ports)
	}

	active := ActiveServerService(z)
	if active.Name != "test-zabbix-server-active" {
		t.Errorf("active service name = %q", active.Name)
	}
	if active.Spec.Selector[HARoleLabel] != HARoleActive {
		t.Errorf("active service selector must include %s=%s, got %v", HARoleLabel, HARoleActive, active.Spec.Selector)
	}
}
