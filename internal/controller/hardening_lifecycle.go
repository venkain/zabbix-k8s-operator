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
	"strconv"
	"time"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// hardeningRetryDelay is the requeue delay while the API-driven
// hardening (password policy, MFA) is converging or pending. When
// converged it doubles as the drift-check cadence: the settings live
// outside Kubernetes and must be realigned periodically (same cadence
// as the other API-driven features).
const hardeningRetryDelay = 30 * time.Second

// HardeningFinalizer guards the API-side hardening state: the password
// policy and the MFA method live inside Zabbix (which outlives the
// resource through its persistent database), so disabling or deleting
// must revert what the operator set there. Present on every instance
// with an API-driven part active (the default-on password policy makes
// that "every instance"), even before any API call happens — adding it
// late (after the first API write) would race the deletion path.
const HardeningFinalizer = "zabbix.venkain.io/hardening-cleanup"

// ensureHardening converges spec.hardening (default-on) and publishes
// the HardeningReady condition. NOT part of the aggregate Ready: an
// instance without spec.api runs fully Ready with the API-driven
// hardening pending — the condition reports exactly what is pending.
// The Kubernetes-side part (NetworkPolicy) is converged through
// ConvergeAll; this handles the stale-object cleanup and the API side.
// Returns the requeue delay (drift cadence while the API parts are
// active, 0 when hardening is fully opted out).
func (r *ZabbixReconciler) ensureHardening(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) time.Duration {
	log := logf.FromContext(ctx)

	// Release the cleanup finalizer when nothing API-driven is active
	// anymore: the revert runs FIRST (it must not race the release).
	if !zabbix.HardeningNeedsFinalizer(z) && controllerutil.ContainsFinalizer(z, HardeningFinalizer) {
		if err := r.revertHardeningAPIState(ctx, z, status); err != nil {
			// The revert failure keeps the finalizer: the state stays
			// guarded and the next reconcile retries.
			log.Error(err, "Hardening revert failed; keeping the cleanup finalizer")
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"HardeningRevertFailed", err.Error(), z.Generation)
			return hardeningRetryDelay
		}
		controllerutil.RemoveFinalizer(z, HardeningFinalizer)
		if err := r.Update(ctx, z); err != nil {
			log.Error(err, "Could not remove the hardening cleanup finalizer")
			return hardeningRetryDelay
		}
	}

	pending := false

	// --- Kubernetes side: stale NetworkPolicies ---------------------
	if err := r.cleanupHardeningNetworkPolicies(ctx, z); err != nil {
		setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
			"NetworkPolicyCleanupFailed", err.Error(), z.Generation)
		return hardeningRetryDelay
	}

	// --- API side: password policy ----------------------------------
	switch {
	case zabbix.PasswordPolicyEnabled(z):
		if z.Spec.API == nil {
			pending = true
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"APINotConfigured", "password policy enforcement requires spec.api (a Zabbix API token)", z.Generation)
		} else if token, err := apiToken(ctx, r.Client, z); err != nil {
			pending = true
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"APITokenPending", err.Error(), z.Generation)
		} else if desired, applied, err := r.ensurePasswordPolicy(ctx, z, token, status); err != nil {
			pending = true
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"PasswordPolicyFailed", err.Error(), z.Generation)
		} else if applied {
			log.Info("Enforced the password policy through the Zabbix API",
				"minLength", desired.MinLength, "checkRules", desired.CheckRules)
		}
	case status.HardeningPasswordMinLength != 0:
		// Explicit opt-out while the operator had applied a policy:
		// revert to the stock values (only a policy still carrying the
		// operator's values is reset — a hand-tuned policy stays).
		if err := r.revertPasswordPolicy(ctx, z, status); err != nil {
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"PasswordPolicyResetFailed", err.Error(), z.Generation)
			return hardeningRetryDelay
		}
	}

	// --- API side: MFA ----------------------------------------------
	if zabbix.MFARequested(z) {
		switch {
		case !zabbix.SupportsMFA(z.Spec.Version):
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"MFAUnsupported", fmt.Sprintf("MFA requires Zabbix 7.0+; instance runs %s", z.Spec.Version), z.Generation)
		case z.Spec.API == nil:
			pending = true
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"APINotConfigured", "MFA requires spec.api (a Zabbix API token)", z.Generation)
		default:
			token, err := apiToken(ctx, r.Client, z)
			if err != nil {
				pending = true
				setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
					"APITokenPending", err.Error(), z.Generation)
			} else if methodID, changed, err := zabbix.EnsureTOTPMFA(ctx, r.HTTP, zabbix.APIEndpoint(z), token, z); err != nil {
				pending = true
				setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
					"MFAFailed", err.Error(), z.Generation)
			} else {
				status.HardeningMFAMethodID = strconv.FormatInt(methodID, 10)
				if changed {
					log.Info("Enabled TOTP MFA through the Zabbix API",
						"method", zabbix.MFAMethodName(z.Name), "mfaid", methodID)
				}
			}
		}
	} else if status.HardeningMFAMethodID != "" {
		// Explicit opt-out while the operator had enabled MFA: switch
		// the global authentication back off and delete the method
		// (DisableMFA only touches state still linked to the
		// operator's own method). Without a working API the record
		// stays and the revert is retried on the next reconcile.
		if z.Spec.API == nil {
			// The whole API was retired together with the MFA
			// opt-out: the record cannot be acted on — drop it.
			status.HardeningMFAMethodID = ""
		} else if token, err := apiToken(ctx, r.Client, z); err != nil {
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"APITokenPending", err.Error(), z.Generation)
			return hardeningRetryDelay
		} else if err := zabbix.DisableMFA(ctx, r.HTTP, zabbix.APIEndpoint(z), token, z.Name); err != nil {
			setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionFalse,
				"MFARevertFailed", err.Error(), z.Generation)
			return hardeningRetryDelay
		} else {
			log.Info("Disabled the operator-managed MFA", "method", zabbix.MFAMethodName(z.Name))
			status.HardeningMFAMethodID = ""
		}
	}

	// Final condition roll-up: dropped entirely when hardening is fully
	// opted out (nothing left to report), True when everything
	// requested is applied.
	if !zabbix.HardeningNeedsFinalizer(z) && !zabbix.NetworkPolicyHardeningEnabled(z) {
		dropCondition(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady)
		return 0
	}
	if pending {
		return hardeningRetryDelay
	}
	setCond(&status.Conditions, zabbixv1alpha1.ConditionHardeningReady, metav1.ConditionTrue,
		"HardeningApplied", "hardening applied: "+zabbix.DescribeHardening(z), z.Generation)
	return hardeningRetryDelay
}

