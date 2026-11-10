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
	"testing"
	"time"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// TestMembersOnline verifies the official per-member online criterion:
// last contact within the failover window. No dedicated online flag
// exists on proxy objects (proxy.lastaccess only).
func TestMembersOnline(t *testing.T) {
	now := time.Unix(1000, 0) // the operator's clock
	delay := time.Minute

	member := func(name string, lastContact int64) ProxyMember {
		return ProxyMember{ID: 1, Name: name, LastContact: lastContact}
	}

	fresh := member("edge-proxy-0", now.Add(-30*time.Second).Unix()) // inside window
	stale := member("edge-proxy-1", now.Add(-10*time.Minute).Unix()) // outside window
	never := member("edge-proxy-2", 0)                               // never contacted
	foreign := member("foreign-proxy", now.Unix())                   // not this instance's

	got := MembersOnline([]ProxyMember{fresh, stale, never, foreign}, "edge", delay, now)
	if got != 1 {
		t.Errorf("MembersOnline = %d, want 1 (only the fresh member)", got)
	}
	if got := MembersOnline(nil, "edge", delay, now); got != 0 {
		t.Errorf("MembersOnline(empty) = %d, want 0", got)
	}
}

// TestMembersOnlineGrace verifies the clock-skew grace: a member whose
// last contact is very slightly past the window still counts, a clearly
// expired one does not.
func TestMembersOnlineGrace(t *testing.T) {
	now := time.Unix(1000, 0)
	delay := time.Minute

	edgeOfGrace := now.Unix() - int64((time.Minute + membersOnlineGrace).Seconds())
	justOutside := edgeOfGrace - 5 // five seconds past the window+grace

	if got := MembersOnline([]ProxyMember{{Name: "edge-proxy-0", LastContact: edgeOfGrace}}, "edge", delay, now); got != 1 {
		t.Errorf("member at the grace edge counted offline")
	}
	if got := MembersOnline([]ProxyMember{{Name: "edge-proxy-0", LastContact: justOutside}}, "edge", delay, now); got != 0 {
		t.Errorf("member past the grace edge counted online")
	}
}

// TestFailoverDelay verifies the spec parsing with the server default
// fallback.
func TestFailoverDelay(t *testing.T) {
	pg := func(delay string) *zabbixv1alpha1.ZabbixProxyGroup {
		return &zabbixv1alpha1.ZabbixProxyGroup{
			Spec: zabbixv1alpha1.ZabbixProxyGroupSpec{FailoverDelay: delay},
		}
	}

	if got := FailoverDelay(pg("30s")); got != 30*time.Second {
		t.Errorf("FailoverDelay(30s) = %v", got)
	}
	if got := FailoverDelay(pg("2m")); got != 2*time.Minute {
		t.Errorf("FailoverDelay(2m) = %v", got)
	}
	if got := FailoverDelay(pg("")); got != time.Minute {
		t.Errorf("FailoverDelay(empty) = %v, want the 1m server default", got)
	}
}
