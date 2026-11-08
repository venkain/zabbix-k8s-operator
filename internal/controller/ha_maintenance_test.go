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

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// Shared literals for the HA maintenance tests.
const (
	haTestVersion     = "8.0"
	haTestTokenSecret = "ha-api-token"
	haTestTokenKey    = "token"
	haTestTokenValue  = "secret-token"
	haTestNS          = "ha-ns"
	haTestPod0        = "ha-server-0"
	haTestPod1        = "ha-server-1"
)

// haTestScheme builds a scheme with all types the HA maintenance touches.
func haTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := zabbixv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// rewriteTransport redirects every request to the local haNodeStub,
// standing in for the frontend's JSON-RPC endpoint in tests.
type rewriteTransport struct{ to *httptest.Server }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = t.to.Listener.Addr().String()
	return http.DefaultTransport.RoundTrip(out)
}

// haNodeStub answers hanode.get with a recorded registry.
func haNodeStub(t *testing.T, wantAuth string, registry string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantAuth {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(registry))
	}))
}

// newHAZabbix returns an HA-mode Zabbix resource with API wiring.
func newHAZabbix(ns string) *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: "ha", Namespace: ns, UID: "uid-ha"},
		Spec: zabbixv1alpha1.ZabbixSpec{
			Version: haTestVersion,
			Database: zabbixv1alpha1.DatabaseSpec{
				CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "db"},
			},
			API: &zabbixv1alpha1.APISpec{
				TokenSecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: haTestTokenSecret},
					Key:                  haTestTokenKey,
				},
			},
			Server: zabbixv1alpha1.ServerSpec{Replicas: ptr.To[int32](2)},
		},
	}
}

var haLabRegistry = `{"jsonrpc":"2.0","result":[
	{"name":"ha-server-0","address":"10.0.0.1","port":"10051","lastaccess":"200","status":"3"},
	{"name":"ha-server-1","address":"10.0.0.2","port":"10051","lastaccess":"199","status":"0"}
],"id":1}`

func TestMaintainHARolesPatchesPodLabels(t *testing.T) {
	ctx := context.Background()
	ns := haTestNS
	z := newHAZabbix(ns)

	stub := haNodeStub(t, haTestTokenValue, haLabRegistry)
	defer stub.Close()

	seed := make([]client.Object, 0, 4)
	seed = append(seed,
		z,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: haTestTokenSecret, Namespace: ns},
			Data:       map[string][]byte{haTestTokenKey: []byte(haTestTokenValue)},
		})
	for _, i := range []string{"0", "1"} {
		seed = append(seed, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ha-server-" + i,
				Namespace: ns,
				Labels: map[string]string{
					zabbix.InstanceLabel:  z.Name,
					zabbix.ComponentLabel: zabbix.ComponentServer,
				},
			},
		})
	}
	c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(seed...).Build()

	r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t), HTTP: &http.Client{Transport: rewriteTransport{to: stub}}}
	var status zabbixv1alpha1.ZabbixStatus
	if err := r.maintainHARoles(ctx, z, &status, nil); err != nil {
		t.Fatalf("maintainHARoles: %v", err)
	}

	// Pod 0 took the active role, pod 1 stepped back to standby.
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Name: haTestPod0, Namespace: ns}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Labels[zabbix.HARoleLabel] != zabbix.HARoleActive {
		t.Errorf("pod 0 label = %q, want active", pod.Labels[zabbix.HARoleLabel])
	}
	if err := c.Get(ctx, client.ObjectKey{Name: haTestPod1, Namespace: ns}, &pod); err != nil {
		t.Fatal(err)
	}
	if got := pod.Labels[zabbix.HARoleLabel]; got != string(zabbix.HANodeStateStandby) {
		t.Errorf("pod 1 label = %q, want standby", got)
	}

	// Status carries the registry (with correct status mapping).
	if status.ServerHA == nil || status.ServerHA.ActiveNode != haTestPod0 {
		t.Fatalf("serverHA = %+v", status.ServerHA)
	}
}

func TestMaintainHARolesRemovesStaleLabel(t *testing.T) {
	ctx := context.Background()
	ns := haTestNS
	z := newHAZabbix(ns)

	// Registry does not know pod 1 (e.g. scaled down then up again):
	// its stale label must go.
	stub := haNodeStub(t, haTestTokenValue, `{"jsonrpc":"2.0","result":[
		{"name":"ha-server-0","address":"10.0.0.1","port":"10051","lastaccess":"200","status":"3"}
	],"id":1}`)
	defer stub.Close()

	c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(
		z,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: haTestTokenSecret, Namespace: ns},
			Data:       map[string][]byte{haTestTokenKey: []byte(haTestTokenValue)},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      haTestPod0,
				Namespace: ns,
				Labels:    map[string]string{zabbix.InstanceLabel: z.Name, zabbix.ComponentLabel: zabbix.ComponentServer},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      haTestPod1,
				Namespace: ns,
				Labels: map[string]string{
					zabbix.InstanceLabel:  z.Name,
					zabbix.ComponentLabel: zabbix.ComponentServer,
					zabbix.HARoleLabel:    "active",
				},
			},
		},
	).Build()

	r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t), HTTP: &http.Client{Transport: rewriteTransport{to: stub}}}
	var status zabbixv1alpha1.ZabbixStatus
	if err := r.maintainHARoles(ctx, z, &status, nil); err != nil {
		t.Fatalf("maintainHARoles: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Name: haTestPod1, Namespace: ns}, &pod); err != nil {
		t.Fatal(err)
	}
	if _, hasLabel := pod.Labels[zabbix.HARoleLabel]; hasLabel {
		t.Errorf("stale HA label survived: %v", pod.Labels)
	}
}

