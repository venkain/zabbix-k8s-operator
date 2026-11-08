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
	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// ActiveHANode returns the active node from the HA registry. Zabbix
// guarantees at most one active node; if a registry anomaly reports
// several, the most recent heartbeat wins (its holder serves traffic),
// and the tie is broken by name for determinism. Returns nil when no
// node is active.
func ActiveHANode(nodes []HANode) *HANode {
	var best *HANode
	for i := range nodes {
		n := &nodes[i]
		if n.Status != HANodeStateActive {
			continue
		}
		if best == nil || n.LastAccess > best.LastAccess ||
			(n.LastAccess == best.LastAccess && n.Name < best.Name) {
			best = n
		}
	}
	return best
}

// DesiredHARoleLabel returns the HA-role label value for a server pod
// and whether the label should be present at all. In operator-managed
// deployments the HA node name equals the pod name, so the role follows
// directly from the registry: the active node routes the -active
// Service, every other known state marks the pod as out of rotation.
// Pods without a registry entry (starting up, or the standalone entry
// whose name matches no pod) get no label.
func DesiredHARoleLabel(podName string, nodes []HANode) (string, bool) {
	for i := range nodes {
		if nodes[i].Name == podName {
			switch nodes[i].Status {
			case HANodeStateActive:
				return HARoleActive, true
			case HANodeStateStandby, HANodeStateStopped, HANodeStateUnavailable:
				return string(nodes[i].Status), true
			default:
				return "", false
			}
		}
	}
	return "", false
}

// ServerHAStatusFromRegistry builds the status subresource section from a
// hanode.get result. Returns nil while no registry information is known
// (standalone mode or API not yet reachable).
func ServerHAStatusFromRegistry(nodes []HANode) *zabbixv1alpha1.ServerHAStatus {
	if nodes == nil {
		return nil
	}
	out := &zabbixv1alpha1.ServerHAStatus{}
	if active := ActiveHANode(nodes); active != nil {
		out.ActiveNode = active.Name
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, zabbixv1alpha1.HANodeStatus{
			Name:    n.Name,
			Status:  string(n.Status),
			Address: n.Address,
		})
	}
	return out
}
