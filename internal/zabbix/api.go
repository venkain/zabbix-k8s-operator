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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// HANodeState is the observed state of a Zabbix server HA node, using the
// terms of the official HA documentation (active, standby, unavailable,
// stopped).
type HANodeState string

const (
	HANodeStateActive      HANodeState = "active"
	HANodeStateStandby     HANodeState = "standby"
	HANodeStateUnavailable HANodeState = "unavailable"
	HANodeStateStopped     HANodeState = "stopped"
	// HANodeStateUnknown covers status codes outside the documented
	// registry values; it must never route a pod into the active Service.
	HANodeStateUnknown HANodeState = "unknown"
)

// hanodeStatusCodes are the numeric ha_node.status values of the official
// Zabbix source (include/zbx_ha_constants.h, release/8.0). Negative values
// are internal client states and do not appear in hanode.get output; they
// map to unknown for safety.
var hanodeStatusCodes = map[int64]HANodeState{
	0: HANodeStateStandby,
	1: HANodeStateStopped,
	2: HANodeStateUnavailable,
	3: HANodeStateActive,
}

// HANode is one entry of the han_node registry as returned by the
// official hanode.get method. In operator-managed deployments the node
// name equals the server pod name (ZBX_AUTOHANODENAME=hostname); the
// empty name is the standalone-mode registry entry, which matches no pod.
type HANode struct {
	Name       string
	Address    string
	Status     HANodeState
	LastAccess int64 // unix seconds of the last heartbeat
}

// APIEndpoint returns the URL of the JSON-RPC endpoint served by the
// instance's frontend. The frontend Service port (80, targeting the
// nginx container port) is used — not the container port: the API
// client talks to the Service, and a Service has no listener on the
// container port (validated in the lab: requests to :8080 hang).
func APIEndpoint(z *zabbixv1alpha1.Zabbix) string {
	return fmt.Sprintf("http://%s.%s.svc:%d/api_jsonrpc.php", FrontendName(z.Name), z.Namespace, frontendServicePort)
}

// jsonrpcRequest is a Zabbix JSON-RPC 2.0 request. Since 6.4 the API
// carries authentication in the Authorization header, not the body.
type jsonrpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
	ID      int            `json:"id"`
}

// jsonrpcError is the error member of a JSON-RPC response.
type jsonrpcError struct {
	Message string `json:"message"`
	Data    string `json:"data"`
}

// jsonrpcResponse is a hanode.get response: either a result or an error.
type jsonrpcResponse struct {
	Result []struct {
		Name       string `json:"name"`
		Address    string `json:"address"`
		Status     int64  `json:"status,string"`
		LastAccess int64  `json:"lastaccess,string"`
	} `json:"result"`
	Error *jsonrpcError `json:"error"`
}

// FetchHANodes queries the HA node registry via hanode.get. The endpoint
// is the frontend's api_jsonrpc.php URL; the token must be an API token
// with permission to read HA node state.
func FetchHANodes(ctx context.Context, httpClient *http.Client, endpoint, token string) ([]HANode, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	body, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "hanode.get",
		Params:  map[string]any{"output": "extend"},
		ID:      1,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding hanode.get request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building hanode.get request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload jsonrpcResponse
	// The status code is checked after decoding: the frontend reports API
	// errors inside a JSON-RPC error member with HTTP 200, while proxy
	// errors surface as non-200 with arbitrary bodies.
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("calling %s: unexpected HTTP status %d", endpoint, resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding hanode.get response: %w", decodeErr)
	}
	if payload.Error != nil {
		return nil, fmt.Errorf("hanode.get failed: %s: %s", payload.Error.Message, payload.Error.Data)
	}

	nodes := make([]HANode, 0, len(payload.Result))
	for _, r := range payload.Result {
		state, ok := hanodeStatusCodes[r.Status]
		if !ok {
			state = HANodeStateUnknown
		}
		nodes = append(nodes, HANode{
			Name:       r.Name,
			Address:    r.Address,
			Status:     state,
			LastAccess: r.LastAccess,
		})
	}
	return nodes, nil
}
