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
	"fmt"
	"net/http"
)

// Zabbix API surface for cluster-metrics host management. Wire shapes
// verified against Zabbix 8.0 on the lab: integer ids arrive quoted;
// mutations return id-string arrays. Zabbix 8.0 authenticates through
// the Authorization header (callJSONRPC already handles this).

const (
	// ClusterTemplate is the official template the cluster host links
	// to. Resolved by NAME: template ids differ between Zabbix builds.
	ClusterTemplate = "Kubernetes Cluster by HTTP"

	// ClusterHostGroup is the Zabbix host group the cluster host lives in.
	ClusterHostGroup = "Kubernetes clusters"

	// MacroKubeAPIToken holds the monitor service-account token.
	MacroKubeAPIToken = "{$KUBE.API.TOKEN}"

	// MacroHTTPTLSVerify controls certificate verification for the
	// template's HTTP items.
	MacroHTTPTLSVerify = "{$HTTP.TLS.VERIFY}"
)

// Macro is one host macro in wire format.
type Macro struct {
	Macro string `json:"macro"`
	Value string `json:"value"`
}

// hostGroupResult is one hostgroup.get entry.
type hostGroupResult struct {
	ID   int64  `json:"groupid,string"`
	Name string `json:"name"`
}

// hostGroupsResult wraps hostgroup.create.
type hostGroupsResult struct {
	GroupIDs []string `json:"groupids"`
}

// templateResult is one template.get entry.
type templateResult struct {
	ID   int64  `json:"templateid,string"`
	Name string `json:"name"`
}

// hostResult is one host.get entry with the linkage the operator
// reconciles: linked templates and host macros.
type hostResult struct {
	ID        int64  `json:"hostid,string"`
	Host      string `json:"host"`
	Templates []struct {
		ID   int64   `json:"templateid,string"`
		Name *string `json:"name,omitempty"`
	} `json:"parentTemplates"`
	Macros []Macro `json:"macros"`
}

// hostsResult wraps host.create / host.delete.
type hostsResult struct {
	HostIDs []string `json:"hostids"`
}

