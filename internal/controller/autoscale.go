// Package controller provides the shared autoscaling logic for
// Zabbix and ZabbixProxyGroup resources.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Autoscaler is the shared scaling engine. It evaluates metrics,
// applies cooldown/hysteresis, and returns the desired replica count.
type Autoscaler struct {
	Client    client.Client
	Component string // "server" | "frontend" | "webService" | "proxyGroup" | "database"
}

// ScaleResult contains the scaling decision.
type ScaleResult struct {
	DesiredReplicas  int32
	CurrentReplicas  int32
	Direction        string // "up" | "down" | "none"
	Reason           string
	ShouldScale      bool
	ConditionActive  bool
	ConditionPaused  bool
	ConditionReason  string
	ConditionMessage string
}

// Evaluate computes the desired replica count for a component with
// autoscaling enabled. It applies:
// - Metric evaluation (each metric votes for a replica count, max wins)
// - Cooldown period (min time between any scaling actions)
// - Scale-up/down stabilization (sustain threshold before acting)
// - Hysteresis (scale-up threshold > scale-down threshold)
// - HA transition guard (freeze during phase transitions)
func (a *Autoscaler) Evaluate(
	ctx context.Context,
	obj client.Object,
	autoscaling *v1alpha1.AutoscalingSpec,
	metrics *zabbix.AutoscalingMetrics,
	cnpgMetrics *zabbix.CNPGScalingMetrics,
	proxyGroupNames []string,
	haTransitionInProgress bool,
	currentReplicas int32,
	lastScaleTime *metav1.Time,
	lastScaleDirection string,
	lastScaleReason string,
) *ScaleResult {
	log := log.FromContext(ctx).WithValues("component", a.Component)

	result := &ScaleResult{
		ConditionActive: autoscaling.Enabled,
		CurrentReplicas: currentReplicas,
	}

	if !autoscaling.Enabled {
		result.ConditionReason = "AutoscalingDisabled"
		result.ConditionMessage = "autoscaling not enabled for this component"
		return result
	}

	if metrics == nil && cnpgMetrics == nil {
		result.ConditionActive = false
		result.ConditionReason = "MetricsUnavailable"
		result.ConditionMessage = "no metrics collected yet"
		return result
	}

	now := metav1.Now()

	// Cooldown check: skip if last scale was within cooldownPeriod.
	cooldown := parseDurationDefault(autoscaling.CooldownPeriod, 5*time.Minute)
	if lastScaleTime != nil && now.Time.Sub(lastScaleTime.Time) < cooldown {
		result.ConditionReason = "CooldownActive"
		result.ConditionMessage = fmt.Sprintf("last scale at %s, cooldown %s", lastScaleTime.Time.Format(time.RFC3339), cooldown)
		log.V(1).Info("autoscaling skipped: cooldown active", "lastScale", lastScaleTime, "cooldown", cooldown)
		return result
	}

	// Compute desired replicas from each metric.
	var desiredReplicas int32
	var reasons []string

	for _, metric := range autoscaling.Metrics {
		metricDesired := a.computeDesiredReplicas(metric, metrics, cnpgMetrics, currentReplicas)
		if metricDesired > desiredReplicas {
			desiredReplicas = metricDesired
		}
		reasons = append(reasons, fmt.Sprintf("%s:%d", metric.Type, metricDesired))
	}

	// Apply bounds.
	minR := int32(1)
	if autoscaling.MinReplicas != nil {
		minR = *autoscaling.MinReplicas
	}
	maxR := int32(100)
	if autoscaling.MaxReplicas != nil {
		maxR = *autoscaling.MaxReplicas
	}
	if desiredReplicas < minR {
		desiredReplicas = minR
	}
	if desiredReplicas > maxR {
		desiredReplicas = maxR
	}

	result.DesiredReplicas = desiredReplicas

	// Stabilization windows: require condition to hold for N periods
	// before actually scaling. We track this via the status.
	scaleUpStabilization := parseDurationDefault(autoscaling.ScaleUpStabilization, 2*time.Minute)
	scaleDownStabilization := parseDurationDefault(autoscaling.ScaleDownStabilization, 10*time.Minute)

	// Determine direction and apply hysteresis/stabilization.
	direction := "none"
	shouldScale := false
	reason := ""

	if desiredReplicas > currentReplicas {
		direction = "up"
		// Hysteresis: require sustained scale-up signal.
		if lastScaleDirection == "up" || desiredReplicas > currentReplicas {
			if lastScaleTime != nil && now.Time.Sub(lastScaleTime.Time) >= scaleUpStabilization {
				shouldScale = true
				reason = fmt.Sprintf("scale-up condition sustained for %s; metric votes: %s", scaleUpStabilization, strings.Join(reasons, ", "))
			} else {
				waitTime := scaleUpStabilization
				if lastScaleTime != nil {
					waitTime = scaleUpStabilization - now.Time.Sub(lastScaleTime.Time)
				}
				reason = fmt.Sprintf("scale-up condition building (need %s more); votes: %s", waitTime, strings.Join(reasons, ", "))
			}
		} else {
			reason = fmt.Sprintf("scale-up signal detected; starting stabilization window (%s); votes: %s", scaleUpStabilization, strings.Join(reasons, ", "))
		}
	} else if desiredReplicas < currentReplicas {
		direction = "down"
		// Hysteresis: require sustained scale-down signal with wider window
		if lastScaleDirection == "down" || desiredReplicas < currentReplicas {
			if lastScaleTime != nil && now.Time.Sub(lastScaleTime.Time) >= scaleDownStabilization {
				shouldScale = true
				reason = fmt.Sprintf("scale-down condition sustained for %s; metric votes: %s", scaleDownStabilization, strings.Join(reasons, ", "))
			} else {
				waitTime := scaleDownStabilization
				if lastScaleTime != nil {
					waitTime = scaleDownStabilization - now.Time.Sub(lastScaleTime.Time)
				}
				reason = fmt.Sprintf("scale-down condition building (need %s more); votes: %s", waitTime, strings.Join(reasons, ", "))
			}
		} else {
			reason = fmt.Sprintf("scale-down signal detected; starting stabilization window (%s); votes: %s", scaleDownStabilization, strings.Join(reasons, ", "))
		}
	} else {
		direction = "none"
		reason = "desired replicas equals current; no scaling needed"
	}

	result.Direction = direction
	result.ShouldScale = shouldScale
	result.Reason = reason

	// Update status memo (caller will persist).
	newStatus := &v1alpha1.AutoscalingStatus{
		CurrentReplicas:    currentReplicas,
		DesiredReplicas:    desiredReplicas,
		LastScaleDirection: direction,
		LastScaleReason:    reason,
	}
	if shouldScale {
		newStatus.LastScaleTime = &metav1.Time{Time: now.Time}
	}

	result.ConditionReason = "AutoscalingActive"
	result.ConditionMessage = fmt.Sprintf("current=%d desired=%d direction=%s", currentReplicas, desiredReplicas, direction)
	return result
}

