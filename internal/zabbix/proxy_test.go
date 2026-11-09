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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Shared test literals — constants so repeated strings satisfy goconst
// and intent stays explicit.
const (
	testPgNamespace   = "monitoring"
	testProxyName     = "pg-proxy"
	testFailoverDelay = "30s"
)

func testProxyGroup() *zabbixv1alpha1.ZabbixProxyGroup {
	return &zabbixv1alpha1.ZabbixProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testPgNamespace},
		Spec: zabbixv1alpha1.ZabbixProxyGroupSpec{
			Version:   testVersion70,
			GroupName: "edge-group",
			ServerRef: "zbx",
		},
	}
}

func testServerRefResource() *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: "zbx", Namespace: testPgNamespace},
		Spec:       zabbixv1alpha1.ZabbixSpec{Version: testVersion70},
	}
}

func TestDesiredProxyReplicas(t *testing.T) {
	pg := testProxyGroup()
	if got := DesiredProxyReplicas(pg); got != 1 {
		t.Errorf("replicas = %d, want 1", got)
	}
	pg.Spec.Replicas = ptr.To[int32](3)
	if got := DesiredProxyReplicas(pg); got != 3 {
		t.Errorf("replicas = %d, want 3", got)
	}
}

func TestProxyRecordNameAndAddress(t *testing.T) {
	if got := ProxyRecordName("pg", 2); got != "pg-proxy-2" {
		t.Errorf("record name = %q", got)
	}
	if got := ProxyRecordAddress("pg", 0, testPgNamespace); got != "pg-proxy-0.pg-proxy.monitoring.svc" {
		t.Errorf("record address = %q", got)
	}
}

func TestProxyStatefulSetBuilder(t *testing.T) {
	pg := testProxyGroup()
	z := testServerRefResource()
	sts := ProxyStatefulSet(pg, 2, z)

	if sts.Name != testProxyName || sts.Namespace != testPgNamespace {
		t.Errorf("statefulset = %s/%s", sts.Namespace, sts.Name)
	}
	if sts.Spec.ServiceName != testProxyName {
		t.Errorf("serviceName = %q", sts.Spec.ServiceName)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 {
		t.Errorf("replicas = %v", sts.Spec.Replicas)
	}
	if sts.Spec.Selector == nil || sts.Spec.Selector.MatchLabels[ComponentLabel] != ComponentProxy ||
		sts.Spec.Selector.MatchLabels[InstanceLabel] != "pg" {
		t.Errorf("selector = %+v", sts.Spec.Selector)
	}

	c := sts.Spec.Template.Spec.Containers[0]
	if c.Name != "zabbix-proxy" {
		t.Errorf("container name = %q", c.Name)
	}
	if got := imageOrPanic(c.Image); got != "zabbix/zabbix-proxy-sqlite3:ubuntu-7.0-latest" {
		t.Errorf("image = %q", got)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["ZBX_PROXYMODE"] != "0" {
		t.Errorf("ZBX_PROXYMODE = %q, want active mode 0", env["ZBX_PROXYMODE"])
	}
	if env["ZBX_SERVER_HOST"] != "zbx-server-active.monitoring.svc" {
		t.Errorf("ZBX_SERVER_HOST = %q", env["ZBX_SERVER_HOST"])
	}
	if env["ZBX_SERVER_PORT"] != "10051" {
		t.Errorf("ZBX_SERVER_PORT = %q", env["ZBX_SERVER_PORT"])
	}
	// Identity: ZBX_HOSTNAME must come from the downward API (the pod
	// name), and HostnameItem must be absent (the 8.0 image combo of
	// empty Hostname + HostnameItem is rejected by the daemon).
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "ZBX_HOSTNAME" {
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.name" {
				t.Errorf("ZBX_HOSTNAME = %+v, want fieldRef metadata.name", e)
			}
		}
		if e.Name == "ZBX_HOSTNAMEITEM" {
			t.Errorf("ZBX_HOSTNAMEITEM must not be set")
		}
	}
	if env["ZBX_USE_NODE_NAME_AS_DB_NAME"] != "true" {
		t.Errorf("ZBX_USE_NODE_NAME_AS_DB_NAME = %q", env["ZBX_USE_NODE_NAME_AS_DB_NAME"])
	}

	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != ProxyPort {
		t.Errorf("ports = %+v", c.Ports)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil ||
		c.ReadinessProbe.Exec.Command[1] != "zabbix_proxy" {
		t.Errorf("readiness probe = %+v", c.ReadinessProbe)
	}
}

// imageOrPanic guards the image assertion failure message.
func imageOrPanic(img string) string { return img }

func TestProxyHeadlessService(t *testing.T) {
	pg := testProxyGroup()
	svc := ProxyHeadlessService(pg)

	if svc.Name != testProxyName || svc.Namespace != testPgNamespace {
		t.Errorf("service = %s/%s", svc.Namespace, svc.Name)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("clusterIP = %q, want None (per-ordinal DNS only)", svc.Spec.ClusterIP)
	}
	if svc.Spec.Selector[ComponentLabel] != ComponentProxy || svc.Spec.Selector[InstanceLabel] != "pg" {
		t.Errorf("selector = %+v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != ProxyPort {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
}

func TestDesiredProxyGroupSettings(t *testing.T) {
	pg := testProxyGroup()
	failoverDelay, minOnline := DesiredProxyGroupSettings(pg)
	if failoverDelay != "1m" || minOnline != "1" {
		t.Errorf("settings = %q/%q, want server defaults 1m/1", failoverDelay, minOnline)
	}

	pg.Spec.FailoverDelay = testFailoverDelay
	pg.Spec.MinOnline = 2
	failoverDelay, minOnline = DesiredProxyGroupSettings(pg)
	if failoverDelay != testFailoverDelay || minOnline != "2" {
		t.Errorf("settings = %q/%q, want 30s/2", failoverDelay, minOnline)
	}
}

// The membership records must stay addressable through the headless DNS,
// which is why the builder is a StatefulSet: compile-time guards for the
// types used in the reconcile pass.
var _ = appsv1.SchemeGroupVersion
var _ = corev1.ServiceSpec{}