// ensurePasswordPolicy converges the password policy through the API
// and records the applied values on the status (the revert paths use
// the record to reset only the operator's own values).
func (r *ZabbixReconciler) ensurePasswordPolicy(ctx context.Context, z *zabbixv1alpha1.Zabbix, token string, status *zabbixv1alpha1.ZabbixStatus) (zabbix.PasswordPolicy, bool, error) {
	desired := zabbix.EffectivePasswordPolicy(z)
	applied, err := zabbix.EnsurePasswordPolicy(ctx, r.HTTP, zabbix.APIEndpoint(z), token, desired)
	if err != nil {
		return desired, false, err
	}
	status.HardeningPasswordMinLength = desired.MinLength
	status.HardeningPasswordCheckRules = desired.CheckRules
	return desired, applied, nil
}

// cleanupHardeningNetworkPolicies deletes the policies that are no
// longer desired: the lockdown was opted out, or the workload they
// restrict (cluster-metrics endpoint / MCP server) is gone. Names
// resolve through the same helpers the builders use, so the cleanup
// addresses exactly the objects ConvergeAll created.
func (r *ZabbixReconciler) cleanupHardeningNetworkPolicies(ctx context.Context, z *zabbixv1alpha1.Zabbix) error {
	npEnabled := zabbix.NetworkPolicyHardeningEnabled(z)
	for _, candidate := range []struct {
		desired bool
		obj     client.Object
	}{
		{desired: npEnabled && z.Spec.ClusterMetrics != nil, obj: zabbix.KSMNetworkPolicy(z)},
		{desired: npEnabled && zabbix.MCPEnabled(z), obj: zabbix.MCPNetworkPolicy(z)},
	} {
		if candidate.desired {
			continue
		}
		if err := r.Delete(ctx, candidate.obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting hardening NetworkPolicy %s: %w", candidate.obj.GetName(), err)
		}
	}
	return nil
}

