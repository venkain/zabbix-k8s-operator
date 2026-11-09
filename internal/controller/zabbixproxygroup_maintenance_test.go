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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// Shared literals for the proxy group tests.
const (
	pgTestVersion  = "7.0"
	pgTestNS       = "pg-ns"
	pgTestServer   = "zbx"
	pgTestSecret   = "pg-api-token"
	pgTestTokenKey = "token"
	pgTestTokenVal = "pg-token"

	// The instance name and the JSON-RPC method names exercised by these
	// tests.
	pgTestName = "edge"

	rpcPGGroupGet    = "proxygroup.get"
	rpcPGGroupUpdate = "proxygroup.update"
	rpcPGGroupDelete = "proxygroup.delete"
	rpcPGProxyGet    = "proxy.get"
	rpcPGProxyCreate = "proxy.create"
	rpcPGProxyDelete = "proxy.delete"

	// respPGGroupIDs is the canned mutation response naming group 7.
	respPGGroupIDs = `{"jsonrpc":"2.0","result":{"proxy_groupids":["7"]},"id":1}`
)

// rpcCall is one recorded JSON-RPC request.
type rpcCall struct {
	Method string
	Params map[string]any
	// Raw carries the params in their original JSON shape — array params
	// (proxy.delete/proxygroup.delete ids) are not map-decodable.
	Raw any
}

// pgHarness is a reconciler with a fake client and a recorded JSON-RPC
// stub standing in for the referenced server's frontend API.
type pgHarness struct {
	ZabbixProxyGroupReconciler
	calls []rpcCall
}

// newPGHarness seeds the fake client and starts the stub. The response
// per JSON-RPC method is given in responses.
func newPGHarness(t *testing.T, responses map[string]string, seed ...client.Object) *pgHarness {
	t.Helper()

	h := &pgHarness{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var params map[string]any
		var raw any
		_ = json.Unmarshal(req.Params, &raw)
		if m, ok := raw.(map[string]any); ok {
			params = m
		}
		h.calls = append(h.calls, rpcCall{Method: req.Method, Params: params, Raw: raw})
		resp, ok := responses[req.Method]
		if !ok {
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(server.Close)

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := zabbixv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	h.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).WithStatusSubresource(
		&zabbixv1alpha1.Zabbix{}, &zabbixv1alpha1.ZabbixProxyGroup{},
	).Build()
	h.Scheme = s
	// The API endpoint resolves an in-cluster DNS name; the existing
	// rewriteTransport (ha_maintenance_test.go) routes it to the stub.
	h.HTTP = &http.Client{Transport: rewriteTransport{to: server}}
	return h
}

// newTestProxyGroup builds a fresh ZabbixProxyGroup pointing at pgTestServer.
func newTestProxyGroup() *zabbixv1alpha1.ZabbixProxyGroup {
	return &zabbixv1alpha1.ZabbixProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: pgTestName, Namespace: pgTestNS, UID: "uid-pg"},
		Spec: zabbixv1alpha1.ZabbixProxyGroupSpec{
			Version:   pgTestVersion,
			GroupName: "edge-group",
			ServerRef: pgTestServer,
			Replicas:  ptr.To[int32](2),
		},
	}
}

// newPGTestZabbix is the referenced server resource with API wiring.
func newPGTestZabbix() *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: pgTestServer, Namespace: pgTestNS, UID: "uid-zbx"},
		Spec: zabbixv1alpha1.ZabbixSpec{
			Version: pgTestVersion,
			Database: zabbixv1alpha1.DatabaseSpec{
				CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "db"},
			},
			API: &zabbixv1alpha1.APISpec{
				TokenSecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pgTestSecret},
					Key:                  pgTestTokenKey,
				},
			},
		},
	}
}

func newPGTestTokenSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pgTestSecret, Namespace: pgTestNS},
		Data:       map[string][]byte{pgTestTokenKey: []byte(pgTestTokenVal)},
	}
}

