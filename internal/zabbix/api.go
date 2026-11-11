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
	"strconv"
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
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      int    `json:"id"`
}

// jsonrpcError is the error member of a JSON-RPC response.
type jsonrpcError struct {
	Message string `json:"message"`
	Data    string `json:"data"`
}

// jsonrpcEnvelope carries the result of any JSON-RPC method. The result
// type varies per method, so decoding is done through the generic
// callJSONRPC helper; hanode.get stays inlined as its historical surface.
type jsonrpcEnvelope[T any] struct {
	JSONRPC string        `json:"jsonrpc"`
	Result  T             `json:"result"`
	Error   *jsonrpcError `json:"error"`
}

// callJSONRPC issues one authenticated JSON-RPC request and decodes the
// result into T. The status code is checked after decoding: the frontend
// reports API errors inside a JSON-RPC error member with HTTP 200, while
// proxy errors surface as non-200 with arbitrary bodies.
func callJSONRPC[T any](ctx context.Context, httpClient *http.Client, endpoint, token, method string, params any) (T, error) {
	var zero T
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	body, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      1,
	})
	if err != nil {
		return zero, fmt.Errorf("encoding %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("building %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload jsonrpcEnvelope[T]
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("calling %s: unexpected HTTP status %d", endpoint, resp.StatusCode)
	}
	if decodeErr != nil {
		return zero, fmt.Errorf("decoding %s response: %w", method, decodeErr)
	}
	if payload.Error != nil {
		return zero, fmt.Errorf("%s failed: %s: %s", method, payload.Error.Message, payload.Error.Data)
	}
	return payload.Result, nil
}

// hanodeResult is one hanode.get registry entry as returned by the API.
// The string tags match the API's actual output format (integers arrive
// quoted), validated against the lab instance.
type hanodeResult struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Status     int64  `json:"status,string"`
	LastAccess int64  `json:"lastaccess,string"`
}

// outputExtend requests the full object output on *.get methods;
// paramOutput/paramName are the corresponding request parameter names.
const (
	outputExtend = "extend"
	paramOutput  = "output"
	paramName    = "name"
	// paramFilter is the request parameter carrying exact-match filters
	// (hostgroup.get, proxygroup.get, host.get, template.get).
	paramFilter = "filter"
	// paramHost matches hosts/templates by technical name.
	paramHost = "host"
	// paramTemplateID/paramMacro/paramValue are the host.get select
	// field names the cluster-metrics host reconciliation reads.
	paramTemplateID = "templateid"
	paramMacro      = "macro"
	paramValue      = "value"
	paramHostID     = "hostid"
)

// FetchHANodes queries the HA node registry via hanode.get. The endpoint
// is the frontend's api_jsonrpc.php URL; the token must be an API token
// with permission to read HA node state.
func FetchHANodes(ctx context.Context, httpClient *http.Client, endpoint, token string) ([]HANode, error) {
	raw, err := callJSONRPC[[]hanodeResult](ctx, httpClient, endpoint, token, "hanode.get", map[string]any{paramOutput: outputExtend})
	if err != nil {
		return nil, err
	}

	nodes := make([]HANode, 0, len(raw))
	for _, r := range raw {
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

// ProxyGroupState is the observed state of a Zabbix proxy group, using
// the terms of the official proxygroup state documentation.
type ProxyGroupState string

const (
	ProxyGroupStateUnknown    ProxyGroupState = "unknown"
	ProxyGroupStateOffline    ProxyGroupState = "offline"
	ProxyGroupStateRecovering ProxyGroupState = "recovering"
	ProxyGroupStateOnline     ProxyGroupState = "online"
	ProxyGroupStateDegrading  ProxyGroupState = "degrading"
)

// proxyGroupStateCodes are the numeric proxygroup.state values of the
// official API (8.0 proxygroup object). No gap covers an undocumented
// code: it maps to unknown for safety.
var proxyGroupStateCodes = map[int64]ProxyGroupState{
	0: ProxyGroupStateUnknown,
	1: ProxyGroupStateOffline,
	2: ProxyGroupStateRecovering,
	3: ProxyGroupStateOnline,
	4: ProxyGroupStateDegrading,
}

// ProxyGroup is one proxy group as returned by proxygroup.get.
type ProxyGroup struct {
	ID            int64
	Name          string
	FailoverDelay string
	MinOnline     string
	State         ProxyGroupState
}

// proxyGroupResult is one proxygroup.get entry in API wire format
// (integers arrive quoted, same as hanode.get).
type proxyGroupResult struct {
	ID            int64  `json:"proxy_groupid,string"`
	Name          string `json:"name"`
	FailoverDelay string `json:"failover_delay"`
	MinOnline     string `json:"min_online"`
	State         int64  `json:"state,string"`
}

// FetchProxyGroup looks up the group by name via proxygroup.get. It
// returns (nil, nil) when the group does not exist.
func FetchProxyGroup(ctx context.Context, httpClient *http.Client, endpoint, token, name string) (*ProxyGroup, error) {
	raw, err := callJSONRPC[[]proxyGroupResult](ctx, httpClient, endpoint, token, "proxygroup.get", map[string]any{
		paramOutput: outputExtend,
		paramFilter: map[string]any{paramName: []string{name}},
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	r := raw[0]
	state, ok := proxyGroupStateCodes[r.State]
	if !ok {
		state = ProxyGroupStateUnknown
	}
	return &ProxyGroup{
		ID:            r.ID,
		Name:          r.Name,
		FailoverDelay: r.FailoverDelay,
		MinOnline:     r.MinOnline,
		State:         state,
	}, nil
}

// CreateProxyGroup creates the operator-managed group via proxygroup.create.
func CreateProxyGroup(ctx context.Context, httpClient *http.Client, endpoint, token, name, failoverDelay, minOnline string) (int64, error) {
	created, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxygroup.create", map[string]any{
		"name":           name,
		"failover_delay": failoverDelay,
		"min_online":     minOnline,
	})
	if err != nil {
		return 0, err
	}
	return parseID(created.ProxyGroupIDs, "proxy_groupids")
}

// UpdateProxyGroup aligns the group settings via proxygroup.update.
func UpdateProxyGroup(ctx context.Context, httpClient *http.Client, endpoint, token string, groupID int64, failoverDelay, minOnline string) error {
	_, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxygroup.update", map[string]any{
		"proxy_groupid":  intToStringID(groupID),
		"failover_delay": failoverDelay,
		"min_online":     minOnline,
	})
	return err
}

