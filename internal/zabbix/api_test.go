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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchHANodesMapsRegistryOutput(t *testing.T) {
	// Recorded from the kind lab: hanode.get returns status/lastaccess
	// as JSON strings, and the standalone (empty-name) node reports
	// status 3 = active while it holds the registry.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want bearer token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json-rpc" {
			t.Errorf("Content-Type = %q", got)
		}
		body := `{"jsonrpc":"2.0","result":[
			{"ha_nodeid":"a","name":"zbx-server-0","address":"10.0.0.1","port":"10051","lastaccess":"1794054744","status":"3"},
			{"ha_nodeid":"b","name":"zbx-server-1","address":"10.0.0.2","port":"10051","lastaccess":"1794054740","status":"0"},
			{"ha_nodeid":"c","name":"zbx-server-2","address":"10.0.0.3","port":"10051","lastaccess":"1794050000","status":"2"},
			{"ha_nodeid":"d","name":"","address":"localhost","port":"10051","lastaccess":"1794054744","status":"3"}
		],"id":1}`
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	nodes, err := FetchHANodes(context.Background(), server.Client(), server.URL, "test-token")
	if err != nil {
		t.Fatalf("FetchHANodes: %v", err)
	}

	wantStates := []HANodeState{HANodeStateActive, HANodeStateStandby, HANodeStateUnavailable, HANodeStateActive}
	if len(nodes) != len(wantStates) {
		t.Fatalf("nodes = %+v, want %d entries", nodes, len(wantStates))
	}
	for i, want := range wantStates {
		if nodes[i].Status != want {
			t.Errorf("node %d status = %q, want %q", i, nodes[i].Status, want)
		}
	}
	if nodes[0].Name != "zbx-server-0" || nodes[0].Address != "10.0.0.1" {
		t.Errorf("node 0 = %+v", nodes[0])
	}
	if nodes[0].LastAccess != 1794054744 {
		t.Errorf("node 0 lastaccess = %d", nodes[0].LastAccess)
	}
}

func TestFetchHANodesUnknownStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[{"name":"n","address":"a","port":"1","lastaccess":"1","status":"42"}],"id":1}`))
	}))
	defer server.Close()

	nodes, err := FetchHANodes(context.Background(), nil, server.URL, "t")
	if err != nil {
		t.Fatalf("FetchHANodes: %v", err)
	}
	if nodes[0].Status != HANodeStateUnknown {
		t.Errorf("status = %q, want unknown", nodes[0].Status)
	}
}

func TestFetchHANodesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"message":"Service unavailable","data":"server is not running"},"id":1}`))
	}))
	defer server.Close()

	_, err := FetchHANodes(context.Background(), nil, server.URL, "t")
	if err == nil || !strings.Contains(err.Error(), "server is not running") {
		t.Errorf("error = %v, want the JSON-RPC error data", err)
	}
}

func TestFetchHANodesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := FetchHANodes(context.Background(), nil, server.URL, "t")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want HTTP status error", err)
	}
}

func TestAPIEndpoint(t *testing.T) {
	z := testZabbix()
	want := "http://test-zabbix-frontend.test-ns.svc:80/api_jsonrpc.php"
	if got := APIEndpoint(z); got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
}
