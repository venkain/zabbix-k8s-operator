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
	"testing"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Shared literals for the recorded wire shapes (constants so repeated
// strings satisfy goconst).
const (
	apmTestToken    = "test-token"
	apmTestURL      = "http://zbx-clickhouse.monitoring.svc:8123"
	apmTestLogin    = "zabbix"
	apmTestPassword = "ch-pass"
)

// TestFetchAPMGlobalDBMapsSettingsOutput verifies the settings.get wire
// shape for the global APM data source (recorded in the lab: the
// response is a name → object map, all fields strings, password
// write-only and absent).
func TestFetchAPMGlobalDBMapsSettingsOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+apmTestToken {
			t.Errorf("Authorization header = %q, want bearer token", got)
		}
		var req struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.Method != "settings.get" {
			t.Errorf("method = %q, want settings.get", req.Method)
		}
		body := `{"jsonrpc":"2.0","result":{
			"apm_global_db":{"status":"1","url":"` + apmTestURL + `","db":"` + apmTestLogin + `","authentication_type":"0","username":"` + apmTestLogin + `","ssl_verify_peer":"0","ssl_verify_host":"0"},
			"timeout_telemetry_query":"3s"
		},"id":1}`
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	ds, err := FetchAPMGlobalDB(context.Background(), server.Client(), server.URL, apmTestToken)
	if err != nil {
		t.Fatalf("FetchAPMGlobalDB: %v", err)
	}
	if ds.Status != "1" || ds.URL != apmTestURL ||
		ds.Database != apmTestLogin || ds.Username != apmTestLogin {
		t.Errorf("parsed data source = %+v", ds)
	}
}

// TestUpdateAPMGlobalDBSendsWriteOnlyPassword verifies the settings.update
// payload: the full object with the write-only password (recorded in the
// lab — the disabled revert also carries an empty password).
func TestUpdateAPMGlobalDBSendsWriteOnlyPassword(t *testing.T) {
	var gotParams map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "settings.update" {
			t.Errorf("method = %q, want settings.update", req.Method)
		}
		gotParams = req.Params
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":["apm_global_db"],"id":1}`))
	}))
	defer server.Close()

	err := UpdateAPMGlobalDB(context.Background(), server.Client(), server.URL, apmTestToken,
		apmTestPassword, APMGlobalDB{Status: APMStatusEnabled, URL: "http://ch:8123", Database: apmTestLogin, Username: apmTestLogin})
	if err != nil {
		t.Fatalf("UpdateAPMGlobalDB: %v", err)
	}
	entry, ok := gotParams["apm_global_db"].(map[string]any)
	if !ok {
		t.Fatalf("params[apm_global_db] = %#v, want object", gotParams["apm_global_db"])
	}
	want := map[string]any{
		"status":              "1",
		"url":                 "http://ch:8123",
		"db":                  apmTestLogin,
		"authentication_type": "0",
		"username":            apmTestLogin,
		"password":            apmTestPassword,
	}
	for k, v := range want {
		if entry[k] != v {
			t.Errorf("apm_global_db[%q] = %v, want %v", k, entry[k], v)
		}
	}
}

// TestUpdateProxiesAPMBatchShape verifies the batched proxy.update
// payload (recorded in the lab: an array of {proxyid, apm} objects with
// string values; proxy.get cannot read the state back).
func TestUpdateProxiesAPMBatchShape(t *testing.T) {
	var gotParams []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string           `json:"method"`
			Params []map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "proxy.update" {
			t.Errorf("method = %q, want proxy.update", req.Method)
		}
		gotParams = req.Params
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"proxyids":["6","7"]},"id":1}`))
	}))
	defer server.Close()

	err := UpdateProxiesAPM(context.Background(), server.Client(), server.URL, apmTestToken, []ProxyAPMUpdate{
		{ProxyID: 6, Enabled: true, MaxMessagesPerSecond: 10000},
		{ProxyID: 7},
	})
	if err != nil {
		t.Fatalf("UpdateProxiesAPM: %v", err)
	}
	if len(gotParams) != 2 {
		t.Fatalf("params length = %d, want 2", len(gotParams))
	}
	if gotParams[0]["proxyid"] != "6" {
		t.Errorf("proxyid = %v, want \"6\"", gotParams[0]["proxyid"])
	}
	apm, ok := gotParams[0]["apm"].(map[string]any)
	if !ok || apm["data_collection_status"] != "1" || apm["max_messages_per_second"] != "10000" {
		t.Errorf("enabled apm object = %#v", gotParams[0]["apm"])
	}
	apm, ok = gotParams[1]["apm"].(map[string]any)
	if !ok || apm["data_collection_status"] != "0" || apm["max_messages_per_second"] != "0" {
		t.Errorf("disabled apm object = %#v", gotParams[1]["apm"])
	}
}

// TestAPMAPIJSONRPCError verifies that a JSON-RPC error member (HTTP 200,
// recorded in the lab) surfaces as a Go error through the APM calls.
func TestAPMAPIJSONRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid params.","data":"bad"},"id":1}`))
	}))
	defer server.Close()

	if _, err := FetchAPMGlobalDB(context.Background(), server.Client(), server.URL, "t"); err == nil {
		t.Error("FetchAPMGlobalDB: want error for JSON-RPC error member")
	}
	if err := UpdateProxiesAPM(context.Background(), server.Client(), server.URL, "t", []ProxyAPMUpdate{{ProxyID: 1, Enabled: true}}); err == nil {
		t.Error("UpdateProxiesAPM: want error for JSON-RPC error member")
	}
}

// TestAPMQuotaDefault verifies the effective quota resolution.
func TestAPMQuotaDefault(t *testing.T) {
	pg := &zabbixv1alpha1.ZabbixProxyGroup{}
	if got := APMQuota(pg); got != 10000 {
		t.Errorf("APMQuota(empty spec) = %d, want 10000", got)
	}
	var quota int64 = 2500
	pg.Spec.APM = &zabbixv1alpha1.ProxyAPMSpec{MaxMessagesPerSecond: quota}
	if got := APMQuota(pg); got != 2500 {
		t.Errorf("APMQuota(specified) = %d, want 2500", got)
	}
}
