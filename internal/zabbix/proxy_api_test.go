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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// proxyGroupStubServer dispatches JSON-RPC methods to canned responses and
// records each request so tests can assert the exact payloads sent to the
// server.
func proxyGroupStubServer(t *testing.T, responses map[string]string, recorded *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		var rawParams any
		_ = json.Unmarshal(req.Params, &rawParams)
		if m, ok := rawParams.(map[string]any); ok {
			params = m
		}
		if recorded != nil {
			*recorded = append(*recorded, map[string]any{"method": req.Method, "params": rawParams})
		}
		_ = params
		resp, ok := responses[req.Method]
		if !ok {
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(resp))
	}))
}

// Shared test literals and canned responses — constants so repeated
// strings satisfy goconst and intent stays explicit.
const (
	rpcProxyGroupGet  = "proxygroup.get"
	respProxyGroupIDs = `{"jsonrpc":"2.0","result":{"proxy_groupids":["7"]},"id":1}`
)

func TestFetchProxyGroupReturnsNilWhenAbsent(t *testing.T) {
	recorded := make([]map[string]any, 0, 4)
	server := proxyGroupStubServer(t, map[string]string{
		rpcProxyGroupGet: `{"jsonrpc":"2.0","result":[],"id":1}`,
	}, &recorded)
	defer server.Close()

	group, err := FetchProxyGroup(context.Background(), server.Client(), server.URL, "tok", "pg")
	if err != nil {
		t.Fatalf("FetchProxyGroup: %v", err)
	}
	if group != nil {
		t.Errorf("group = %+v, want nil", group)
	}
	if len(recorded) != 1 || recorded[0]["method"] != rpcProxyGroupGet {
		t.Fatalf("recorded = %+v", recorded)
	}
	filter, _ := recorded[0]["params"].(map[string]any)["filter"].(map[string]any)
	names, _ := filter["name"].([]any)
	if len(names) != 1 || names[0] != "pg" {
		t.Errorf("filter = %+v, want name filter", filter)
	}
}

func TestFetchProxyGroupMapsState(t *testing.T) {
	for st, want := range map[int]ProxyGroupState{0: ProxyGroupStateUnknown, 1: ProxyGroupStateOffline, 2: ProxyGroupStateRecovering, 3: ProxyGroupStateOnline, 4: ProxyGroupStateDegrading} {
		resp := `{"jsonrpc":"2.0","result":[{"proxy_groupid":"7","name":"pg","failover_delay":"30s","min_online":"2","state":"` + itoa(st) + `"}],"id":1}`
		server := proxyGroupStubServer(t, map[string]string{rpcProxyGroupGet: resp}, nil)
		group, err := FetchProxyGroup(context.Background(), nil, server.URL, "tok", "pg")
		if err != nil {
			t.Fatalf("FetchProxyGroup state %d: %v", st, err)
		}
		if group.State != want {
			t.Errorf("state %d -> %q, want %q", st, group.State, want)
		}
		if group.ID != 7 || group.FailoverDelay != testFailoverDelay || group.MinOnline != "2" {
			t.Errorf("group = %+v", group)
		}
	}
}

// itoa avoids a strconv import just for building JSON literals.
func itoa(n int) string {
	return string(rune('0' + n))
}