func TestMaintainHARolesFailsWithoutTokenSecret(t *testing.T) {
	ctx := context.Background()
	ns := haTestNS
	z := newHAZabbix(ns)

	c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(z).Build()
	r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t)}
	var status zabbixv1alpha1.ZabbixStatus
	if err := r.maintainHARoles(ctx, z, &status, nil); err == nil {
		t.Error("expected an error when the API token secret is missing")
	}
	if status.ServerHA != nil {
		t.Errorf("serverHA must stay empty on failure, got %+v", status.ServerHA)
	}
}

func TestMaintainHARolesFailsWhenAPIDown(t *testing.T) {
	ctx := context.Background()
	ns := haTestNS
	z := newHAZabbix(ns)

	stub := haNodeStub(t, haTestTokenValue, `{"jsonrpc":"2.0","error":{"message":"Service unavailable","data":"server is not running"},"id":1}`)
	c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(
		z,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: haTestTokenSecret, Namespace: ns},
			Data:       map[string][]byte{haTestTokenKey: []byte(haTestTokenValue)},
		},
	).Build()
	r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t), HTTP: &http.Client{Transport: rewriteTransport{to: stub}}}
	var status zabbixv1alpha1.ZabbixStatus
	if err := r.maintainHARoles(ctx, z, &status, nil); err == nil {
		t.Error("expected an error when the API reports an error")
	}
}

func TestMigrateLegacyServerDeploymentLifecycle(t *testing.T) {
	ctx := context.Background()
	ns := "mig-ns"
	z := newHAZabbix(ns)
	z.Name = "mig"

	legacyScale := func(replicas, status int32) *appsv1.Deployment {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      zabbix.ServerName(z.Name),
				Namespace: ns,
				Labels:    map[string]string{zabbix.InstanceLabel: z.Name, zabbix.ComponentLabel: zabbix.ComponentServer},
			},
			Spec: appsv1.DeploymentSpec{Replicas: new(replicas)},
		}
		dep.Status.Replicas = status
		return dep
	}
	readySTS := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: zabbix.ServerName(z.Name), Namespace: ns},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](2)},
		}
		sts.Status.ReadyReplicas = 2
		return sts
	}

	t.Run("drains", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(z, legacyScale(1, 2)).Build()
		r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t)}
		requeue, err := r.migrateLegacyServerDeployment(ctx, z)
		if err != nil || !requeue {
			t.Fatalf("requeue=%v err=%v, want requeue while running", requeue, err)
		}
		var dep appsv1.Deployment
		if err := c.Get(ctx, client.ObjectKey{Name: legacyScale(0, 0).Name, Namespace: ns}, &dep); err != nil {
			t.Fatal(err)
		}
		if *dep.Spec.Replicas != 0 {
			t.Errorf("replicas = %d, want 0", *dep.Spec.Replicas)
		}
	})

	t.Run("waits for drain", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(z, legacyScale(0, 1)).Build()
		r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t)}
		requeue, err := r.migrateLegacyServerDeployment(ctx, z)
		if err != nil || !requeue {
			t.Fatalf("requeue=%v err=%v, want requeue while terminating", requeue, err)
		}
	})

	t.Run("deletes husk after statefulset takeover", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(z, legacyScale(0, 0), readySTS()).Build()
		r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t)}
		requeue, err := r.migrateLegacyServerDeployment(ctx, z)
		if err != nil {
			t.Fatalf("migration delete: %v", err)
		}
		var dep appsv1.DeploymentList
		if err := c.List(ctx, &dep, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		if len(dep.Items) != 0 {
			t.Errorf("legacy deployment survived: %d items", len(dep.Items))
		}
		_ = requeue
	})

	t.Run("keeps husk until takeover and lets convergence proceed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(haTestScheme(t)).WithObjects(z, legacyScale(0, 0)).Build()
		r := &ZabbixReconciler{Client: c, Scheme: haTestScheme(t)}
		requeue, err := r.migrateLegacyServerDeployment(ctx, z)
		if err != nil {
			t.Fatalf("migration: %v", err)
		}
		if requeue {
			t.Error("drained husk must not requeue: convergence may not be blocked")
		}
		var dep appsv1.DeploymentList
		if err := c.List(ctx, &dep, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		if len(dep.Items) != 1 {
			t.Errorf("husk must stay until takeover, got %d items", len(dep.Items))
		}
	})
}