// idResult is the common shape of proxy/proxygroup create, update and
// delete responses: the mutated ids.
type idResult struct {
	ProxyGroupIDs []string `json:"proxy_groupids"`
	ProxyIDs      []string `json:"proxyids"`
}

// parseID decodes the single id of a mutation response.
func parseID(ids []string, field string) (int64, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("%s response has no ids", field)
	}
	id, err := strconv.ParseInt(ids[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", field, err)
	}
	return id, nil
}

// ProxyMember is one proxy record as returned by proxy.get, restricted
// to the fields the group membership maintenance reads and writes. The
// identity property is "name" in 7.0+ (the pre-7.0 "host" name is gone).
type ProxyMember struct {
	ID            int64
	Name          string
	GroupID       int64
	OperatingMode int64
	LocalAddress  string
	LocalPort     string
	// LastContact is proxy.lastaccess (a read-only timestamp; the API
	// wire format carries it as a JSON string of a Unix timestamp in
	// seconds): the last time the member contacted the server.
	LastContact int64 `json:",omitempty"`
}

// proxyMemberResult is one proxy.get entry in API wire format.
type proxyMemberResult struct {
	ID            int64  `json:"proxyid,string"`
	Name          string `json:"name"`
	GroupID       int64  `json:"proxy_groupid,string"`
	OperatingMode int64  `json:"operating_mode,string"`
	LocalAddress  string `json:"local_address"`
	LocalPort     string `json:"local_port"`
	// lastaccess is a Unix timestamp carried as a JSON string (verified
	// against a live 8.0 endpoint and the API's timestamp column type).
	LastContact int64 `json:"lastaccess,string"`
}

// FetchGroupProxies lists the proxies that belong to the group, via
// proxy.get with the official proxy_groupids filter.
func FetchGroupProxies(ctx context.Context, httpClient *http.Client, endpoint, token string, groupID int64) ([]ProxyMember, error) {
	raw, err := callJSONRPC[[]proxyMemberResult](ctx, httpClient, endpoint, token, "proxy.get", map[string]any{
		paramOutput:      outputExtend,
		"proxy_groupids": []string{intToStringID(groupID)},
	})
	if err != nil {
		return nil, err
	}
	members := make([]ProxyMember, 0, len(raw))
	for _, r := range raw {
		members = append(members, ProxyMember(r))
	}
	return members, nil
}

// CreateProxyMember registers the server-side proxy record that makes a
// daemon join the group: name (the pod name — the identity the daemon
// self-reports), operating_mode=0 (active), the group assignment, and
// the local_address active agents connect to (official proxy object
// requirements for group members).
func CreateProxyMember(ctx context.Context, httpClient *http.Client, endpoint, token string, groupID int64, name, localAddress, localPort string) (int64, error) {
	created, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxy.create", map[string]any{
		paramName:        name,
		"operating_mode": 0,
		"proxy_groupid":  intToStringID(groupID),
		"local_address":  localAddress,
		"local_port":     localPort,
	})
	if err != nil {
		return 0, err
	}
	return parseID(created.ProxyIDs, "proxyids")
}

// UpdateProxyMember aligns a member's routing address via proxy.update.
func UpdateProxyMember(ctx context.Context, httpClient *http.Client, endpoint, token string, memberID int64, localAddress, localPort string) error {
	_, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxy.update", map[string]any{
		"proxyid":       intToStringID(memberID),
		"local_address": localAddress,
		"local_port":    localPort,
	})
	return err
}

// DeleteProxyMembers removes operator-created group members via proxy.delete.
func DeleteProxyMembers(ctx context.Context, httpClient *http.Client, endpoint, token string, memberIDs []string) error {
	_, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxy.delete", memberIDs)
	return err
}

// DeleteProxyGroup removes the operator-managed group via proxygroup.delete.
// Members receive proxy_groupid=0 server-side (they are not deleted).
func DeleteProxyGroup(ctx context.Context, httpClient *http.Client, endpoint, token string, groupID int64) error {
	_, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxygroup.delete", []string{intToStringID(groupID)})
	return err
}

// intToStringID renders an ID for the request payload; the API accepts
// both numbers and strings, strings match the server's own output style.
func intToStringID(id int64) string {
	return strconv.FormatInt(id, 10)
}