func TestCreateAndUpdateProxyGroupPayload(t *testing.T) {
	recorded := make([]map[string]any, 0, 4)
	server := proxyGroupStubServer(t, map[string]string{
		"proxygroup.create": respProxyGroupIDs,
		"proxygroup.update": respProxyGroupIDs,
	}, &recorded)
	defer server.Close()

	id, err := CreateProxyGroup(context.Background(), server.Client(), server.URL, "tok", "pg", testFailoverDelay, "2")
	if err != nil {
		t.Fatalf("CreateProxyGroup: %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}
	if err := UpdateProxyGroup(context.Background(), server.Client(), server.URL, "tok", 7, "45s", "3"); err != nil {
		t.Fatalf("UpdateProxyGroup: %v", err)
	}

	create := recorded[0]["params"].(map[string]any)
	if create["name"] != "pg" || create["failover_delay"] != testFailoverDelay || create["min_online"] != "2" {
		t.Errorf("create params = %+v", create)
	}
	update := recorded[1]["params"].(map[string]any)
	if update["proxy_groupid"] != "7" || update["failover_delay"] != "45s" || update["min_online"] != "3" {
		t.Errorf("update params = %+v", update)
	}
}

func TestGroupProxiesLifecycle(t *testing.T) {
	recorded := make([]map[string]any, 0, 4)
	server := proxyGroupStubServer(t, map[string]string{
		"proxy.get":    `{"jsonrpc":"2.0","result":[{"proxyid":"3","name":"pg-proxy-0","proxy_groupid":"7","operating_mode":"0","local_address":"pg-proxy-0.pg-proxy.ns.svc","local_port":"10051"}],"id":1}`,
		"proxy.create": `{"jsonrpc":"2.0","result":{"proxyids":["4"]},"id":1}`,
		"proxy.update": `{"jsonrpc":"2.0","result":{"proxyids":["3"]},"id":1}`,
		"proxy.delete": `{"jsonrpc":"2.0","result":{"proxyids":["3","4"]},"id":1}`,
	}, &recorded)
	defer server.Close()

	members, err := FetchGroupProxies(context.Background(), server.Client(), server.URL, "tok", 7)
	if err != nil {
		t.Fatalf("FetchGroupProxies: %v", err)
	}
	if len(members) != 1 || members[0].Name != "pg-proxy-0" || members[0].ID != 3 || members[0].OperatingMode != 0 || members[0].GroupID != 7 {
		t.Fatalf("members = %+v", members)
	}

	id, err := CreateProxyMember(context.Background(), server.Client(), server.URL, "tok", 7, "pg-proxy-2", "pg-proxy-2.pg-proxy.ns.svc", "10051")
	if err != nil {
		t.Fatalf("CreateProxyMember: %v", err)
	}
	if id != 4 {
		t.Errorf("id = %d, want 4", id)
	}
	create := recorded[1]["params"].(map[string]any)
	if create["proxy_groupid"] != "7" {
		t.Errorf("create params = %+v", create)
	}
	if create["operating_mode"] != 0.0 {
		t.Errorf("create params = %+v, want operating_mode 0", create)
	}
	if create["local_address"] != "pg-proxy-2.pg-proxy.ns.svc" || create["local_port"] != "10051" {
		t.Errorf("create params = %+v", create)
	}

	if err := UpdateProxyMember(context.Background(), server.Client(), server.URL, "tok", 3, "pg-proxy-0.pg-proxy.ns.svc", "10051"); err != nil {
		t.Fatalf("UpdateProxyMember: %v", err)
	}
	if err := DeleteProxyMembers(context.Background(), server.Client(), server.URL, "tok", []string{"3", "4"}); err != nil {
		t.Fatalf("DeleteProxyMembers: %v", err)
	}
	del := recorded[3]["params"]
	ids, ok := del.([]any)
	if !ok || len(ids) != 2 || ids[0] != "3" {
		t.Errorf("delete params = %+v", del)
	}
}

func TestCreateProxyGroupAPIError(t *testing.T) {
	server := proxyGroupStubServer(t, map[string]string{
		"proxygroup.create": `{"jsonrpc":"2.0","error":{"message":"Invalid params.","data":"No permissions to referred object or it does not exist!"},"id":1}`,
	}, nil)
	defer server.Close()

	_, err := CreateProxyGroup(context.Background(), nil, server.URL, "tok", "pg", "1m", "1")
	if err == nil || !strings.Contains(err.Error(), "No permissions") {
		t.Errorf("error = %v, want the JSON-RPC error data", err)
	}
}

func TestProxygroupDeletePayload(t *testing.T) {
	recorded := make([]map[string]any, 0, 4)
	server := proxyGroupStubServer(t, map[string]string{
		"proxygroup.delete": respProxyGroupIDs,
	}, &recorded)
	defer server.Close()

	if err := DeleteProxyGroup(context.Background(), nil, server.URL, "tok", 7); err != nil {
		t.Fatalf("DeleteProxyGroup: %v", err)
	}
	if ids, ok := recorded[0]["params"].([]any); !ok || len(ids) != 1 || ids[0] != "7" {
		t.Errorf("delete params = %+v, want [\"7\"]", recorded[0]["params"])
	}
}
