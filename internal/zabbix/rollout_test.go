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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// rolloutZabbix builds the spec under test for the named instance.
func rolloutZabbix(name string) *zabbixv1alpha1.Zabbix {
	return &zabbixv1alpha1.Zabbix{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: zabbixv1alpha1.ZabbixSpec{
			Version: "8.0",
			Server:  zabbixv1alpha1.ServerSpec{},
		},
	}
}

// liveSTS builds a live server StatefulSet in the requested shape.
func liveSTS(z *zabbixv1alpha1.Zabbix, replicas int32, haIdentity bool) *appsv1.StatefulSet {
	return serverStatefulSet(z, testDB(), replicas, haIdentity)
}

func replicationOK(t *testing.T, sts *appsv1.StatefulSet, wantReplicas int32, wantHA bool) {
	t.Helper()
	if n := *sts.Spec.Replicas; n != wantReplicas {
		t.Errorf("replicas = %d, want %d", n, wantReplicas)
	}
	if got := StatefulSetHasHAIdentity(sts); got != wantHA {
		t.Errorf("HA identity = %v, want %v", got, wantHA)
	}
	// The standalone template must stamp the active role; HA templates
	// never stamp (per-pod maintenance owns the label).
	stamped := sts.Spec.Template.Labels[HARoleLabel] == HARoleActive
	if stamped == wantHA {
		t.Errorf("role stamp = %v (HA identity = %v); stamp and HA identity must be exclusive", stamped, wantHA)
	}
}

