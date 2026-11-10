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
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// revisionHashLabel is the label the StatefulSet controller stamps on
// pods with the template revision they were created from.
const revisionHashLabel = "controller-revision-hash"

// podIsReady reports whether the pod's Ready condition is True.
func podIsReady(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// RecycleStalePods un-wedges a StatefulSet rollout whose members failed
// to replace: a member pod that is not Ready yet still carries an older
// controller-revision-hash (a crash loop that predates the current
// template) can starve its own replacement — the rollout cannot make it
// Ready, and the pod counts against the update budget until something
// removes it. Deleting such a pod lets the StatefulSet controller
// recreate it from the current template; a pod on the current revision
// (simply unhealthy) and a Ready pod mid-rollout are left to the
// StatefulSet controller. Stale pods that are already terminating are
// skipped.
func RecycleStalePods(ctx context.Context, c client.Client, sts *appsv1.StatefulSet) error {
	// Without rollout state there is no authoritative current template
	// to compare pods against; recycling would be guessing.
	target := sts.Status.UpdateRevision
	if target == "" {
		target = sts.Status.CurrentRevision
	}
	if target == "" {
		return nil
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(sts.Namespace), client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err != nil {
		return err
	}

	// The label carries the FULL revision name ("<sts-name>-<hash>"),
	// exactly like sts.Status.UpdateRevision/CurrentRevision — compare
	// without extraction.
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() || podIsReady(pod) {
			continue
		}
		revision, ok := pod.Labels[revisionHashLabel]
		if !ok || revision == target {
			continue
		}
		log.FromContext(ctx).Info("Recycled Pod", "pod", pod.Name, "revision", revision, "current", target, "statefulset", sts.Name)
		if err := c.Delete(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}