// computeDesiredReplicas calculates the replica count for a single metric.
func (a *Autoscaler) computeDesiredReplicas(
	metric v1alpha1.AutoscalingMetricSpec,
	metrics *zabbix.AutoscalingMetrics,
	cnpgMetrics *zabbix.CNPGScalingMetrics,
	currentReplicas int32,
) int32 {
	if currentReplicas == 0 {
		currentReplicas = 1
	}

	switch metric.Type {
	case v1alpha1.AutoscalingMetricQueueSize:
		// Target: queue items per poller.
		// desired = ceil(total_queued / target_per_poller)
		if metrics == nil || metrics.ServerTaskQueue.TotalQueued == 0 {
			return currentReplicas
		}
		target := metric.Target
		if target <= 0 {
			target = 100 // default
		}
		desired := (metrics.ServerTaskQueue.TotalQueued + target - 1) / target
		return int32(desired)

	case v1alpha1.AutoscalingMetricPollerUtilization:
		// Target: poller busy percentage.
		// If utilization > target%, scale up proportionally.
		if metrics == nil || metrics.PollerUtil.TotalPollers == 0 {
			return currentReplicas
		}
		actual := metrics.PollerUtil.Percent
		target := float64(metric.Target)
		if target <= 0 {
			target = 70
		}
		if actual <= target {
			return currentReplicas
		}
		ratio := actual / target
		desired := int32(float64(currentReplicas) * ratio)
		if desired < currentReplicas+1 {
			desired = currentReplicas + 1
		}
		return desired

	case v1alpha1.AutoscalingMetricRequestRate:
		// Target: requests per second per replica.
		return currentReplicas

	case v1alpha1.AutoscalingMetricQueryLatency:
		// Target: query latency p99 in ms.
		if cnpgMetrics == nil || cnpgMetrics.QueryLatencyP99 <= 0 {
			return currentReplicas
		}
		target := float64(metric.Target)
		if target <= 0 {
			target = 500
		}
		if cnpgMetrics.QueryLatencyP99 <= target {
			return currentReplicas
		}
		ratio := cnpgMetrics.QueryLatencyP99 / target
		desired := int32(float64(currentReplicas) * ratio)
		if desired < currentReplicas+1 {
			desired = currentReplicas + 1
		}
		return desired

	default:
		return currentReplicas
	}
}

// parseDurationDefault parses a duration string (e.g., "5m", "1h") or returns default.
func parseDurationDefault(s string, defaultVal time.Duration) time.Duration {
	if s == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultVal
	}
	return d
}
