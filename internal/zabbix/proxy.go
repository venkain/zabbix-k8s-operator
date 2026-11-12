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
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Proxy env names (official image interface, verified against the 7.0/8.0
// entrypoints): ZBX_PROXYMODE selects active mode (required for proxy
// groups) and ZBX_USE_NODE_NAME_AS_DB_NAME names the embedded SQLite
// database after the pod so per-ordinal members do not share a database
// file. The proxy name itself comes from ZBX_HOSTNAME (downward API,
// see the container env comment).
const (
	envProxyMode  = "ZBX_PROXYMODE"
	envNodeNameDB = "ZBX_USE_NODE_NAME_AS_DB_NAME"

	portNameProxyTrapper = "zabbix-trapper"
	proxyContainerName   = "zabbix-proxy"
)

// DesiredProxyReplicas returns the configured group member count (1 by
// default). A single member already provides load balancing (host
// assignment); more members provide HA within the group.
func DesiredProxyReplicas(pg *zabbixv1alpha1.ZabbixProxyGroup) int32 {
	if pg.Spec.Replicas != nil {
		return *pg.Spec.Replicas
	}
	return 1
}

// ProxyRecordName returns the Zabbix proxy name of member <ordinal>: it
// equals the pod name (the identity the daemon resolves through
// HostnameItem=system.hostname and the name the server-side proxy record
// must carry for the member to join the group).
func ProxyRecordName(instance string, ordinal int32) string {
	return ProxyName(instance) + "-" + strconv.Itoa(int(ordinal))
}

// ProxyRecordAddress returns the DNS name registered as the member's
// local_address — the address active agents use to reach that proxy.
// The per-ordinal headless-Service DNS name is stable across pod restarts
// (validated requirement: local addresses of group members must survive
// rescheduling; pod IPs would orphan agent routing on every restart).
func ProxyRecordAddress(instance string, ordinal int32, namespace string) string {
	return fmt.Sprintf("%s.%s.%s.svc", ProxyRecordName(instance, ordinal), ProxyName(instance), namespace)
}

// ProxyStatefulSet builds the proxy-group member StatefulSet. A
// StatefulSet (not a Deployment) because membership identity is per
// ordinal: the pod name is the Zabbix proxy name (delivered as
// ZBX_HOSTNAME through the downward API) and must stay stable across
// restarts so it keeps matching the proxy record the operator created
// server-side, and the per-ordinal headless DNS keeps the registered
// local_address valid.
func ProxyStatefulSet(pg *zabbixv1alpha1.ZabbixProxyGroup, replicas int32, z *zabbixv1alpha1.Zabbix) *appsv1.StatefulSet {
	podLabels := commonLabels(pg.Name, ComponentProxy)

	proxyContainer := corev1.Container{
		Name:            proxyContainerName,
		Image:           ImageFor("proxy-sqlite3", pg.Spec.Version, pg.Spec.Image),
		ImagePullPolicy: pullPolicy(pg.Spec.Image),
		Env: []corev1.EnvVar{
			// Active mode: only operating_mode=0 proxies can be members
			// of a proxy group (official proxy object requirements).
			{Name: "ZBX_PROXYMODE", Value: "0"},
			// Explicit server wiring: Kubernetes injects Service-derived
			// env variables into every pod; owned values make these
			// authoritative (same reasoning as the agent sidecar env).
			// The active-node Service is addressed so trapper traffic
			// reaches the HA leader after a failover.
			{Name: envServerHost, Value: fmt.Sprintf("%s.%s.svc", ActiveServerService(z).Name, z.Namespace)},
			{Name: envServerPort, Value: fmt.Sprintf("%d", ServerPort)},
			// Per-pod identity: the pod name is the Zabbix proxy name —
			// <instance>-proxy-<n> — delivered through the downward API
			// instead of HostnameItem=system.hostname: the 8.0 image
			// entrypoint writes an explicitly empty Hostname alongside
			// the item, which the daemon rejects ("invalid Hostname
			// configuration parameter '': name is empty"; live-caught).
			// StatefulSet pod names are stable across restarts, so the
			// identity keeps matching the proxy record the operator
			// created server-side.
			{Name: "ZBX_HOSTNAME", ValueFrom: fieldRef("metadata.name")},
			// Separate embedded SQLite database per member.
			{Name: envNodeNameDB, Value: "true"},
			// ClickHouse telemetry provider wiring (APM output must land
			// in the same backend the server reads from; Zabbix 8.0+,
			// no-op otherwise). See telemetry.go for the contract.
		},
		Ports: []corev1.ContainerPort{
			{Name: portNameProxyTrapper, ContainerPort: ProxyPort},
		},
		ReadinessProbe: proxyReadyProbe(),
		LivenessProbe:  proxyLivenessProbe(),
		Resources:      pg.Spec.Resources,
	}
	// Telemetry wiring is appended after the literal so the base env
	// stays readable; both helpers are no-ops when disabled or on
	// pre-8.0 versions.
	proxyContainer.Env = append(proxyContainer.Env, ProxyTelemetryEnvs(pg, z)...)
	proxyContainer.Env = append(proxyContainer.Env, ProxyAPMEnvs(pg)...)
	if apm := APMListenPortIfEnabled(pg); apm > 0 {
		proxyContainer.Ports = append(proxyContainer.Ports,
			corev1.ContainerPort{Name: otlpPortName, ContainerPort: apm})
	}
	proxyContainer.VolumeMounts = append(TelemetryVolumeMounts(z), APMVolumeMounts(pg)...)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyName(pg.Name),
			Namespace: pg.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: ProxyName(pg.Name),
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(pg.Name, ComponentProxy)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{proxyContainer},
					Volumes:    append(TelemetrySecretVolumes(z), APMSecretVolumes(pg)...),
				},
			},
		},
	}
}

