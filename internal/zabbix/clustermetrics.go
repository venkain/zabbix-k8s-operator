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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Cluster-metrics ingestion: the objects that feed Kubernetes cluster
// metrics into the instance's Zabbix server through the official
// "Kubernetes Cluster by HTTP" template. Wire constraints from the
// lab verification (docs/requirements.md, OQ5):
//   - the template discovers kube-state-metrics by regex
//     (.+)kube-state-metrics$ against Endpoints names — the Service
//     name must END with "kube-state-metrics";
//   - the discovered Endpoints port must be named "http";
//   - the template scrapes THROUGH the Kubernetes API proxy, so the
//     monitor identity needs services/proxy and nodes/proxy besides
//     read access.

const (
	// kubeStateMetricsImageRepo is the default kube-state-metrics image.
	kubeStateMetricsImageRepo = "registry.k8s.io/kube-state-metrics/kube-state-metrics"
	// kubeStateMetricsImageTag is the lab-verified default version.
	kubeStateMetricsImageTag = "v2.9.2"

	// KubeStateMetricsPort is kube-state-metrics' listening port.
	KubeStateMetricsPort = int32(8080)

	// serviceAccountTokenSecretType is the legacy service-account token
	// Secret type: created empty with a service-account.name annotation,
	// the API server's token controller fills it with a long-lived
	// token the Zabbix server uses as {$KUBE.API.TOKEN}.
	serviceAccountTokenSecretType = corev1.SecretTypeServiceAccountToken

	// serviceAccountNameAnnotation points a token Secret at its account.
	serviceAccountNameAnnotation = corev1.ServiceAccountNameKey
)

// Names of the derived cluster-metrics objects for a Zabbix resource
// named <name> in namespace <ns>. The kube-state-metrics Service name
// ends with "kube-state-metrics" so the template's discovery regex
// matches. The cluster-scoped RBAC names embed the namespace: two
// instances in different namespaces must not fight over one ClusterRole.
func KubeStateMetricsName(instance string) string { return instance + "-kube-state-metrics" }

// MonitorServiceAccountName returns the service account whose long-lived
// token becomes the Zabbix host's {$KUBE.API.TOKEN}.
func MonitorServiceAccountName(instance string) string { return instance + "-kube-monitor" }

// MonitorTokenSecretName returns the Secret holding the monitor token.
func MonitorTokenSecretName(instance string) string { return instance + "-kube-monitor-token" }

// ClusterMetricsRBACName returns the cluster-scoped ClusterRole /
// ClusterRoleBinding name for both cluster-metrics identities (the
// kube-state-metrics pod and the monitor account). The names embed the
// namespace: ClusterRoles are cluster-scoped, and two instances in
// different namespaces must not fight over one shared role.
func ClusterMetricsRBACName(z *zabbixv1alpha1.Zabbix) string {
	return fmt.Sprintf("%s-%s", z.Namespace, KubeStateMetricsName(z.Name))
}

// kubeStateMetricsImage resolves the KSM image from the optional
// override (repository/tag replace the default pair wholesale).
func kubeStateMetricsImage(spec *zabbixv1alpha1.KubeStateMetricsSpec) string {
	if spec == nil || spec.Image == nil {
		return kubeStateMetricsImageRepo + ":" + kubeStateMetricsImageTag
	}
	repo := kubeStateMetricsImageRepo
	tag := kubeStateMetricsImageTag
	if spec.Image.Repository != "" {
		repo = spec.Image.Repository
	}
	if spec.Image.Tag != "" {
		tag = spec.Image.Tag
	}
	return repo + ":" + tag
}

// KubeStateMetricsDeployment builds the kube-state-metrics Deployment.
// Single replica: the template scrapes one endpoint and two KSM
// replicas would serve independent counters.
func KubeStateMetricsDeployment(z *zabbixv1alpha1.Zabbix) *appsv1.Deployment {
	spec := z.Spec.ClusterMetrics.KubeStateMetrics
	var resources corev1.ResourceRequirements
	if spec != nil {
		resources = spec.Resources
	}
	podLabels := commonLabels(z.Name, ComponentClusterMetrics)
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeStateMetricsName(z.Name),
			Namespace: z.Namespace,
			Labels:    podLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(z.Name, ComponentClusterMetrics)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: KubeStateMetricsName(z.Name),
					Containers: []corev1.Container{{
						Name:            "kube-state-metrics",
						Image:           kubeStateMetricsImage(spec),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: portNameHTTP, ContainerPort: KubeStateMetricsPort},
						},
						ReadinessProbe: httpProbe("/healthz"),
						LivenessProbe:  httpProbe("/healthz"),
						Resources:      resources,
					}},
				},
			},
		},
	}
}

