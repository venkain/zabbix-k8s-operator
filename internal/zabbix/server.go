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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Server deployment env names (official image interface, verified against
// the 8.0 image entrypoint): ZBX_AUTOHANODENAME/ZBX_AUTONODEADDRESS derive
// per-pod HA identity from the pod hostname; ZBX_NODEADDRESSPORT sets the
// port registered alongside the node address.
const (
	envAutoHANodeName  = "ZBX_AUTOHANODENAME"
	envAutoNodeAddress = "ZBX_AUTONODEADDRESS"
	envNodeAddressPort = "ZBX_NODEADDRESSPORT"
	// Server wiring of the trapper address; the proxy group members set
	// the same pair (see proxy.go).
	envServerHost       = "ZBX_SERVER_HOST"
	envServerPort       = "ZBX_SERVER_PORT"
	portNameTrapper     = "zabbix-trapper"
	serverContainerName = "zabbix-server"
	agentContainerName  = "agent2"
	// probePgrep is the process-liveness command of the server and proxy
	// daemons (exec pgrep <daemon>): TCP probes would kill standby pods,
	// which accept no connections (see the HA probe lesson).
	probePgrep = "pgrep"
)

// ServerStatefulSet builds the server StatefulSet for the configured
// spec (steady state). The rollout-phase variant ServerWorkloadForRollout
// shapes the same statefulset during registry transitions.
func ServerStatefulSet(z *zabbixv1alpha1.Zabbix, db *DatabaseEnv) *appsv1.StatefulSet {
	return serverStatefulSet(z, db, ServerReplicas(z), ServerReplicas(z) > 1)
}

// serverStatefulSet builds the server StatefulSet with explicit HA
// identity and replica control. StatefulSet (not Deployment) because HA
// node identity must be STABLE: with ZBX_AUTOHANODENAME=hostname the pod
// name is the HA node name, and a restarted Deployment pod would get a
// new random name — registering a new ha_node registry entry while the
// old one lingers as unavailable forever. StatefulSet ordinals keep the
// same name across restarts, so the registry entry is reused instead of
// leaked. Note that HA identity and replica count are decoupled: HA-mode
// templates can run with a single replica (scale down before the
// standalone flip, ServerRolloutPhase), and standalone templates must
// never carry identity env.
//
// haIdentity marks the pod template as a HA participant; when false the
// single pod is standalone and the pod template stamps the HA-role label
// as active (no API access needed for the -active Service). HA templates
// never stamp the label: the per-pod operator maintenance derives it
// from the official hanode.get registry.
func serverStatefulSet(z *zabbixv1alpha1.Zabbix, db *DatabaseEnv, replicas int32, haIdentity bool) *appsv1.StatefulSet {
	spec := &z.Spec.Server

	podLabels := commonLabels(z.Name, ComponentServer)
	if !haIdentity {
		podLabels[HARoleLabel] = HARoleActive
	}
	env := append(db.ServerEnvVars(), webServiceEnv(z)...)
	if haIdentity {
		env = append(env, serverHAEnv()...)
	}
	// ClickHouse history/telemetry wiring (Zabbix 8.0+; no-op when unset
	// or on older versions). See telemetry.go for the entrypoint contract.
	env = append(env, ServerTelemetryEnvs(z)...)

	serverContainer := corev1.Container{
		Name:            serverContainerName,
		Image:           ImageFor("server-pgsql", z.Spec.Version, spec.Image),
		ImagePullPolicy: pullPolicy(spec.Image),
		Env:             env,
		Ports: []corev1.ContainerPort{
			{Name: portNameTrapper, ContainerPort: ServerPort},
		},
		ReadinessProbe: serverReadyProbe(),
		LivenessProbe:  serverLivenessProbe(),
		Resources:      spec.Resources,
		VolumeMounts:   TelemetryVolumeMounts(z),
		// The server entrypoint prepares the database schema and node
		// identity on startup; give it room to finish cleanly.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	containers := []corev1.Container{serverContainer}
	if agentEnabled(spec.Agent) {
		containers = append(containers, agentSidecar(z, spec.Agent))
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			// The governing service does not need per-pod DNS (traffic
			// reaches the server through the -active Service), but the
			// StatefulSet requires a service name.
			ServiceName: ServerName(z.Name),
			// OrderedReady (default): predecessors are Running and Ready
			// before the next ordinal starts. This is the standalone→HA
			// sequencing guarantee — the first pod re-registers the
			// registry entry (the clean stop of the replaced pod marks
			// the standalone entry inactive), and only then the next
			// ordinal joins as standby.
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentServer)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: containers,
					Volumes:    TelemetrySecretVolumes(z),
				},
			},
		},
	}
}

// ServerService builds the Service fronting all server pods (used by
// proxies and agents that speak to any node... in practice trapper
// traffic goes to the active node; this Service selects every pod).
func ServerService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentServer),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(z.Name, ComponentServer),
			Ports: []corev1.ServicePort{{
				Name:       portNameTrapper,
				Port:       ServerPort,
				TargetPort: intstr.FromInt32(ServerPort),
			}},
		},
	}
}

