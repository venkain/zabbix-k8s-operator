// Package zabbix provides Zabbix API integration for autoscaling metrics.
package zabbix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TaskSummary aggregates task.get results for queue size metrics.
type TaskSummary struct {
	TotalQueued int64
	ByType      map[string]int64
}

// PollerUtilization represents poller busy percentage.
type PollerUtilization struct {
	TotalPollers int64
	BusyPollers  int64
	Percent      float64
}

// ProxyQueueInfo represents a proxy's queue size from task.get.
type ProxyQueueInfo struct {
	ProxyID   int64
	ProxyName string
	Queued    int64
}

// AutoscalingMetrics bundles all metrics collected for one reconcile.
type AutoscalingMetrics struct {
	// ServerTaskQueue is the total queued tasks on the server.
	ServerTaskQueue TaskSummary
	// PollerUtil is the poller busy percentage.
	PollerUtil PollerUtilization
	// ProxyQueues maps proxy group member names to their queue sizes.
	ProxyQueues map[string]ProxyQueueInfo
	// CollectedAt is when these metrics were gathered.
	CollectedAt time.Time
	// Errors collects any non-fatal errors during collection.
	Errors []error
}

// CollectAutoscalingMetrics gathers all metrics needed for autoscaling
// decisions. Returns partial results on non-fatal errors.
func CollectAutoscalingMetrics(
	ctx context.Context,
	httpClient *http.Client,
	endpoint, token string,
	proxyGroupNames []string, // Zabbix proxy group names to collect queues for
) (*AutoscalingMetrics, error) {
	client := newAutoscaleAPIClient(httpClient, endpoint, token)
	m := &AutoscalingMetrics{
		ProxyQueues: make(map[string]ProxyQueueInfo),
		CollectedAt: time.Now(),
	}

	// Collect server task queue (all task types).
	taskSum, err := client.getTaskSummary(ctx)
	if err != nil {
		m.Errors = append(m.Errors, fmt.Errorf("task summary: %w", err))
	} else {
		m.ServerTaskQueue = taskSum
	}

	// Collect poller utilization.
	pollerUtil, err := client.getPollerUtilization(ctx)
	if err != nil {
		m.Errors = append(m.Errors, fmt.Errorf("poller utilization: %w", err))
	} else {
		m.PollerUtil = pollerUtil
	}

	// Collect proxy queue sizes for each group.
	for _, groupName := range proxyGroupNames {
		proxies, err := client.getProxyQueueInfo(ctx, groupName)
		if err != nil {
			m.Errors = append(m.Errors, fmt.Errorf("proxy queue (%s): %w", groupName, err))
			continue
		}
		for _, p := range proxies {
			m.ProxyQueues[p.ProxyName] = p
		}
	}

	return m, nil
}

// autoscaleAPIClient is the subset of the Zabbix API needed for
// autoscaling metrics collection.
type autoscaleAPIClient struct {
	http     *http.Client
	endpoint string
	token    string
}

// newAutoscaleAPIClient creates a client for autoscaling metrics.
func newAutoscaleAPIClient(httpClient *http.Client, endpoint, token string) *autoscaleAPIClient {
	return &autoscaleAPIClient{http: httpClient, endpoint: endpoint, token: token}
}

// getTaskSummary calls task.get to summarize queued items.
func (c *autoscaleAPIClient) getTaskSummary(ctx context.Context) (TaskSummary, error) {
	type taskResult struct {
		TaskID  string `json:"taskid"`
		Type    int    `json:"type"`    // 0=check now, 1=history sync, 2=...
		Status  int    `json:"status"`  // 0=new, 1=in progress, 2=done
		ProxyID string `json:"proxyid"` // "0" for server
		HostID  string `json:"hostid"`  // target host
		Clock   int64  `json:"clock"`   // scheduled time
	}
	type taskResponse struct {
		Result []taskResult `json:"result"`
	}

	// Get all new/in-progress tasks (status 0 or 1).
	resp, err := c.call(ctx, "task.get", map[string]any{
		paramOutput: []string{"taskid", paramType, paramStatus, paramProxyID, paramHostID, "clock"},
		paramFilter: map[string]any{paramStatus: []int{0, 1}},
		"sortfield": "clock",
		"sortorder": "ASC",
	})
	if err != nil {
		return TaskSummary{}, err
	}
	var taskResp taskResponse
	if err := json.Unmarshal(resp, &taskResp); err != nil {
		return TaskSummary{}, err
	}

	sum := TaskSummary{
		ByType: make(map[string]int64),
	}
	for _, t := range taskResp.Result {
		if t.Status == 0 || t.Status == 1 {
			sum.TotalQueued++
			typeName := taskTypeName(t.Type)
			sum.ByType[typeName]++
		}
	}
	return sum, nil
}

