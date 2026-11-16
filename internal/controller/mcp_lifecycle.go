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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

// reconcileAgentIntegration converges the agent-integration components
// (currently the MCP server). The Deployment/Service convergence flows
// through ConvergeAll (fully owner-referenced, same namespace); this
// handles the disabled/unsupported paths (deterministic deletion, like
// the managed ClickHouse removal) and publishes the readiness condition.
// Returns whether the components are ready, folded into Ready.
func (r *ZabbixReconciler) reconcileAgentIntegration(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) (bool, error) {
	if zabbix.MCPEnabled(z) && !zabbix.SupportsAgentIntegration(z.Spec.Version) {
		// The MCP server does not exist in 7.0: treat as disabled but
		// tell the user why instead of silently dropping the
		// condition. Delete anything an earlier (supported) version
		// deployed, so a version downgrade cleans up deterministically.
		if err := r.removeMCPObjects(ctx, z); err != nil {
			return false, err
		}
		setCond(&status.Conditions, zabbixv1alpha1.ConditionAgentIntegrationReady, metav1.ConditionFalse,
			"MCPUnsupportedVersion",
			fmt.Sprintf("agentIntegration.mcp requires Zabbix 8.0+; instance version is %s", z.Spec.Version),
			z.Generation)
		return false, nil
	}

	if !zabbix.MCPEnabled(z) {
		if err := r.removeMCPObjects(ctx, z); err != nil {
			return false, err
		}
		dropCondition(&status.Conditions, zabbixv1alpha1.ConditionAgentIntegrationReady)
		return true, nil
	}

	ready, err := zabbix.ComponentIsReady(ctx, r.Client, z.Namespace,
		zabbix.MCPServerName(z.Name), 1)
	if err != nil {
		return false, fmt.Errorf("checking MCP server readiness: %w", err)
	}
	setCond(&status.Conditions, zabbixv1alpha1.ConditionAgentIntegrationReady, readyCondition(ready),
		"MCPDeployment", deploymentMessage("MCP server", ready), z.Generation)
	return ready, nil
}

// removeMCPObjects deletes the MCP server's objects by name. Stubs carry
// only the identity: deletion must work even when the spec no longer
// describes the workload. Owner references make this a no-op after the
// resource itself is deleted (garbage collection).
func (r *ZabbixReconciler) removeMCPObjects(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: zabbix.MCPServerName(z.Name), Namespace: z.Namespace,
	}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: zabbix.MCPServerName(z.Name), Namespace: z.Namespace,
	}}
	for _, obj := range []client.Object{dep, svc} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting MCP server object %s: %w", obj.GetName(), err)
		}
	}
	return nil
}