// KubeStateMetricsService builds the KSM Service. The NAME is the
// discovery contract (must end with "kube-state-metrics"); the port
// NAME is the other half of the contract ("http").
func KubeStateMetricsService(z *zabbixv1alpha1.Zabbix) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeStateMetricsName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClusterMetrics),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(z.Name, ComponentClusterMetrics),
			Ports: []corev1.ServicePort{{
				Name:       portNameHTTP,
				Port:       KubeStateMetricsPort,
				TargetPort: intstr.FromString(portNameHTTP),
			}},
		},
	}
}

// KubeStateMetricsServiceAccount builds the service account the
// kube-state-metrics pod runs under (it reads cluster state through the
// API). Separate from the monitor account: one identity scrapes, the
// other is only ever a token source for the Zabbix server.
func KubeStateMetricsServiceAccount(z *zabbixv1alpha1.Zabbix) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KubeStateMetricsName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClusterMetrics),
		},
	}
}

// MonitorServiceAccount builds the monitor service account. No pod runs
// under it — its only consumer is the Zabbix server, which authenticates
// to the Kubernetes API with the account's long-lived token.
func MonitorServiceAccount(z *zabbixv1alpha1.Zabbix) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MonitorServiceAccountName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClusterMetrics),
		},
		// Nothing mounts this account; keep the projected token out of
		// any pod template that might reference it by accident.
		AutomountServiceAccountToken: new(false),
	}
}

// MonitorTokenSecret builds the (initially empty) token Secret. The
// service-account token controller fills data["token"], data["ca.crt"]
// and data["namespace"] asynchronously; the controller reconciles until
// the token key appears. Server-side apply never claims the data field
// (the operator leaves it unset), so the filled token is not wiped.
func MonitorTokenSecret(z *zabbixv1alpha1.Zabbix) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MonitorTokenSecretName(z.Name),
			Namespace: z.Namespace,
			Labels:    commonLabels(z.Name, ComponentClusterMetrics),
			Annotations: map[string]string{
				serviceAccountNameAnnotation: MonitorServiceAccountName(z.Name),
			},
		},
		Type: serviceAccountTokenSecretType,
	}
}

// ClusterMetricsClusterRole builds the shared ClusterRole for both
// identities. The rule set is deliberately minimal: exactly what the
// kube-state-metrics endpoint and the "Kubernetes Cluster by HTTP"
// template read. Secrets are NOT granted (no template item needs
// kube_secret_* metrics, and read access to them must not be handed
// out for monitoring). Note the Kubernetes privilege-escalation
// protection: the operator's own role must hold a superset of these
// rules (see the rbac markers in the controller).
func ClusterMetricsClusterRole(z *zabbixv1alpha1.Zabbix) *rbacv1.ClusterRole {
	read := []string{"get", "list", "watch"}
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ClusterMetricsRBACName(z),
			Labels: commonLabels(z.Name, ComponentClusterMetrics),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{corev1.GroupName},
				Resources: []string{
					"endpoints", "namespaces", "nodes",
					"persistentvolumeclaims", "persistentvolumes", "pods",
					"services",
				},
				Verbs: read,
			},
			{
				// The template scrapes THROUGH the API proxy: KSM
				// endpoint discovery + scrape (services/proxy) and
				// kubelet resource metrics (nodes/proxy).
				APIGroups: []string{corev1.GroupName},
				Resources: []string{"services/proxy", "nodes/proxy"},
				Verbs:     []string{"create", "get"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"daemonsets", "deployments", "replicasets", "statefulsets"},
				Verbs:     read,
			},
			{
				APIGroups: []string{"batch"},
				Resources: []string{"cronjobs", "jobs"},
				Verbs:     read,
			},
			{
				APIGroups: []string{"autoscaling"},
				Resources: []string{"horizontalpodautoscalers"},
				Verbs:     read,
			},
			{
				APIGroups: []string{"policy"},
				Resources: []string{"poddisruptionbudgets"},
				Verbs:     read,
			},
		},
	}
}

// ClusterMetricsClusterRoleBinding binds the shared ClusterRole to both
// identities: the KSM workload pod and the monitor account the Zabbix
// server authenticates as.
func ClusterMetricsClusterRoleBinding(z *zabbixv1alpha1.Zabbix) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ClusterMetricsRBACName(z),
			Labels: commonLabels(z.Name, ComponentClusterMetrics),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     ClusterMetricsRBACName(z),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      KubeStateMetricsName(z.Name),
				Namespace: z.Namespace,
			},
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      MonitorServiceAccountName(z.Name),
				Namespace: z.Namespace,
			},
		},
	}
}

// ClusterMetricsIsEnabled reports whether the instance opted into
// cluster-metrics ingestion.
func ClusterMetricsIsEnabled(z *zabbixv1alpha1.Zabbix) bool {
	return z.Spec.ClusterMetrics != nil
}