// ProxyHeadlessService is the governing Service of the member StatefulSet.
// headless — not a routed frontend: its only job is per-ordinal pod DNS
// (the member local_addresses) plus the identity the StatefulSet requires.
func ProxyHeadlessService(pg *zabbixv1alpha1.ZabbixProxyGroup) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyName(pg.Name),
			Namespace: pg.Namespace,
			Labels:    commonLabels(pg.Name, ComponentProxy),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  selectorLabels(pg.Name, ComponentProxy),
			Ports: []corev1.ServicePort{{
				Name:       portNameProxyTrapper,
				Port:       ProxyPort,
				TargetPort: intstr.FromInt32(ProxyPort),
			}},
		},
	}
}

// proxyReadyProbe reports readiness once the proxy daemon runs. pgrep is
// used instead of a TCP probe: a departing/failed member must go unready
// when its process is gone, and the trapper port only accepts connections
// of a fully-initialised daemon anyway.
func proxyReadyProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{probePgrep, "zabbix_proxy"},
			},
		},
		// The entrypoint prepares the schema of the embedded SQLite
		// database before the daemon starts.
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

// proxyLivenessProbe restarts the container after the daemon has been
// gone for several minutes.
func proxyLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{probePgrep, "zabbix_proxy"},
			},
		},
		InitialDelaySeconds: 120,
		PeriodSeconds:       30,
		FailureThreshold:    5,
	}
}

// DesiredProxyGroupSettings renders the API object payload values the
// DesiredProxyGroupSettings returns the group settings the operator
// maintains for the group (failover_delay/min_online with
// server-side defaults substituted for empty spec fields).
func DesiredProxyGroupSettings(pg *zabbixv1alpha1.ZabbixProxyGroup) (failoverDelay, minOnline string) {
	failoverDelay = "1m"
	if pg.Spec.FailoverDelay != "" {
		failoverDelay = pg.Spec.FailoverDelay
	}
	minOnline = "1"
	if pg.Spec.MinOnline > 0 {
		minOnline = fmt.Sprintf("%d", pg.Spec.MinOnline)
	}
	return failoverDelay, minOnline
}

// FailoverDelay returns the group's effective failover window as a
// duration (an unparsable spec value falls back to the server default,
// the CRD schema already constrains the format).
func FailoverDelay(pg *zabbixv1alpha1.ZabbixProxyGroup) time.Duration {
	spec := "1m"
	if pg.Spec.FailoverDelay != "" {
		spec = pg.Spec.FailoverDelay
	}
	delay, err := time.ParseDuration(spec)
	if err != nil {
		return time.Minute
	}
	return delay
}

// membersOnlineGrace tolerates clock skews between the operator (which
// compares timestamps against its own now) and the server (which stamps
// lastaccess with its clock), without stretching the failover window in
// any meaningful way.
const membersOnlineGrace = 30 * time.Second

// MembersOnline reports how many of the OPERATOR-MANAGED group members
// the Zabbix server currently counts as online: their last contact
// (proxy.lastaccess, the official per-member availability datum; there
// is no dedicated online flag on proxy objects) lies within the group's
// failover window. A member that never contacted the server, and other
// instances' or user-created proxies, are never counted.
func MembersOnline(members []ProxyMember, instance string, failoverDelay time.Duration, now time.Time) int32 {
	var online int32
	for _, m := range members {
		if !IsManagedProxyRecord(instance, m.Name) {
			continue
		}
		if age := now.Unix() - m.LastContact; m.LastContact > 0 &&
			time.Duration(age)*time.Second <= failoverDelay+membersOnlineGrace {
			online++
		}
	}
	return online
}