// ActiveServerService builds the Service that routes ONLY to the active
// HA node (via the operator-maintained HARoleLabel). When the server runs
// standalone (replicas=1) this equals the plain server Service behavior.
func ActiveServerService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	labels := commonLabels(z.Name, ComponentServer)
	labels[HARoleLabel] = HARoleActive

	sel := selectorLabels(z.Name, ComponentServer)
	sel[HARoleLabel] = HARoleActive

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(z.Name) + "-active",
			Namespace: z.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: sel,
			Ports: []corev1.ServicePort{{
				Name:       portNameTrapper,
				Port:       ServerPort,
				TargetPort: intstr.FromInt32(ServerPort),
			}},
		},
	}
}

// ServerEnvVarsForFrontend returns the env the frontend needs to reach the
// server: the host is the ACTIVE server service when HA is enabled, so the
// frontend always talks to whichever node is active.
func ServerEnvVarsForFrontend(z *zabbixv1alpha1.Zabbix) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: envServerHost, Value: ActiveServerService(z).Name},
		{Name: envServerPort, Value: fmt.Sprintf("%d", ServerPort)},
		{Name: "ZBX_SERVER_NAME", Value: z.Name},
	}
}

// webServiceEnv wires the server to the web service component. The
// packaged conf fragment zabbix_server_reports.conf performs
// ${ZBX_WEBSERVICEURL}/${ZBX_STARTREPORTWRITERS} substitution (official
// 8.0 config-env mechanism), so plain env is sufficient — no config
// mounts. Returns nil when no web service is deployed.
func webServiceEnv(z *zabbixv1alpha1.Zabbix) []corev1.EnvVar {
	if z.Spec.WebService == nil {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "ZBX_STARTREPORTWRITERS", Value: "1"},
		{Name: "ZBX_WEBSERVICEURL", Value: fmt.Sprintf("http://%s:%d/report", WebServiceService(z).Name, WebServicePort)},
	}
}

// serverHAEnv returns the env marking the pod as a HA participant
// (official entrypoint: ZBX_AUTOHANODENAME=hostname makes each pod
// register under its own hostname; ZBX_AUTONODEADDRESS=hostname
// advertises the hostname and ZBX_NODEADDRESSPORT the port as the node
// address). Unconditional: the caller decides whether the template is a
// HA participant at all (standalone templates must never carry it — the
// server refuses to change mode to HA while a standalone node is
// active).
func serverHAEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: envAutoHANodeName, Value: "hostname"},
		{Name: envAutoNodeAddress, Value: "hostname"},
		{Name: envNodeAddressPort, Value: fmt.Sprintf("%d", ServerPort)},
	}
}

func agentEnabled(spec *zabbixv1alpha1.AgentSidecarSpec) bool {
	return spec == nil || spec.Enabled == nil || *spec.Enabled
}

func agentSidecar(z *zabbixv1alpha1.Zabbix, spec *zabbixv1alpha1.AgentSidecarSpec) corev1.Container {
	return corev1.Container{
		Name:            agentContainerName,
		Image:           ImageFor("agent2", z.Spec.Version, agentImage(spec)),
		ImagePullPolicy: pullPolicy(agentImage(spec)),
		Env: []corev1.EnvVar{
			// Passive sidecar: only answers within the pod; named after
			// the pod so the server self-monitors each replica. POD_NAME
			// must precede ZBX_HOSTNAME for $(VAR) expansion to resolve.
			{Name: "POD_NAME", ValueFrom: fieldRef("metadata.name")},
			{Name: "ZBX_HOSTNAME", Value: "$(POD_NAME)"},
			// Explicit overrides: Kubernetes injects Service environment
			// variables into every pod — a Service named <instance>-server
			// injects ZBX_SERVER_PORT=<tcp://ip:port> namespace-wide, which
			// corrupts the agent's ServerActive composition. Defining the
			// variables here makes the container values authoritative.
			{Name: envServerHost, Value: "127.0.0.1"},
			{Name: envServerPort, Value: fmt.Sprintf("%d", ServerPort)},
			{Name: "ZBX_SERVER_NAME", Value: z.Name},
		},
	}
}

func agentImage(spec *zabbixv1alpha1.AgentSidecarSpec) *zabbixv1alpha1.ImageSpec {
	if spec == nil {
		return nil
	}
	return spec.Image
}

func pullPolicy(spec *zabbixv1alpha1.ImageSpec) corev1.PullPolicy {
	if spec != nil && spec.PullPolicy != "" {
		return spec.PullPolicy
	}
	return corev1.PullIfNotPresent
}

// tcpProbe probes a TCP port (shared by the frontend and web service
// deployments).
func tcpProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

// serverReadyProbe reports pod readiness as HA cluster membership: the
// zabbix_server process runs. A TCP probe against the trapper port
// cannot be used — only the active HA node accepts trapper connections
// (validated in the lab: a standby server closes port 10051 and the
// control socket refuses runtime commands, so a TCP probe liveness
// killed every standby pod about a minute after it entered standby).
// The active/standby ROLE is not the pod's concern: routing is driven
// by the operator-maintained HA-role label on the -active Service.
func serverReadyProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{probePgrep, "zabbix_server"},
			},
		},
		// The entrypoint prepares the database schema before it starts
		// the server process.
		InitialDelaySeconds: 15,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

// serverLivenessProbe restarts the container after the zabbix_server
// process has been gone for several minutes — long enough not to
// interfere with a schema-level upgrade or re-registration, short
// enough to recover from a hung process without operator intervention.
func serverLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{probePgrep, "zabbix_server"},
			},
		},
		InitialDelaySeconds: 120,
		PeriodSeconds:       30,
		FailureThreshold:    5,
	}
}

func fieldRef(field string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: field},
	}
}
