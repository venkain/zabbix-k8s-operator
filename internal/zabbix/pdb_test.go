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
	"maps"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// TestDisruptionBudgets verifies the anti-wedge disruption policy: at
// most one pod down at a time, unhealthy runs of pods evictable without
// consuming the budget, and workload-aligned naming/selection.
func TestDisruptionBudgets(t *testing.T) {
	z := testZabbix()

	pg := &zabbixv1alpha1.ZabbixProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: testNamespace},
		Spec:       zabbixv1alpha1.ZabbixProxyGroupSpec{GroupName: "edge-group"},
	}

	budgets := map[string]struct {
		budget    *policyv1.PodDisruptionBudget
		name      string
		instance  string
		component string
	}{
		"server":       {ServerDisruptionBudget(z), testInstance + "-server", testInstance, ComponentServer},
		"frontend":     {FrontendDisruptionBudget(z), testInstance + "-frontend", testInstance, ComponentFrontend},
		"web-service":  {WebServiceDisruptionBudget(z), "test-zabbix-web-service", testInstance, ComponentWebService},
		"proxy member": {ProxyDisruptionBudget(pg), "edge-proxy", "edge", ComponentProxy},
	}

	for _, want := range budgets {
		b := want.budget
		if b.Name != want.name || b.Labels[InstanceLabel] != want.instance {
			t.Errorf("%s budget: name=%q instance label=%q, want %q/%q",
				want.component, b.Name, b.Labels[InstanceLabel], want.name, want.instance)
		}
		if got := b.Spec.MaxUnavailable.IntValue(); got != 1 {
			t.Errorf("%s budget: maxUnavailable=%d, want 1 (drains must not wedge)", want.component, got)
		}
		if b.Spec.UnhealthyPodEvictionPolicy == nil || *b.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
			t.Errorf("%s budget: unhealthy eviction policy %v, want AlwaysAllow", want.component, b.Spec.UnhealthyPodEvictionPolicy)
		}
		if matchLabels := b.Spec.Selector.MatchLabels; !maps.Equal(matchLabels, selectorLabels(want.instance, want.component)) {
			t.Errorf("%s budget: pod selector %v, want %v", want.component, matchLabels, selectorLabels(want.instance, want.component))
		}
	}
}
