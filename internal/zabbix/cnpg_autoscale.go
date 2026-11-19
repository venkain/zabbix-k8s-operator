// Package zabbix provides CNPG autoscaling metrics collection.
package zabbix

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNPGScalingMetrics bundles CNPG read-replica metrics.
type CNPGScalingMetrics struct {
	// CurrentReadReplicas is the number of read replicas in the cluster.
	CurrentReadReplicas int32
	// QueryLatencyP99 is the p99 query latency in milliseconds.
	QueryLatencyP99 float64
	// ConnectionPoolSaturation is the PgBouncer pool usage (0-1).
	ConnectionPoolSaturation float64
	// CPUUtilization is the primary pod CPU usage (0-1).
	CPUUtilization float64
	// MemoryUtilization is the primary pod memory usage (0-1).
	MemoryUtilization float64
	// CollectedAt is when these metrics were gathered.
	CollectedAt time.Time
	// Errors collects any non-fatal errors.
	Errors []error
}

// CNPGScalerConfig configures how to collect CNPG metrics.
type CNPGScalerConfig struct {
	// ClusterName is the CNPG Cluster resource name.
	ClusterName string
	// Namespace is the cluster's namespace.
	Namespace string
	// KubeClient is the Kubernetes client for status subresources.
	KubeClient client.Client
	// PrometheusURL is optional; if set, query Prometheus for advanced metrics.
	PrometheusURL string
	// PrometheusBearerToken is optional auth for Prometheus.
	PrometheusBearerToken string
}

// CollectCNPGScalingMetrics gathers CNPG metrics for read-replica scaling.
// Returns partial results on non-fatal errors.
func CollectCNPGScalingMetrics(ctx context.Context, cfg *CNPGScalerConfig) (*CNPGScalingMetrics, error) {
	m := &CNPGScalingMetrics{
		CollectedAt: time.Now(),
	}

	if cfg.KubeClient == nil {
		m.Errors = append(m.Errors, fmt.Errorf("no kube client"))
		return m, nil
	}

	// 1. Get read replica count from Cluster status.
	// getCNPGCluster is a placeholder that always returns an error until
	// a proper CNPG dynamic client is integrated. For now, skip the
	// cluster fetch and use the fallback values.
	_ = getCNPGCluster        // silence unused warning
	m.CurrentReadReplicas = 0 // unknown

	// 2. If Prometheus configured, query advanced metrics.
	if cfg.PrometheusURL != "" {
		promMetrics, err := queryPrometheusCNPGMetrics(ctx, cfg)
		if err != nil {
			m.Errors = append(m.Errors, fmt.Errorf("prometheus query: %w", err))
		} else {
			m.QueryLatencyP99 = promMetrics.QueryLatencyP99
			m.ConnectionPoolSaturation = promMetrics.PoolSaturation
			m.CPUUtilization = promMetrics.CPUUtilization
			m.MemoryUtilization = promMetrics.MemoryUtilization
		}
	} else {
		// Fallback: basic heuristics from Cluster status.
		// Check if instances are Ready/Conditioned.
		m.ConnectionPoolSaturation = 0 // unknown
		m.QueryLatencyP99 = 0          // unknown
		m.CPUUtilization = 0
		m.MemoryUtilization = 0
	}

	return m, nil
}

// cnpCluster is a subset of the CNPG Cluster resource we need.
type cnpCluster struct {
	Metadata struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Instances int32 `json:"instances"`
	} `json:"spec"`
	Status struct {
		Phase          string `json:"phase"`
		Instances      int32  `json:"instances"`
		ReadyInstances int32  `json:"readyInstances"`
		Primary        string `json:"primary"`
		Replicas       []struct {
			Name   string `json:"name"`
			Role   string `json:"role"` // "primary" | "replica"
			Status string `json:"status"`
		} `json:"replicas"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// getCNPGCluster is a placeholder for future CNPG metrics integration.
// When implemented, it will fetch the CNPG Cluster resource and return
// parsed metrics. Currently returns a not-implemented error.
//
//nolint:unparam // placeholder always returns nil for first result
func getCNPGCluster(ctx context.Context, k8s client.Client, namespace, name string) (*cnpCluster, error) {
	_ = schema.GroupVersionResource{
		Group:    "postgresql.cnpg.io",
		Version:  "v1",
		Resource: "clusters",
	}
	_ = ctx
	_ = k8s
	_ = namespace
	_ = name
	return nil, fmt.Errorf("CNPG client not implemented; use CNPG's typed client")
}

// promCNPGMetrics holds metrics from Prometheus queries.
type promCNPGMetrics struct {
	QueryLatencyP99   float64
	PoolSaturation    float64
	CPUUtilization    float64
	MemoryUtilization float64
}

func queryPrometheusCNPGMetrics(ctx context.Context, cfg *CNPGScalerConfig) (*promCNPGMetrics, error) {
	// TODO: Implement Prometheus queries:
	// - pg_stat_database query latency p99
	// - pgbouncer pool saturation
	// - container CPU/memory from kubelet/cadvisor
	// Placeholder for now.
	return &promCNPGMetrics{}, nil
}
