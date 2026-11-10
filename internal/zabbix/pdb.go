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
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// disruptionMaxUnavailable is the voluntary-disruption budget applied
// to every workload the operator owns: drains (node upgrades, host
// maintenance) proceed one pod at a time and can advance while pods
// recover, so a disruption can never wedge the whole workload — the
// same anti-wedge philosophy as stale-revision pod recycling. For a
// single-replica workload this allows its one pod to be evicted:
// minAvailable=1 there would block drains indefinitely (a wedge).
const disruptionMaxUnavailable = int32(1)

// unhealthyPodEvictionPolicy lets the eviction API delete running-but-
// unready pods (crash loops, image pulls) immediately: they are about
// to be replaced anyway, and skipping them frees the disruption budget
// for healthy pods instead of wedging a drain behind them.
const unhealthyPodEvictionPolicy = policyv1.AlwaysAllow

// DisruptionBudget builds the PodDisruptionBudget protecting one
// workload (selector labels selects the workload's pods): at most one
// pod down at a time during voluntary evictions.
func DisruptionBudget(namespace, instance, component, name string, podSelector map[string]string) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(disruptionMaxUnavailable)
	unhealthyPolicy := unhealthyPodEvictionPolicy
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    commonLabels(instance, component),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable:             &maxUnavailable,
			UnhealthyPodEvictionPolicy: &unhealthyPolicy,
			Selector:                   &metav1.LabelSelector{MatchLabels: podSelector},
		},
	}
}

// ServerDisruptionBudget guards the server StatefulSet (name matches
// the workload object; namespaces are per kind).
func ServerDisruptionBudget(z *zabbixv1alpha1.Zabbix) *policyv1.PodDisruptionBudget {
	return DisruptionBudget(z.Namespace, z.Name, ComponentServer, ServerName(z.Name), selectorLabels(z.Name, ComponentServer))
}

// FrontendDisruptionBudget guards the frontend Deployment.
func FrontendDisruptionBudget(z *zabbixv1alpha1.Zabbix) *policyv1.PodDisruptionBudget {
	return DisruptionBudget(z.Namespace, z.Name, ComponentFrontend, FrontendName(z.Name), selectorLabels(z.Name, ComponentFrontend))
}

// WebServiceDisruptionBudget guards the web service Deployment.
func WebServiceDisruptionBudget(z *zabbixv1alpha1.Zabbix) *policyv1.PodDisruptionBudget {
	return DisruptionBudget(z.Namespace, z.Name, ComponentWebService, WebServiceName(z.Name), selectorLabels(z.Name, ComponentWebService))
}

// ProxyDisruptionBudget guards the proxy group member StatefulSet.
func ProxyDisruptionBudget(pg *zabbixv1alpha1.ZabbixProxyGroup) *policyv1.PodDisruptionBudget {
	return DisruptionBudget(pg.Namespace, pg.Name, ComponentProxy, ProxyName(pg.Name), selectorLabels(pg.Name, ComponentProxy))
}