// reconcileOnce runs one reconcile for the group and fails the test on error.
func (h *pgHarness) reconcileOnce(t *testing.T) {
	t.Helper()
	_, err := h.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: pgTestNS, Name: pgTestName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (h *pgHarness) methods() []string {
	out := make([]string, 0, len(h.calls))
	for _, c := range h.calls {
		out = append(out, c.Method)
	}
	return out
}

func countCalls(methods []string, want string) int {
	n := 0
	for _, m := range methods {
		if m == want {
			n++
		}
	}
	return n
}

// Edge-group API wire responses used by the "fresh pass" tests.
var pgCreateResponses = map[string]string{
	rpcPGGroupGet:       `{"jsonrpc":"2.0","result":[],"id":1}`,
	"proxygroup.create": respPGGroupIDs,
	rpcPGProxyGet:       `{"jsonrpc":"2.0","result":[],"id":1}`,
	rpcPGProxyCreate:    `{"jsonrpc":"2.0","result":{"proxyids":["11"]},"id":1}`,
}

// pgLiveResponses describes an in-sync state: group exists with the
// default settings (FailoverDelay 1m, MinOnline 1) and both members are
// present with the addresses the operator would register.
func pgLiveResponses() map[string]string {
	return map[string]string{
		rpcPGGroupGet: `{"jsonrpc":"2.0","result":[{"proxy_groupid":"7","name":"edge-group","failover_delay":"1m","min_online":"1","state":"3"}],"id":1}`,
		rpcPGProxyGet: `{"jsonrpc":"2.0","result":[
			{"proxyid":"11","host":"edge-proxy-0","proxy_groupid":"7","operating_mode":"0","local_address":"edge-proxy-0.edge-proxy.pg-ns.svc","local_port":"10051"},
			{"proxyid":"12","host":"edge-proxy-1","proxy_groupid":"7","operating_mode":"0","local_address":"edge-proxy-1.edge-proxy.pg-ns.svc","local_port":"10051"}
		],"id":1}`,
	}
}

func TestReconcileConvergesWorkloadAndServerSideGroup(t *testing.T) {
	h := newPGHarness(t, pgCreateResponses,
		newTestProxyGroup(), newPGTestZabbix(), newPGTestTokenSecret())

	h.reconcileOnce(t)

	// Workload: member StatefulSet with the per-pod identity env and the
	// headless Service for member DNS.
	var sts appsv1.StatefulSet
	if err := h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: "edge-proxy"}, &sts); err != nil {
		t.Fatalf("statefulset get: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 {
		t.Errorf("replicas = %v", sts.Spec.Replicas)
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "zabbix/zabbix-proxy-sqlite3:ubuntu-7.0-latest" {
		t.Errorf("image = %q", got)
	}
	wantOwner := []metav1.OwnerReference{{
		APIVersion:         "zabbix.venkain.io/v1alpha1",
		Kind:               "ZabbixProxyGroup",
		Name:               pgTestName,
		UID:                "uid-pg",
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}}
	if !reflect.DeepEqual(sts.OwnerReferences, wantOwner) {
		t.Errorf("owner refs = %+v", sts.OwnerReferences)
	}

	var svc corev1.Service
	if err := h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: "edge-proxy"}, &svc); err != nil {
		t.Fatalf("service get: %v", err)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("service clusterIP = %q, want headless", svc.Spec.ClusterIP)
	}

	// Server-side: one group creation, one member record per ordinal.
	methods := h.methods()
	if n := countCalls(methods, rpcPGProxyCreate); n != 2 {
		t.Errorf("proxy.create calls = %d, want 2 (one per member)", n)
	}
	for _, call := range h.calls {
		if call.Method != rpcPGProxyCreate {
			continue
		}
		if call.Params["operating_mode"] != 0.0 {
			t.Errorf("create params = %+v, want operating_mode 0", call.Params)
		}
		host, _ := call.Params["host"].(string)
		addr, _ := call.Params["local_address"].(string)
		wantAddr := host + ".edge-proxy." + pgTestNS + ".svc"
		if addr != wantAddr {
			t.Errorf("member %s local_address = %q, want %q", host, addr, wantAddr)
		}
	}

	// Custom resources: finalizer set, group state published on status.
	var stored zabbixv1alpha1.ZabbixProxyGroup
	if err := h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: pgTestName}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	found := false
	for _, f := range stored.Finalizers {
		if f == ProxyGroupFinalizer {
			found = true
		}
	}
	if !found {
		t.Errorf("finalizers = %v", stored.Finalizers)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions,
		string(zabbixv1alpha1.ConditionProxyGroupAPIReady))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("APIReady condition = %+v", cond)
	}
	// The state of a freshly created group is polled by the next pass;
	// it is not published until proxygroup.get returns one.
	if stored.Status.GroupState != "" {
		t.Errorf("groupState = %q, want empty right after creation", stored.Status.GroupState)
	}
}

func TestReconcileInSyncPassIsIdempotent(t *testing.T) {
	h := newPGHarness(t, pgLiveResponses(),
		newTestProxyGroup(), newPGTestZabbix(), newPGTestTokenSecret())

	h.reconcileOnce(t)
	before := len(h.calls)
	if before == 0 {
		t.Fatal("no API calls made")
	}
	h.calls = nil

	h.reconcileOnce(t)
	methods := h.methods()
	// Only the two reads remain; no group/member mutation in sync state.
	for _, m := range methods {
		switch m {
		case rpcPGGroupGet, rpcPGProxyGet:
		default:
			t.Errorf("out-of-sync pass issued %q", m)
		}
	}

	var stored zabbixv1alpha1.ZabbixProxyGroup
	if err := h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: pgTestName}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status.GroupState != string(zabbix.ProxyGroupStateOnline) {
		t.Errorf("groupState = %q, want online", stored.Status.GroupState)
	}
}

func TestReconcileUnresolvedServerRefSetsCondition(t *testing.T) {
	h := newPGHarness(t, nil, newTestProxyGroup(), newPGTestTokenSecret())

	h.reconcileOnce(t)

	var stored zabbixv1alpha1.ZabbixProxyGroup
	if err := h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: pgTestName}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := apimeta.FindStatusCondition(stored.Status.Conditions,
		string(zabbixv1alpha1.ConditionProxyGroupServerResolved))
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServerRefUnresolved" {
		t.Fatalf("condition = %+v", cond)
	}
	// No workload must have been deployed for an unresolved reference.
	var stsList appsv1.StatefulSetList
	if err := h.List(context.Background(), &stsList); err != nil {
		t.Fatal(err)
	}
	if len(stsList.Items) != 0 {
		t.Errorf("statefulsets = %+v", stsList.Items)
	}
}

