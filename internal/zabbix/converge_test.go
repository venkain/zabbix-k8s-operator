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
	"context"
	"testing"

	"k8s.io/utils/ptr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zabbixv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestConvergeCreateSetsOwner(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	z := testZabbix()

	if err := Converge(context.Background(), c, z, FrontendService(z)); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	var live corev1.Service
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: z.Namespace, Name: FrontendName(z.Name)}, &live); err != nil {
		t.Fatalf("get created service: %v", err)
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != z.UID {
		t.Errorf("owner references = %+v, want the Zabbix resource", live.OwnerReferences)
	}
}

func TestConvergeUpdateAppliesSpecAndPreservesUnmanagedFields(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	z := testZabbix()

	// Pre-existing service as if allocated by the API server, with a
	// stale selector to prove the applied configuration is authoritative
	// for the fields the operator manages.
	existing := FrontendService(z)
	existing.Spec.ClusterIP = "10.0.0.5"
	existing.Spec.Selector = map[string]string{"legacy": "selector"}
	if err := c.Create(ctx, existing); err != nil {
		t.Fatalf("seeding existing service: %v", err)
	}

	if err := Converge(ctx, c, z, FrontendService(z)); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	var live corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: FrontendName(z.Name)}, &live); err != nil {
		t.Fatalf("get converged service: %v", err)
	}
	if live.Spec.ClusterIP != "10.0.0.5" {
		t.Errorf("clusterIP = %q, want preserved 10.0.0.5 (unmanaged field)", live.Spec.ClusterIP)
	}
	if live.Spec.Selector[ComponentLabel] != ComponentFrontend {
		t.Errorf("selector = %v, want applied selector", live.Spec.Selector)
	}
	if live.OwnerReferences[0].UID != z.UID {
		t.Errorf("owner references = %+v, want the Zabbix resource", live.OwnerReferences)
	}
}

func TestConvergeUpdateAppliesStatefulSetSpec(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	z := testZabbix()

	// First converge creates the statefulset...
	if err := Converge(ctx, c, z, ServerStatefulSet(z, testDB())); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	// ...then a spec change on the Zabbix resource must reach the live object.
	z.Spec.Server.Replicas = ptr.To[int32](3)
	if err := Converge(ctx, c, z, ServerStatefulSet(z, testDB())); err != nil {
		t.Fatalf("Converge after change: %v", err)
	}

	var live appsv1.StatefulSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: ServerName(z.Name)}, &live); err != nil {
		t.Fatalf("get converged statefulset: %v", err)
	}
	if *live.Spec.Replicas != 3 {
		t.Errorf("live replicas = %d, want 3", *live.Spec.Replicas)
	}
}

func TestConvergeAllCreatesTheStack(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	z := testZabbix()
	z.Spec.WebService = &zabbixv1alpha1.WebServiceSpec{}

	if err := ConvergeAll(ctx, c, z, testDB(), ServerStatefulSet(z, testDB())); err != nil {
		t.Fatalf("ConvergeAll: %v", err)
	}

	want := []client.ObjectKey{
		{Name: ServerName(z.Name), Namespace: z.Namespace},
		{Name: ServerName(z.Name) + "-active", Namespace: z.Namespace},
		{Name: FrontendName(z.Name), Namespace: z.Namespace},
		{Name: WebServiceName(z.Name), Namespace: z.Namespace},
	}
	for _, key := range want {
		var svc corev1.Service
		if err := c.Get(ctx, key, &svc); err != nil {
			t.Errorf("service %s missing after ConvergeAll: %v", key.Name, err)
		}
	}
	// The server is a StatefulSet (stable HA node identity); the rest are
	// Deployments.
	var sts appsv1.StatefulSet
	if err := c.Get(ctx, client.ObjectKey{Name: ServerName(z.Name), Namespace: z.Namespace}, &sts); err != nil {
		t.Errorf("statefulset %s missing after ConvergeAll: %v", ServerName(z.Name), err)
	}
	for _, key := range []client.ObjectKey{
		{Name: FrontendName(z.Name), Namespace: z.Namespace},
		{Name: WebServiceName(z.Name), Namespace: z.Namespace},
	} {
		var dep appsv1.Deployment
		if err := c.Get(ctx, key, &dep); err != nil {
			t.Errorf("deployment %s missing after ConvergeAll: %v", key.Name, err)
		}
	}
}

func TestServerReplicasDefault(t *testing.T) {
	if got := ServerReplicas(testZabbix()); got != 1 {
		t.Errorf("default server replicas = %d, want 1", got)
	}
	z := testZabbix()
	z.Spec.Server.Replicas = ptr.To[int32](5)
	if got := ServerReplicas(z); got != 5 {
		t.Errorf("server replicas = %d, want 5", got)
	}
}

func TestSetCondition(t *testing.T) {
	var status zabbixv1alpha1.ZabbixStatus
	SetCondition(&status, "ServerReady", metav1.ConditionTrue, "R", "m", 7)

	if len(status.Conditions) != 1 {
		t.Fatalf("conditions = %+v", status.Conditions)
	}
	cond := status.Conditions[0]
	if cond.Type != "ServerReady" || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != 7 {
		t.Errorf("condition = %+v", cond)
	}

	// Idempotent reapplication must not duplicate.
	SetCondition(&status, "ServerReady", metav1.ConditionFalse, "R2", "m2", 8)
	if len(status.Conditions) != 1 {
		t.Fatalf("conditions after update = %+v", status.Conditions)
	}
	if status.Conditions[0].Status != metav1.ConditionFalse || status.Conditions[0].ObservedGeneration != 8 {
		t.Errorf("condition not updated: %+v", status.Conditions[0])
	}
}