// FetchHostGroup returns the hostgroup by name, or nil when absent.
func FetchHostGroup(ctx context.Context, httpClient *http.Client, endpoint, token, name string) (*hostGroupResult, error) {
	raw, err := callJSONRPC[[]hostGroupResult](ctx, httpClient, endpoint, token, "hostgroup.get", map[string]any{
		paramOutput: outputExtend,
		paramFilter: map[string]any{paramName: []string{name}},
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return &raw[0], nil
}

// CreateHostGroup creates the host group and returns its id.
func CreateHostGroup(ctx context.Context, httpClient *http.Client, endpoint, token, name string) (int64, error) {
	created, err := callJSONRPC[hostGroupsResult](ctx, httpClient, endpoint, token, "hostgroup.create", map[string]any{
		"name": name,
	})
	if err != nil {
		return 0, err
	}
	return parseID(created.GroupIDs, "groupids")
}

// FetchTemplateID resolves a template id by name. Returns 0 when the
// template is not present (the "Kubernetes Cluster by HTTP" template is
// bundled with Zabbix 8.0 but a stripped-down installation may lack it).
func FetchTemplateID(ctx context.Context, httpClient *http.Client, endpoint, token, name string) (int64, error) {
	raw, err := callJSONRPC[[]templateResult](ctx, httpClient, endpoint, token, "template.get", map[string]any{
		paramOutput: outputExtend,
		paramFilter: map[string]any{paramHost: []string{name}},
	})
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}
	return raw[0].ID, nil
}

// FetchClusterHost returns the cluster host by technical name, or nil
// when absent.
func FetchClusterHost(ctx context.Context, httpClient *http.Client, endpoint, token, host string) (*hostResult, error) {
	raw, err := callJSONRPC[[]hostResult](ctx, httpClient, endpoint, token, "host.get", map[string]any{
		paramOutput:             []string{"hostid", "host"},
		paramFilter:             map[string]any{paramHost: []string{host}},
		"selectParentTemplates": []string{paramTemplateID},
		"selectMacros":          []string{paramMacro, paramValue},
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return &raw[0], nil
}

// clusterMacros builds the desired host macro set. The host is
// operator-managed: passing "macros" on update replaces the full set.
func clusterMacros(tokenValue, tlsVerify string) []Macro {
	return []Macro{
		{Macro: MacroKubeAPIToken, Value: tokenValue},
		{Macro: MacroHTTPTLSVerify, Value: tlsVerify},
	}
}

// macrosEqual compares desired against live macros for the two
// operator-managed macros only (other macros, if any, are not ours).
func macrosEqual(desired []Macro, live []Macro) bool {
	liveByKey := make(map[string]string, len(live))
	for _, m := range live {
		liveByKey[m.Macro] = m.Value
	}
	for _, m := range desired {
		if v, ok := liveByKey[m.Macro]; !ok || v != m.Value {
			return false
		}
	}
	return true
}

// EnsureClusterHost idempotently registers the cluster host: ensures the
// host group and the template exist, creates the host (with the dummy
// agent interface the lab-verified setup carries) or realigns an
// existing host's template linkage and macros. Returns the host id and
// whether the host was created in this call.
func EnsureClusterHost(ctx context.Context, httpClient *http.Client, endpoint, token string,
	clusterName, monitorToken, tlsVerify string) (int64, bool, error) {
	group, err := FetchHostGroup(ctx, httpClient, endpoint, token, ClusterHostGroup)
	if err != nil {
		return 0, false, fmt.Errorf("hostgroup.get: %w", err)
	}
	if group == nil {
		groupID, err := CreateHostGroup(ctx, httpClient, endpoint, token, ClusterHostGroup)
		if err != nil {
			return 0, false, fmt.Errorf("hostgroup.create: %w", err)
		}
		group = &hostGroupResult{ID: groupID, Name: ClusterHostGroup}
	}

	templateID, err := FetchTemplateID(ctx, httpClient, endpoint, token, ClusterTemplate)
	if err != nil {
		return 0, false, fmt.Errorf("template.get: %w", err)
	}
	if templateID == 0 {
		return 0, false, fmt.Errorf("template %q is not installed on this Zabbix", ClusterTemplate)
	}

	desired := clusterMacros(monitorToken, tlsVerify)

	host, err := FetchClusterHost(ctx, httpClient, endpoint, token, clusterName)
	if err != nil {
		return 0, false, fmt.Errorf("host.get: %w", err)
	}
	if host == nil {
		created, err := callJSONRPC[hostsResult](ctx, httpClient, endpoint, token, "host.create", map[string]any{
			"host": clusterName,
			"groups": []map[string]any{
				{"groupid": intToStringID(group.ID)},
			},
			"templates": []map[string]any{
				{"templateid": intToStringID(templateID)},
			},
			// HTTP-agent-only host: the template has no agent items,
			// but Zabbix wants at least one interface on the host.
			"interfaces": []map[string]any{
				{
					"type": "1", "main": "1", "useip": "1",
					"ip": "127.0.0.1", "dns": "", "port": "10050",
				},
			},
			"macros": desired,
		})
		if err != nil {
			return 0, false, fmt.Errorf("host.create: %w", err)
		}
		id, err := parseID(created.HostIDs, "hostids")
		return id, true, err
	}

	linked := false
	for _, t := range host.Templates {
		if t.ID == templateID {
			linked = true
			break
		}
	}
	if !linked || !macrosEqual(desired, host.Macros) {
		params := map[string]any{
			"hostid": intToStringID(host.ID),
			"templates": []map[string]any{
				{"templateid": intToStringID(templateID)},
			},
			"macros": desired,
		}
		if _, err := callJSONRPC[hostsResult](ctx, httpClient, endpoint, token, "host.update", params); err != nil {
			return 0, false, fmt.Errorf("host.update: %w", err)
		}
	}
	return host.ID, false, nil
}

// DeleteClusterHost removes the cluster host (finalizer cleanup). A
// missing host is not an error: someone may have deleted it manually.
func DeleteClusterHost(ctx context.Context, httpClient *http.Client, endpoint, token, clusterName string) error {
	host, err := FetchClusterHost(ctx, httpClient, endpoint, token, clusterName)
	if err != nil {
		return fmt.Errorf("host.get: %w", err)
	}
	if host == nil {
		return nil
	}
	if _, err := callJSONRPC[hostsResult](ctx, httpClient, endpoint, token, "host.delete", []string{intToStringID(host.ID)}); err != nil {
		return fmt.Errorf("host.delete: %w", err)
	}
	return nil
}