// revertPasswordPolicy reverts ONLY the password policy on the
// password-opt-out path (MFA stays untouched — it may still be
// requested): back to the stock values, but only when the live policy
// still carries the operator's recorded values — a policy a user
// re-tuned by hand in the frontend is never touched. The password
// records are cleared afterwards.
func (r *ZabbixReconciler) revertPasswordPolicy(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) error {
	log := logf.FromContext(ctx)
	defer func() {
		status.HardeningPasswordMinLength = 0
		status.HardeningPasswordCheckRules = 0
	}()
	if z.Spec.API == nil || status.HardeningPasswordMinLength == 0 {
		// Nothing was ever applied through an API this instance does
		// not have (or there is no record): nothing to revert.
		return nil
	}
	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		return err
	}
	// The reset itself guards against touching a hand-tuned policy:
	// it only fires when the live values still match the recorded
	// operator values.
	recorded := zabbix.PasswordPolicy{MinLength: status.HardeningPasswordMinLength, CheckRules: status.HardeningPasswordCheckRules}
	applied, err := zabbix.EnsurePasswordPolicyStock(ctx, r.HTTP, zabbix.APIEndpoint(z), token, recorded)
	if err != nil {
		return err
	}
	if applied {
		log.Info("Reset the password policy to the stock values")
	}
	return nil
}

// revertHardeningAPIState reverts the FULL API-side hardening state —
// used when the API-driven hardening is being retired entirely (all
// knobs opted out, or the resource is being deleted): the password
// policy back to stock (operator's values only) and the operator-
// managed MFA off (only when the global authentication still links to
// the operator's method). The records are cleared afterwards.
func (r *ZabbixReconciler) revertHardeningAPIState(ctx context.Context, z *zabbixv1alpha1.Zabbix, status *zabbixv1alpha1.ZabbixStatus) error {
	if err := r.revertPasswordPolicy(ctx, z, status); err != nil {
		return err
	}
	if z.Spec.API == nil || status.HardeningMFAMethodID == "" {
		// Nothing was ever applied through an API this instance does
		// not have (or no method was ever created): just drop the
		// record.
		status.HardeningMFAMethodID = ""
		return nil
	}
	token, err := apiToken(ctx, r.Client, z)
	if err != nil {
		return err
	}
	if err := zabbix.DisableMFA(ctx, r.HTTP, zabbix.APIEndpoint(z), token, z.Name); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Disabled the operator-managed MFA", "method", zabbix.MFAMethodName(z.Name))
	status.HardeningMFAMethodID = ""
	return nil
}

// finalizeHardened runs when the resource is being deleted: the
// API-side hardening state has no Kubernetes garbage collection to
// lean on. An unreachable Zabbix API keeps the finalizer (requeue with
// backoff) — EXCEPT for a missing token Secret, which cannot
// authenticate no matter how long it waits (mirrors the cluster-metrics
// finalize behavior). The cluster-metrics cleanup runs afterwards.
func (r *ZabbixReconciler) finalizeHardened(ctx context.Context, z *zabbixv1alpha1.Zabbix) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if controllerutil.ContainsFinalizer(z, HardeningFinalizer) {
		if _, err := apiToken(ctx, r.Client, z); err != nil {
			// Without a token the API state cannot be addressed; the
			// policy carries on inside Zabbix until the instance is
			// manually reset.
			log.Error(err, "Skipping hardening cleanup: API token unavailable")
		} else if err := r.revertHardeningAPIState(ctx, z, &z.Status); err != nil {
			// A reachable-but-failing API wedges deletion: the revert
			// is retried on the next reconcile instead of being
			// silently dropped.
			log.Error(err, "Hardening cleanup failed; the finalizer stays")
			return ctrl.Result{}, fmt.Errorf("reverting hardening state on deletion: %w", err)
		}
		controllerutil.RemoveFinalizer(z, HardeningFinalizer)
		if err := r.Update(ctx, z); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing hardening cleanup finalizer: %w", err)
		}
		log.Info("Hardening cleanup finished", "instance", z.Name)
	}
	return r.finalizeClusterMetrics(ctx, z)
}