func TestServerWorkloadForRolloutPhases(t *testing.T) {
	containerEnv := func(sts *appsv1.StatefulSet) []corev1.EnvVar {
		for i := range sts.Spec.Template.Spec.Containers {
			if sts.Spec.Template.Spec.Containers[i].Name == serverContainerName {
				return sts.Spec.Template.Spec.Containers[i].Env
			}
		}
		return nil
	}

	t.Run("fresh install desired 2 goes straight to HA spec", func(t *testing.T) {
		z := rolloutZabbix("fresh")
		z.Spec.Server.Replicas = ptr.To[int32](2)

		sts, phase := ServerWorkloadForRollout(z, testDB(), nil, nil)
		if phase != RolloutSteady {
			t.Errorf("phase = %q, want Steady", phase)
		}
		replicationOK(t, sts, 2, true)
	})

	t.Run("fresh install desired 1 stays standalone", func(t *testing.T) {
		z := rolloutZabbix("fresh1")

		sts, phase := ServerWorkloadForRollout(z, testDB(), nil, nil)
		if phase != RolloutSteady {
			t.Errorf("phase = %q, want Steady", phase)
		}
		replicationOK(t, sts, 1, false)
	})

	t.Run("standalone live scaled to 2 caps at 1 with HA identity", func(t *testing.T) {
		z := rolloutZabbix("z")
		z.Spec.Server.Replicas = ptr.To[int32](2)
		live := liveSTS(z, 1, false)

		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nil)
		if phase != RolloutTemplateFirst {
			t.Errorf("phase = %q, want TemplateFirst", phase)
		}
		replicationOK(t, sts, 1, true)

		// The HA identity env must actually appear on the template
		// (this is what pod-0 rolls onto).
		if !hasEnvNamed(containerEnv(sts), envAutoHANodeName) {
			t.Errorf("template lacks %s", envAutoHANodeName)
		}
	})

	t.Run("standalone registry entry active blocks straight-to-HA", func(t *testing.T) {
		// No live workload (migration residue: standalone entry still
		// active after an unclean kill). Applying the full HA spec
		// here would crash-loop pod-0 with the validated transition
		// error.
		z := rolloutZabbix("z")
		z.Spec.Server.Replicas = ptr.To[int32](2)

		sts, phase := ServerWorkloadForRollout(z, testDB(), nil, []HANode{
			{Name: "", Status: HANodeStateActive},
		})
		if phase != RolloutTemplateFirst {
			t.Errorf("phase = %q, want TemplateFirst", phase)
		}
		replicationOK(t, sts, 1, true)
	})

	t.Run("HA live with named active scales to spec", func(t *testing.T) {
		z := rolloutZabbix("z")
		z.Spec.Server.Replicas = ptr.To[int32](2)
		live := liveSTS(z, 1, true)

		nodes := []HANode{{Name: ServerName(z.Name) + "-0", Status: HANodeStateActive}}
		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nodes)
		if phase != RolloutSteady {
			t.Errorf("phase = %q, want Steady", phase)
		}
		replicationOK(t, sts, 2, true)
	})

	t.Run("HA live with unknown registry holds at live replicas", func(t *testing.T) {
		z := rolloutZabbix("z")
		z.Spec.Server.Replicas = ptr.To[int32](3)
		live := liveSTS(z, 2, true)

		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nil)
		if phase != RolloutHoldHA {
			t.Errorf("phase = %q, want HoldHA", phase)
		}
		if *sts.Spec.Replicas != 2 {
			t.Errorf("held replicas = %d, want min(desired=3, live=2)=2", *sts.Spec.Replicas)
		}
		if !StatefulSetHasHAIdentity(sts) {
			t.Error("hold must keep the HA identity")
		}
	})

	t.Run("HA live with empty registry holds at one", func(t *testing.T) {
		z := rolloutZabbix("z")
		z.Spec.Server.Replicas = ptr.To[int32](2)
		live := liveSTS(z, 2, true)

		sts, phase := ServerWorkloadForRollout(z, testDB(), live, []HANode{})
		if phase != RolloutHoldHA {
			t.Errorf("phase = %q, want HoldHA", phase)
		}
		if *sts.Spec.Replicas != 1 {
			t.Errorf("replicas = %d, want 1 (no named active node: never scale out)", *sts.Spec.Replicas)
		}
	})

	t.Run("scale down to 1 with ordinal 0 active flips to standalone", func(t *testing.T) {
		z := rolloutZabbix("z")
		live := liveSTS(z, 1, true)

		nodes := []HANode{{Name: ServerName(z.Name) + "-0", Status: HANodeStateActive}}
		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nodes)
		if phase != RolloutFlipStandalone {
			t.Errorf("phase = %q, want StandaloneFlip", phase)
		}
		replicationOK(t, sts, 1, false)
	})

	t.Run("scale down to 1 with another node active keeps HA template", func(t *testing.T) {
		z := rolloutZabbix("z")
		live := liveSTS(z, 2, true)

		nodes := []HANode{
			{Name: ServerName(z.Name) + "-0", Status: HANodeStateStandby},
			{Name: ServerName(z.Name) + "-1", Status: HANodeStateActive},
		}
		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nodes)
		if phase != RolloutScaleDownHA {
			t.Errorf("phase = %q, want ScaleDownHA", phase)
		}
		// Cap at one: the StatefulSet stops the highest ordinal; its
		// clean termination hands the active role to pod-0.
		replicationOK(t, sts, 1, true)
	})

	t.Run("scale down to 1 without registry info keeps HA template", func(t *testing.T) {
		z := rolloutZabbix("z")
		live := liveSTS(z, 2, true)

		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nil)
		if phase != RolloutScaleDownHA {
			t.Errorf("phase = %q, want ScaleDownHA", phase)
		}
		replicationOK(t, sts, 1, true)
	})

	t.Run("scale down to 1 during failover keeps HA template", func(t *testing.T) {
		// No active node anywhere (electing a new active): the flip
		// waits — a template swap now would make the outage worse by
		// restarting the only surviving server.
		z := rolloutZabbix("z")
		live := liveSTS(z, 2, true)

		nodes := []HANode{
			{Name: ServerName(z.Name) + "-0", Status: HANodeStateUnavailable},
		}
		sts, phase := ServerWorkloadForRollout(z, testDB(), live, nodes)
		if phase != RolloutScaleDownHA {
			t.Errorf("phase = %q, want ScaleDownHA", phase)
		}
		replicationOK(t, sts, 1, true)
	})
}

func hasEnvNamed(envs []corev1.EnvVar, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestStatefulSetHasHAIdentity(t *testing.T) {
	z := rolloutZabbix("z")

	if StatefulSetHasHAIdentity(nil) {
		t.Error("nil workload must be standalone")
	}
	if StatefulSetHasHAIdentity(liveSTS(z, 1, false)) {
		t.Error("standalone template reported HA identity")
	}
	if !StatefulSetHasHAIdentity(liveSTS(z, 2, true)) {
		t.Error("HA template not recognized")
	}
}
