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
	appsv1 "k8s.io/api/apps/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// ServerRolloutPhase names the step of the standalone↔HA transition the
// server workload is in. A Zabbix server cannot switch between standalone
// and HA mode in place: the official server refuses to start a HA node
// while a standalone registry entry is still active ("cannot change mode
// to HA while standalone node is active", validated in the lab), and a
// HA cluster scaled to a single node must be drained before the server
// may run outside HA mode again. The StatefulSet by itself cannot order
// this (its rollout may create the next ordinal before the predecessor
// rolled), so the operator phases the transition.
type ServerRolloutPhase string

const (
	// RolloutSteady: the live template already matches the desired mode;
	// apply the spec as configured.
	RolloutSteady ServerRolloutPhase = "Steady"

	// RolloutTemplateFirst: a standalone server must become a HA
	// cluster. Roll ordinal 0 into the HA identity first (replicas
	// capped at 1); the registry entry of the drained standalone node
	// is retired by its clean termination, and ordinal 0 re-registers
	// as a HA node.
	RolloutTemplateFirst ServerRolloutPhase = "TemplateFirst"

	// RolloutHoldHA: a HA template is live but the node registry gives
	// no usable information (API unreachable, or only entries without
	// a node name). Scale-outs are unsafe — hold the specimen count so
	// a crash-looping replica cannot poison the registry, and retry on
	// the next reconcile.
	RolloutHoldHA ServerRolloutPhase = "HoldHA"

	// RolloutScaleDownHA: replicas are being reduced while the HA
	// template runs. Keep the HA identity and let the StatefulSet stop
	// the highest ordinals; a clean stop hands the active role over to
	// a survivor without registry churn.
	RolloutScaleDownHA ServerRolloutPhase = "ScaleDownHA"

	// RolloutFlipStandalone: the last HA node (ordinal 0) is active and
	// no other node is — the HA identity can be retired. Switch the
	// template back to standalone and stamp the active role label so
	// the -active Service keeps routing without API access.
	RolloutFlipStandalone ServerRolloutPhase = "StandaloneFlip"
)

// StatefulSetHasHAIdentity reports whether the given StatefulSet's
// template carries the HA identity environment (a nil or non-server
// workload is standalone by definition).
func StatefulSetHasHAIdentity(sts *appsv1.StatefulSet) bool {
	if sts == nil {
		return false
	}
	for i := range sts.Spec.Template.Spec.Containers {
		container := &sts.Spec.Template.Spec.Containers[i]
		if container.Name != serverContainerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == envAutoHANodeName {
				return true
			}
		}
		return false
	}
	return false
}

// statefulSetReplicas returns the replica count of the live workload
// (1 when absent — the StatefulSet default).
func statefulSetReplicas(sts *appsv1.StatefulSet) int32 {
	if sts == nil || sts.Spec.Replicas == nil {
		return 1
	}
	return *sts.Spec.Replicas
}

// namedActiveNodes filters the registry to active nodes that carry a
// node name (the nameless entry of a standalone server matches no pod
// and cannot drive rollout decisions).
func namedActiveNodes(nodes []HANode) []HANode {
	var out []HANode
	for _, n := range nodes {
		if n.Status == HANodeStateActive && n.Name != "" {
			out = append(out, n)
		}
	}
	return out
}

// hasActiveNode reports whether the registry currently holds an active
// node of any kind (named or standalone nameless entry).
func hasActiveNode(nodes []HANode) bool {
	for i := range nodes {
		if nodes[i].Status == HANodeStateActive {
			return true
		}
	}
	return false
}

// ServerWorkloadForRollout shapes the server StatefulSet for the current
// transition step (see ServerRolloutPhase). inputs:
//   - the Zabbix spec (desired replicas / API config),
//   - the live server StatefulSet (nil when none exists yet),
//   - the hanode.get registry (nil when no information — API unreachable
//     or token not yet seeded).
//
// The returned StatefulSet is intended to be applied once by the
// controller during this reconcile (the steady-state ServerStatefulSet
// wrapper must not be applied in the same pass).
func ServerWorkloadForRollout(
	z *zabbixv1alpha1.Zabbix,
	db *DatabaseEnv,
	liveSTS *appsv1.StatefulSet,
	nodes []HANode,
) (*appsv1.StatefulSet, ServerRolloutPhase) {
	desired := ServerReplicas(z)
	liveHA := StatefulSetHasHAIdentity(liveSTS)
	liveReps := statefulSetReplicas(liveSTS)

	switch {
	case desired > 1:
		if !liveHA {
			// Standalone template (or workload absent). A fresh
			// install can go straight to the HA spec when no registry
			// entry is active at all — the first pod registers as the
			// founding node. Two blockers force the capped path,
			// because the server refuses to start in HA mode while a
			// standalone registry entry is active:
			//   - a live standalone workload must terminate cleanly
			//     first (its own pod retires the registry entry), or
			//   - an active registry entry without a live workload
			//     (residue of an unclean shutdown, or an unmanaged
			//     server sharing the database).
			if (liveSTS == nil || liveReps == 0) && !hasActiveNode(nodes) {
				return serverStatefulSet(z, db, desired, true), RolloutSteady
			}
			return serverStatefulSet(z, db, 1, true), RolloutTemplateFirst
		}
		// HA template live.
		if nodes == nil {
			// Registry unknown: hold, do not feed a possibly
			// crash-looping replica into the cluster.
			return serverStatefulSet(z, db, min32(desired, max32(liveReps, 1)), true), RolloutHoldHA
		}
		if actives := namedActiveNodes(nodes); len(actives) == 0 {
			// API reachable, but no named active node — the cluster is
			// founding or re-electing; do not scale out into that.
			return serverStatefulSet(z, db, 1, true), RolloutHoldHA
		}
		return serverStatefulSet(z, db, desired, true), RolloutSteady

	default: // desired <= 1
		if !liveHA {
			// Standalone template already: nothing to transition.
			return serverStatefulSet(z, db, desired, false), RolloutSteady
		}
		// HA template live, single replica desired. The higher
		// ordinals must be drained first (scale-down under HA
		// identity); only once the registry names ordinal 0 as the
		// sole active node may the template flip to standalone.
		if nodes == nil {
			return serverStatefulSet(z, db, 1, true), RolloutScaleDownHA
		}
		if active, set := DesiredHARoleLabel(ServerName(z.Name)+"-0", nodes); set && active == HARoleActive {
			return serverStatefulSet(z, db, 1, false), RolloutFlipStandalone
		}
		return serverStatefulSet(z, db, 1, true), RolloutScaleDownHA
	}
}

func min32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
