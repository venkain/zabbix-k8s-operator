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
	envAutoHANodeName   = "ZBX_AUTOHANODENAME"
	envAutoNodeAddress  = "ZBX_AUTONODEADDRESS"
	envNodeAddressPort  = "ZBX_NODEADDRESSPORT"
	portNameTrapper     = "zabbix-trapper"
	serverContainerName = "zabbix-server"
	agentContainerName  = "agent2"
)

// ServerDeployment builds the server Deployment. replicas > 1 enables
// native HA: each pod joins the HA cluster with its own hostname as node
// name, and all replicas share the selected database.
func ServerDeployment(z *zabbixv1alpha1.Zabbix, db *DatabaseEnv) *appsv1.Deployment {
	spec := &z.Spec.Server

	replicas := int32(1)
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	podLabels := commonLabels(z.Name, ComponentServer)
	if replicas == 1 {
		// Standalone mode: the single pod is the active node by
		// definition. Stamping the HA-role label keeps the active-node
		// Service functional before per-pod HA maintenance exists
		// (replicas > 1 requires the HA reconcile slice, which maintains
		// the label per pod from the Zabbix hanode state).
		podLabels[HARoleLabel] = HARoleActive
	}
	env := append(db.ServerEnvVars(), serverHAEnv(replicas)...)
	env = append(env, webServiceEnv(z)...)

	serverContainer := corev1.Container{
		Name:            serverContainerName,
		Image:           ImageFor("server-pgsql", z.Spec.Version, spec.Image),
		ImagePullPolicy: pullPolicy(spec.Image),
		Env:             env,
		Ports: []corev1.ContainerPort{
			{Name: portNameTrapper, ContainerPort: ServerPort},
		},
		ReadinessProbe: tcpProbe(ServerPort),
		LivenessProbe:  tcpProbe(ServerPort),
		Resources:      spec.Resources,
		// The server entrypoint prepares the database schema and node
		// identity on startup; give it room to finish cleanly.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	containers := []corev1.Container{serverContainer}
	if agentEnabled(spec.Agent) {
		containers = append(containers, agentSidecar(z, spec.Agent))
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.DeploymentSpec{
			// Recreate: HA node identity is per-pod; rolling replacement
			// would briefly double-declare node names.
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentServer)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: containers,
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
		{Name: "ZBX_SERVER_HOST", Value: ActiveServerService(z).Name},
		{Name: "ZBX_SERVER_PORT", Value: fmt.Sprintf("%d", ServerPort)},
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

// serverHAEnv returns the HA identity env when replicas > 1 (official
// entrypoint: ZBX_AUTOHANODENAME=hostname makes each pod register under
// its own hostname; ZBX_AUTONODEADDRESS=hostname advertises the hostname
// and ZBX_NODEADDRESSPORT the port as the node address).
func serverHAEnv(replicas int32) []corev1.EnvVar {
	if replicas <= 1 {
		return nil
	}
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
			{Name: "ZBX_SERVER_HOST", Value: "127.0.0.1"},
			{Name: "ZBX_SERVER_PORT", Value: fmt.Sprintf("%d", ServerPort)},
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

func tcpProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
	}
}

func fieldRef(field string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: field},
	}
}
