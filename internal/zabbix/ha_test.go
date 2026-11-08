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

import "testing"

// labScaleUpRegistry is the registry shape mid-transition: the old
// standalone node (empty name, still active) plus the first managed
// node on standby.
var labScaleUpRegistry = []HANode{
	{Name: "", Address: "localhost", Status: HANodeStateActive},
	{Name: testHANode0, Address: "10.0.0.1", Status: HANodeStateStandby},
	{Name: testHANode1, Address: "10.0.0.2", Status: HANodeStateActive, LastAccess: 200},
	{Name: "zbx-server-2", Address: "10.0.0.3", Status: HANodeStateStandby},
}

// Named registry nodes reused across the mapping tests.
const (
	testHANode0 = "zbx-server-0"
	testHANode1 = "zbx-server-1"
)

func TestActiveHANode(t *testing.T) {
	active := ActiveHANode(labScaleUpRegistry)
	if active == nil || active.Name != testHANode1 {
		t.Fatalf("active = %+v, want zbx-server-1", active)
	}

	// No active node at all.
	none := []HANode{
		{Name: "a", Status: HANodeStateStandby},
		{Name: "b", Status: HANodeStateStopped},
	}
	if got := ActiveHANode(none); got != nil {
		t.Errorf("active = %+v, want nil", got)
	}

	// Registry anomaly: two actives — the newest heartbeat wins.
	split := []HANode{
		{Name: "old-active", Status: HANodeStateActive, LastAccess: 100},
		{Name: "new-active", Status: HANodeStateActive, LastAccess: 500},
	}
	if got := ActiveHANode(split); got == nil || got.Name != "new-active" {
		t.Errorf("active = %+v, want new-active (most recent heartbeat)", got)
	}
}

func TestDesiredHARoleLabel(t *testing.T) {
	// Empty-name standalone entry matches no pod.
	if _, set := DesiredHARoleLabel("zbx-server-0", labScaleUpRegistry); !set {
		t.Fatal("zbx-server-0 must have a label")
	}
	if role, _ := DesiredHARoleLabel("zbx-server-0", labScaleUpRegistry); role != "standby" {
		t.Errorf("role = %q, want standby", role)
	}
	if role, _ := DesiredHARoleLabel(testHANode1, labScaleUpRegistry); role != HARoleActive {
		t.Errorf("role = %q, want active", role)
	}

	// Pod with no registry entry: no label.
	if _, set := DesiredHARoleLabel("zbx-server-9", labScaleUpRegistry); set {
		t.Error("unknown pod must not get a label")
	}

	// Unknown status never routes into rotation.
	unknown := []HANode{{Name: "p", Status: HANodeStateUnknown}}
	if _, set := DesiredHARoleLabel("p", unknown); set {
		t.Error("unknown-state node must not get a label")
	}

	// Non-active lifecycle states keep the pod out of the active Service.
	for _, tc := range []struct{ state HANodeState }{
		{HANodeStateStopped}, {HANodeStateUnavailable},
	} {
		got, set := DesiredHARoleLabel("p", []HANode{{Name: "p", Status: tc.state}})
		if !set || got == HARoleActive {
			t.Errorf("state %q → label %q (set=%v), must be non-active", tc.state, got, set)
		}
	}
}

func TestServerHAStatusFromRegistry(t *testing.T) {
	st := ServerHAStatusFromRegistry(labScaleUpRegistry)
	if st == nil || st.ActiveNode != testHANode1 {
		t.Fatalf("activeNode = %+v", st)
	}
	if len(st.Nodes) != len(labScaleUpRegistry) {
		t.Fatalf("nodes = %+v, want %d", st.Nodes, len(labScaleUpRegistry))
	}
	if st.Nodes[0].Status != string(HANodeStateActive) || st.Nodes[0].Name != "" {
		t.Errorf("node 0 = %+v", st.Nodes[0])
	}

	// nil = no registry information (standalone mode).
	if got := ServerHAStatusFromRegistry(nil); got != nil {
		t.Errorf("nil registry must produce nil status, got %+v", got)
	}
}

func TestServerHAStatusEmptyRegistry(t *testing.T) {
	st := ServerHAStatusFromRegistry([]HANode{})
	if st == nil {
		t.Fatal("empty registry must produce a status (API reachable), not nil")
	}
	if st.ActiveNode != "" || len(st.Nodes) != 0 {
		t.Errorf("status = %+v, want empty", st)
	}
}
