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

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Platform hardening (spec.hardening): the resolution of the default-on
// spec knobs into the values the API calls and the NetworkPolicy
// builders need. Unlike the optional features (presence = enabled),
// hardening is inverted: an absent block means the hardened defaults —
// only explicit opt-outs turn parts off. Wire contracts verified
// against Zabbix 8.0 on the lab (authentication object and mfa object).

const (
	// DefaultPasswordMinLength is the enforced minimum password length
	// (Zabbix passwd_min_length) when spec.hardening.passwordPolicy
	// does not override it.
	DefaultPasswordMinLength = 12

	// stock password-policy values of a fresh Zabbix installation —
	// what the disable/deletion paths reset to (only a policy still
	// carrying the operator's values is reset).
	StockPasswordMinLength  = 8
	StockPasswordCheckRules = 8

	// Password check-rule bits (Zabbix passwd_check_rules bitmask):
	// the length check is governed by passwd_min_length itself, the
	// four configurable rules are the bits below.
	PasswordRuleCase            = 1
	PasswordRuleDigits          = 2
	PasswordRuleSpecial         = 4
	PasswordRuleCommonPasswords = 8

	// DefaultMFAHashFunction and DefaultMFACodeLength are the TOTP
	// method parameters the operator ensures when spec.hardening.mfa
	// does not override them (mfa.hash_function 3 = SHA-512, code
	// length 8; both verified against the live API).
	DefaultMFAHashFunction = 3
	DefaultMFACodeLength   = 8

	// mfa wire values: method type 1 = TOTP.
	mfaTypeTOTP = 1
)

// passwordRuleBits maps the CRD enum names to their Zabbix
// passwd_check_rules bits.
var passwordRuleBits = map[string]int{
	"case":            PasswordRuleCase,
	"digits":          PasswordRuleDigits,
	"special":         PasswordRuleSpecial,
	"commonPasswords": PasswordRuleCommonPasswords,
}

// PasswordPolicy is the resolved wire-shaped password policy.
type PasswordPolicy struct {
	// MinLength is passwd_min_length.
	MinLength int
	// CheckRules is the passwd_check_rules bitmask.
	CheckRules int
}

// PasswordPolicyEnabled reports whether the password policy is
// enforced on this instance (default: yes; explicit opt-out:
// spec.hardening.passwordPolicy.enabled = false).
func PasswordPolicyEnabled(z *zabbixv1alpha1.Zabbix) bool {
	spec := z.Spec.Hardening
	if spec == nil || spec.PasswordPolicy == nil {
		return true
	}
	if spec.PasswordPolicy.Enabled != nil {
		return *spec.PasswordPolicy.Enabled
	}
	return true
}

// EffectivePasswordPolicy resolves the password policy the operator
// enforces (spec overrides folded onto the hardened defaults).
func EffectivePasswordPolicy(z *zabbixv1alpha1.Zabbix) PasswordPolicy {
	policy := PasswordPolicy{
		MinLength:  DefaultPasswordMinLength,
		CheckRules: PasswordRuleCase | PasswordRuleDigits | PasswordRuleSpecial | PasswordRuleCommonPasswords,
	}
	spec := z.Spec.Hardening
	if spec == nil || spec.PasswordPolicy == nil {
		return policy
	}
	if spec.PasswordPolicy.MinLength > 0 {
		policy.MinLength = spec.PasswordPolicy.MinLength
	}
	if len(spec.PasswordPolicy.CheckRules) > 0 {
		rules := 0
		for _, rule := range spec.PasswordPolicy.CheckRules {
			rules |= passwordRuleBits[rule]
		}
		policy.CheckRules = rules
	}
	return policy
}

// NetworkPolicyHardeningEnabled reports whether the NetworkPolicy
// lockdown of the auxiliary workloads is requested (default: yes).
func NetworkPolicyHardeningEnabled(z *zabbixv1alpha1.Zabbix) bool {
	spec := z.Spec.Hardening
	if spec == nil || spec.NetworkPolicy == nil {
		return true
	}
	return *spec.NetworkPolicy
}

// MFARequested reports whether the instance opted into MFA (the
// presence of spec.hardening.mfa enables it).
func MFARequested(z *zabbixv1alpha1.Zabbix) bool {
	spec := z.Spec.Hardening
	return spec != nil && spec.MFA != nil
}

// SupportsMFA reports whether the Zabbix release supports MFA (7.0+;
// the mfa_* authentication fields and mfa.* API methods appeared in
// 7.0).
func SupportsMFA(version string) bool {
	major, ok := majorVersion(version)
	return ok && major >= 7
}

// MFAHashFunction resolves the TOTP hash function code (mfa object:
// 1 = SHA-1, 2 = SHA-256, 3 = SHA-512; default SHA-512).
func MFAHashFunction(z *zabbixv1alpha1.Zabbix) int {
	spec := z.Spec.Hardening
	if spec == nil || spec.MFA == nil || spec.MFA.HashFunction == "" {
		return DefaultMFAHashFunction
	}
	switch spec.MFA.HashFunction {
	case "sha1":
		return 1
	case "sha256":
		return 2
	default:
		return 3
	}
}

// MFACodeLength resolves the TOTP code length (mfa.code_length: 6 or
// 8; default 8).
func MFACodeLength(z *zabbixv1alpha1.Zabbix) int {
	spec := z.Spec.Hardening
	if spec == nil || spec.MFA == nil || spec.MFA.CodeLength == 0 {
		return DefaultMFACodeLength
	}
	return spec.MFA.CodeLength
}

// MFAMethodName is the name of the TOTP method the operator ensures
// for this instance (mfa methods are global Zabbix objects; the name
// embeds the instance so disable/deletion can find it by name even
// when the status record was lost).
func MFAMethodName(instance string) string { return instance + "-totp" }

// mfaMethodFor renders the wire parameters of the instance's TOTP
// method.
func mfaMethodFor(z *zabbixv1alpha1.Zabbix) map[string]any {
	return map[string]any{
		"type":          mfaTypeTOTP,
		paramName:       MFAMethodName(z.Name),
		"hash_function": MFAHashFunction(z),
		"code_length":   MFACodeLength(z),
	}
}

// HardeningNeedsFinalizer reports whether the hardening cleanup
// finalizer must be present: the API-side state (password policy,
// MFA method) outlives Kubernetes garbage collection, so the disable
// and deletion paths need the hook to revert it.
func HardeningNeedsFinalizer(z *zabbixv1alpha1.Zabbix) bool {
	return PasswordPolicyEnabled(z) || MFARequested(z)
}

// DescribeHardening renders the effective hardening summary for
// condition messages and logs.
func DescribeHardening(z *zabbixv1alpha1.Zabbix) string {
	policy := EffectivePasswordPolicy(z)
	summary := fmt.Sprintf("password policy (min length %d, rules bitmask %d)", policy.MinLength, policy.CheckRules)
	if MFARequested(z) {
		summary += fmt.Sprintf(", TOTP MFA (hash %d, code length %d)", MFAHashFunction(z), MFACodeLength(z))
	} else {
		summary += ", MFA off"
	}
	if NetworkPolicyHardeningEnabled(z) {
		summary += ", NetworkPolicy lockdown on"
	}
	return summary
}