func TestSyncGroupMembersCreatesUpdatesAndDeletes(t *testing.T) {
	// In-sync members come back amended: one drifted address, one extra
	// scaled-down member, one missing ordinal.
	responses := map[string]string{
		rpcPGGroupGet:    `{"jsonrpc":"2.0","result":[{"proxy_groupid":"7","name":"edge-group","failover_delay":"1m","min_online":"1","state":"3"}],"id":1}`,
		rpcPGGroupUpdate: respPGGroupIDs,
		rpcPGProxyGet: `{"jsonrpc":"2.0","result":[
			{"proxyid":"11","host":"edge-proxy-0","proxy_groupid":"7","operating_mode":"0","local_address":"stale-address.svc","local_port":"10051"},
			{"proxyid":"13","host":"edge-proxy-2","proxy_groupid":"7","operating_mode":"0","local_address":"edge-proxy-2.edge-proxy.pg-ns.svc","local_port":"10051"},
			{"proxyid":"20","host":"user-own-proxy","proxy_groupid":"7","operating_mode":"0","local_address":"user.svc","local_port":"10051"}
		],"id":1}`,
		rpcPGProxyCreate: `{"jsonrpc":"2.0","result":{"proxyids":["12"]},"id":1}`,
		"proxy.update":   `{"jsonrpc":"2.0","result":{"proxyids":["11"]},"id":1}`,
		rpcPGProxyDelete: `{"jsonrpc":"2.0","result":{"proxyids":["13"]},"id":1}`,
	}
	h := newPGHarness(t, responses,
		newTestProxyGroup(), newPGTestZabbix(), newPGTestTokenSecret())

	h.reconcileOnce(t)

	created, updated, deleted := 0, 0, false
	var createdHosts []string
	for _, call := range h.calls {
		switch call.Method {
		case rpcPGProxyCreate:
			created++
			createdHosts = append(createdHosts, call.Params["host"].(string))
		case "proxy.update":
			updated++
			if id, _ := call.Params["proxyid"].(string); id != "11" || call.Params["local_address"] != "edge-proxy-0.edge-proxy.pg-ns.svc" {
				t.Errorf("update params = %+v", call.Params)
			}
		case rpcPGProxyDelete:
			deleted = true
			if ids, ok := call.Raw.([]any); !ok || len(ids) != 1 || ids[0] != "13" {
				t.Errorf("delete params = %+v, want [\"13\"]", call.Raw)
			}
		case rpcPGGroupUpdate:
			if call.Params["min_online"] != "1" || call.Params["failover_delay"] != "1m" {
				t.Errorf("group update params = %+v, want defaults", call.Params)
			}
		}
	}
	if !deleted {
		t.Errorf("stale member edge-proxy-2 was not deleted")
	}
	if created != 1 || createdHosts[0] != "edge-proxy-1" {
		t.Errorf("created = %d %v, want only edge-proxy-1", created, createdHosts)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}
}

// finalizeResponses covers the cleanup path: resolve the group, list its
// members, delete both records.
func finalizeResponses() map[string]string {
	return map[string]string{
		rpcPGGroupGet:    pgLiveResponses()[rpcPGGroupGet],
		rpcPGProxyGet:    pgLiveResponses()[rpcPGProxyGet],
		rpcPGProxyDelete: `{"jsonrpc":"2.0","result":{"proxyids":["11","12"]},"id":1}`,
		rpcPGGroupDelete: respPGGroupIDs,
	}
}

func TestFinalizeDeletesServerSideRecords(t *testing.T) {
	pg := newTestProxyGroup()
	pg.Finalizers = []string{ProxyGroupFinalizer}
	now := metav1.Now()
	pg.DeletionTimestamp = &now

	h := newPGHarness(t, finalizeResponses(), pg, newPGTestZabbix(), newPGTestTokenSecret())
	_, err := h.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: pgTestNS, Name: pgTestName},
	})
	if err != nil {
		t.Fatalf("Reconcile(delete): %v", err)
	}

	var groupDeleted bool
	for _, call := range h.calls {
		if call.Method == rpcPGGroupDelete {
			if ids, ok := call.Raw.([]any); ok && len(ids) == 1 && ids[0] == "7" {
				groupDeleted = true
			}
		}
	}
	if !groupDeleted {
		t.Errorf("group.delete not issued with the right id: %v", h.calls)
	}

	// With the finalizer released, the resource (and its workload
	// objects through owner references) is gone.
	var stored zabbixv1alpha1.ZabbixProxyGroup
	err = h.Get(context.Background(),
		types.NamespacedName{Namespace: pgTestNS, Name: pgTestName}, &stored)
	if err == nil {
		t.Fatalf("resource still present with finalizers %v", stored.Finalizers)
	}
}
