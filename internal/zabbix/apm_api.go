package zabbix

// APM server-side configuration through the JSON-RPC API. Two objects
// gate the proxy's OTLP collector beyond the operator-wired environment
// (verified in the lab against Zabbix 8.0):
//
//  1. the global APM data source — stored in the settings table row
//     apm_global_db (the "Administration → Data source" object), only
//     reachable through the settings API; no datasource.* methods exist.
//  2. the per-proxy APM switch — stored in the proxy table's apm column
//     and pushed to members through the config syncer; proxy.update
//     accepts the apm parameter even though proxy.get's output list
//     does not advertise it (and there is no selectAPM), so the current
//     state is not readable back.
//
// The collector's gRPC listener activates on the per-proxy
// data_collection_status transition (apm_manager_activate), writing to
// the local TelemetryProvider or, absent one, to the global data source.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// APM data-source status values (strings — the settings API stores
// booleans as JSON strings and returns them unchanged).
const (
	APMStatusEnabled  = "1"
	APMStatusDisabled = "0"
)

// APMGlobalDB is the global APM telemetry data source: where proxies
// without a local TelemetryProvider send their accepted OTLP data, and
// what the frontend's APM menu reads. The password is write-only — it
// is never returned by settings.get, so it is carried separately from
// the diffable fields.
type APMGlobalDB struct {
	// Status is "1" (enabled) or "0" (disabled).
	Status string
	// URL is the ClickHouse HTTP endpoint.
	URL string
	// Database is the ClickHouse database holding the otel_* tables.
	Database string
	// Username is the ClickHouse login.
	Username string
}

// apmGlobalDBResult mirrors the settings.get output shape (recorded in
// the lab: all fields come back as strings; authentication_type and the
// ssl_verify_* flags exist but are managed implicitly — password is
// write-only and never returned).
type apmGlobalDBResult struct {
	Status   string `json:"status"`
	URL      string `json:"url"`
	Database string `json:"db"`
	Username string `json:"username"`
}

// FetchAPMGlobalDB reads the global APM data source via settings.get.
// The response carries every setting keyed by name (strings and objects
// mixed), so the apm_global_db entry is picked out of raw JSON.
func FetchAPMGlobalDB(ctx context.Context, httpClient *http.Client, endpoint, token string) (*APMGlobalDB, error) {
	raw, err := callJSONRPC[map[string]json.RawMessage](ctx, httpClient, endpoint, token, "settings.get", map[string]any{})
	if err != nil {
		return nil, err
	}
	entry, ok := raw["apm_global_db"]
	if !ok {
		return nil, fmt.Errorf("settings.get response has no apm_global_db entry")
	}
	var r apmGlobalDBResult
	if err := json.Unmarshal(entry, &r); err != nil {
		return nil, fmt.Errorf("decoding apm_global_db setting: %w", err)
	}
	return &APMGlobalDB{
		Status:   r.Status,
		URL:      r.URL,
		Database: r.Database,
		Username: r.Username,
	}, nil
}

// UpdateAPMGlobalDB writes the global APM data source via settings.update
// (params {"apm_global_db": {...}}, response ["apm_global_db"]). The
// password is a write-only field and must be sent on every write; the
// disabled state clears url/db/username/password.
func UpdateAPMGlobalDB(ctx context.Context, httpClient *http.Client, endpoint, token, password string, ds APMGlobalDB) error {
	_, err := callJSONRPC[[]string](ctx, httpClient, endpoint, token, "settings.update", map[string]any{
		"apm_global_db": map[string]any{
			"status":              ds.Status,
			"url":                 ds.URL,
			"db":                  ds.Database,
			"authentication_type": "0",
			"username":            ds.Username,
			"password":            password,
		},
	})
	return err
}

// ProxyAPMUpdate is one member's per-proxy APM configuration for
// proxy.update.
type ProxyAPMUpdate struct {
	// ProxyID is the server-side proxy record id.
	ProxyID int64
	// Enabled turns the member's OTLP collector on (the listener
	// activates on this transition).
	Enabled bool
	// MaxMessagesPerSecond is the collector's message quota.
	MaxMessagesPerSecond int64
}

// UpdateProxiesAPM converges the per-proxy APM switches in one batched
// proxy.update call (params are an array of {proxyid, apm} objects).
func UpdateProxiesAPM(ctx context.Context, httpClient *http.Client, endpoint, token string, updates []ProxyAPMUpdate) error {
	params := make([]map[string]any, len(updates))
	for i, u := range updates {
		status, quota := APMStatusDisabled, "0"
		if u.Enabled {
			status, quota = APMStatusEnabled, strconv.FormatInt(u.MaxMessagesPerSecond, 10)
		}
		params[i] = map[string]any{
			"proxyid": strconv.FormatInt(u.ProxyID, 10),
			"apm": map[string]any{
				"data_collection_status":  status,
				"max_messages_per_second": quota,
			},
		}
	}
	_, err := callJSONRPC[idResult](ctx, httpClient, endpoint, token, "proxy.update", params)
	return err
}
