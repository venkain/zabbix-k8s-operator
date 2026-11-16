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
	corev1 "k8s.io/api/core/v1"
)

// PodSecurityContext returns the pod-level security baseline applied to
// every workload the operator deploys: the RuntimeDefault seccomp
// profile. Container-level hardening is applied per component where the
// image is known to run without privileges — see
// RestrictedSecurityContext.
func PodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// RestrictedSecurityContext is the container-level restricted baseline
// for images known to run without privileges. The uid must be the
// numeric upstream uid of the image's USER so runAsNonRoot is
// verifiable even when the image declares a non-numeric user name (e.g.
// kube-state-metrics' "nobody"). Do not apply to images whose startup
// requirements are unverified — the e2e suite is the gate.
func RestrictedSecurityContext(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		RunAsUser:                new(uid),
		AllowPrivilegeEscalation: new(false),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
