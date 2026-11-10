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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	recycleNamespace = "default"
	recycleStsName   = "recycle-test-proxy"
)

// recycleRevision names the rollout target revision the way the
// StatefulSet controller does: "<sts-name>-<hash>" (dashes included in
// the name itself).

func recycleTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding appsv1 to scheme: %v", err)
	}
	return scheme
}

// stsRevision builds the revision name in the StatefulSet form:
// "<sts-name>-<hash>", used both by sts.Status.UpdateRevision and by the
// pods' controller-revision-hash label.
func stsRevision(hash string) string { return recycleStsName + "-" + hash }

// recycleSTS is a member StatefulSet mid-rollout: the desired template
// is revision "rev4", an earlier generation produced "rev3". The
// revision names carry the StS-name-prefixed form the StatefulSet
// controller uses for UpdateRevision and the pod label alike.
func recycleSTS(targetRevision string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: recycleStsName, Namespace: recycleNamespace},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "recycle"}},
		},
	}
	if targetRevision != "" {
		sts.Status.UpdateRevision = targetRevision
	}
	return sts
}

func recyclePod(name, hash string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: recycleNamespace,
			Labels:    map[string]string{"app": "recycle"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionUnknown,
			}},
		},
	}
	if hash != "" {
		pod.Labels[revisionHashLabel] = hash
	}
	if ready {
		pod.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	return pod
}

func TestRecycleStalePods(t *testing.T) {
	sts := recycleSTS(stsRevision("rev4"))

	staleNotReady := recyclePod("recycle-test-proxy-0", stsRevision("rev3"), false)
	currentNotReady := recyclePod("recycle-test-proxy-1", stsRevision("rev4"), false)
	staleReady := recyclePod("recycle-test-proxy-2", stsRevision("rev3"), true)
	unlabeled := recyclePod("recycle-test-proxy-3", "", false)

	c := fake.NewClientBuilder().
		WithScheme(recycleTestScheme(t)).
		WithObjects(sts, staleNotReady, currentNotReady, staleReady, unlabeled).
		Build()

	if err := RecycleStalePods(context.Background(), c, sts); err != nil {
		t.Fatalf("RecycleStalePods: %v", err)
	}

	// Only the stale, not-Ready pod is removed: the crash loop predating
	// the current template was starving its own replacement. Everything
	// else stays with the StatefulSet controller.
	for _, pod := range []*corev1.Pod{currentNotReady, staleReady, unlabeled} {
		if err := c.Get(context.Background(), clientKeyFrom(pod), pod); err != nil {
			t.Errorf("stale-and-Ready pod %q was deleted: %v", pod.Name, err)
		}
	}
	if err := c.Get(context.Background(), clientKeyFrom(staleNotReady), staleNotReady); !apierrors.IsNotFound(err) {
		t.Errorf("stale not-Ready pod deletion: got %v, want NotFound", err)
	}
}

func TestRecycleStalePodsSkipsTerminating(t *testing.T) {
	sts := recycleSTS(stsRevision("rev4"))
	terminating := recyclePod("recycle-test-proxy-0", stsRevision("rev3"), false)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	// A terminating object carries at least one finalizer (the fake
	// client refuses a deletionTimestamp without one).
	terminating.Finalizers = []string{"example.com/terminating"}

	c := fake.NewClientBuilder().
		WithScheme(recycleTestScheme(t)).
		WithObjects(sts, terminating).
		Build()

	if err := RecycleStalePods(context.Background(), c, sts); err != nil {
		t.Fatalf("RecycleStalePods: %v", err)
	}
	if err := c.Get(context.Background(), clientKeyFrom(terminating), terminating); err != nil {
		t.Errorf("terminating pod was deleted again: %v", err)
	}
}

// TestRecycleStalePodsNoRolloutState verifies the no-guessing guard:
// without reported rollout state the helper must not touch anything.
func TestRecycleStalePodsNoRolloutState(t *testing.T) {
	sts := recycleSTS("")
	stale := recyclePod("recycle-test-proxy-0", stsRevision("rev3"), false)

	c := fake.NewClientBuilder().
		WithScheme(recycleTestScheme(t)).
		WithObjects(sts, stale).
		Build()

	if err := RecycleStalePods(context.Background(), c, sts); err != nil {
		t.Fatalf("RecycleStalePods: %v", err)
	}
	if err := c.Get(context.Background(), clientKeyFrom(stale), stale); err != nil {
		t.Errorf("pod deleted with no rollout state reported: %v", err)
	}
}

func clientKeyFrom(pod *corev1.Pod) client.ObjectKey {
	return client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}
}