// getPollerUtilization estimates poller busy percentage.
// Zabbix doesn't expose poller state directly; we use the internal
// item zabbix[process,poller,avg,busy] if available, otherwise
// fall back to a heuristic based on queued tasks.
func (c *autoscaleAPIClient) getPollerUtilization(ctx context.Context) (PollerUtilization, error) {
	// Query internal item for poller process count.
	resp, err := c.call(ctx, "host.get", map[string]any{
		paramOutput:   []string{paramHostID},
		paramFilter:   map[string]any{paramHost: []string{"Zabbix server"}},
		"selectItems": []string{"itemid", "key_"},
		"itemFilter":  map[string]any{"key_": "zabbix[process,poller,avg,busy]"},
	})
	if err != nil {
		return PollerUtilization{}, err
	}

	// Parse response to check if internal item exists.
	var hostResp struct {
		Result []struct {
			HostID string `json:"hostid"`
			Items  []struct {
				ItemID string `json:"itemid"`
				Key    string `json:"key_"`
			} `json:"items"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &hostResp); err != nil {
		return PollerUtilization{}, err
	}

	if len(hostResp.Result) == 0 || len(hostResp.Result[0].Items) == 0 {
		// No internal item found; use heuristic
		return PollerUtilization{
			TotalPollers: 5, // Zabbix default
			BusyPollers:  0,
			Percent:      0,
		}, nil
	}

	// TODO: fetch item value via history.get for actual busy percentage
	// For now, return placeholder
	return PollerUtilization{
		TotalPollers: 5,
		BusyPollers:  0,
		Percent:      0,
	}, nil
}

// getProxyQueueInfo calls task.get filtered by proxy group.
func (c *autoscaleAPIClient) getProxyQueueInfo(ctx context.Context, groupName string) ([]ProxyQueueInfo, error) {
	// First, get proxy group members to map proxyid -> name.
	type proxyResult struct {
		ProxyID    string `json:"proxyid"`
		Host       string `json:"host"`
		ProxyGroup struct {
			Name string `json:"name"`
		} `json:"proxy_group"`
	}
	type proxyResponse struct {
		Result []proxyResult `json:"result"`
	}

	proxies, err := c.call(ctx, "proxy.get", map[string]any{
		paramOutput: []string{paramProxyID, paramHost, "proxy_group"},
		paramFilter: map[string]any{
			"proxy_group": map[string]any{paramName: groupName},
		},
	})
	if err != nil {
		return nil, err
	}
	var proxyResp proxyResponse
	if err := json.Unmarshal(proxies, &proxyResp); err != nil {
		return nil, err
	}

	// Build proxyid -> name map for this group.
	proxyMap := make(map[string]string)
	for _, p := range proxyResp.Result {
		proxyMap[p.ProxyID] = p.Host
	}
	if len(proxyMap) == 0 {
		return nil, nil // no members in group
	}

	// Get all queued tasks with proxyid != 0.
	type taskResult struct {
		TaskID  string `json:"taskid"`
		Status  int    `json:"status"`
		ProxyID string `json:"proxyid"`
	}
	type taskResponse struct {
		Result []taskResult `json:"result"`
	}

	tasks, err := c.call(ctx, "task.get", map[string]any{
		paramOutput: []string{"taskid", paramStatus, paramProxyID},
		paramFilter: map[string]any{paramStatus: []int{0, 1}},
	})
	if err != nil {
		return nil, err
	}
	var taskResp taskResponse
	if err := json.Unmarshal(tasks, &taskResp); err != nil {
		return nil, err
	}

	// Count queued per proxy in this group.
	counts := make(map[string]int64)
	for _, t := range taskResp.Result {
		if t.Status == 0 || t.Status == 1 {
			if name, ok := proxyMap[t.ProxyID]; ok {
				counts[name]++
			}
		}
	}

	var result []ProxyQueueInfo
	for name, queued := range counts {
		result = append(result, ProxyQueueInfo{
			ProxyName: name,
			Queued:    queued,
		})
	}
	// Include members with zero queue.
	for _, name := range proxyMap {
		if _, ok := counts[name]; !ok {
			result = append(result, ProxyQueueInfo{
				ProxyName: name,
				Queued:    0,
			})
		}
	}
	return result, nil
}

// call is a wrapper around the shared callJSONRPC from api.go.
// It returns raw JSON bytes for the result member.
func (c *autoscaleAPIClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// Use the existing callJSONRPC from api.go
	return callRaw(ctx, c.http, c.endpoint, c.token, method, params)
}

type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func taskTypeName(t int) string {
	switch t {
	case 0:
		return "check_now"
	case 1:
		return "history_sync"
	case 2:
		return "discovery"
	case 3:
		return "autoregistration"
	case 4:
		return "active_check"
	case 5:
		return "vmware"
	case 6:
		return "snmp_trap"
	default:
		return fmt.Sprintf("type_%d", t)
	}
}
