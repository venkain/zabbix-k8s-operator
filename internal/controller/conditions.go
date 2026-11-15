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

package controller

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// setCond upserts one condition on the given status conditions slice.
// Shared by the Zabbix and proxy group reconcilers (the condition
// type parameter accepts either controller's condition type).
func setCond[T ~string](cs *[]metav1.Condition, t T, s metav1.ConditionStatus, reason, message string, generation int64) {
	zabbix.SetCondition(cs, string(t), s, reason, message, generation)
}

// dropCondition removes one condition type from the given conditions
// slice — used when a feature is disabled and its (stale) condition
// must not linger.
func dropCondition[T ~string](cs *[]metav1.Condition, t T) {
	kept := make([]metav1.Condition, 0, len(*cs))
	for _, c := range *cs {
		if c.Type != string(t) {
			kept = append(kept, c)
		}
	}
	*cs = kept
}

// readyCondition maps a boolean readiness to the condition status.
func readyCondition(ready bool) metav1.ConditionStatus {
	if ready {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// statefulSetMessage is the condition message for StatefulSet-backed
// components.
func statefulSetMessage(component string, ready bool) string {
	if ready {
		return fmt.Sprintf("%s statefulset is ready", component)
	}
	return fmt.Sprintf("%s statefulset is not ready yet", component)
}

// deploymentMessage is the condition message for Deployment-backed
// components.
func deploymentMessage(component string, ready bool) string {
	if ready {
		return fmt.Sprintf("%s deployment is ready", component)
	}
	return fmt.Sprintf("%s deployment is not ready yet", component)
}
