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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPodSecurityContext verifies the pod-level baseline: the
// RuntimeDefault seccomp profile on every deployed workload.
func TestPodSecurityContext(t *testing.T) {
	sc := PodSecurityContext()
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context seccomp profile %v, want RuntimeDefault", sc.SeccompProfile)
	}
	if sc.RunAsUser != nil || sc.RunAsGroup != nil {
		t.Errorf("pod security context pins uid/gid (%v/%v), want image-default (unverified images keep their user)",
			sc.RunAsUser, sc.RunAsGroup)
	}
}

// TestRestrictedSecurityContext verifies the restricted container
// baseline, including the numeric uid: kube-state-metrics' image
// declares a non-numeric USER (nobody), so runAsNonRoot is only
// verifiable with an explicit RunAsUser (the e2e-caught failure).
func TestRestrictedSecurityContext(t *testing.T) {
	sc := RestrictedSecurityContext(kubeStateMetricsUID)
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("restricted context runAsNonRoot=%v, want true", sc.RunAsNonRoot)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65534 {
		t.Errorf("restricted context runAsUser=%v, want 65534 (image's non-numeric nobody uid)", sc.RunAsUser)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("restricted context allowPrivilegeEscalation=%v, want false", sc.AllowPrivilegeEscalation)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("restricted context seccomp profile %v, want RuntimeDefault", sc.SeccompProfile)
	}
	if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 {
		t.Errorf("restricted context capabilities %v, want drop [ALL] with no adds", sc.Capabilities)
	}
}

// TestKubeStateMetricsSecurityWiring verifies the KSM deployment wires
// the pod-level baseline and the restricted container context together.
func TestKubeStateMetricsSecurityWiring(t *testing.T) {
	d := KubeStateMetricsDeployment(testZabbix())
	pod := d.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("KSM pod security context %v, want RuntimeDefault seccomp", pod.SecurityContext)
	}
	for _, c := range pod.Containers {
		if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 65534 {
			t.Errorf("KSM container %q security context %+v, want restricted with runAsUser 65534", c.Name, c.SecurityContext)
		}
	}
}
