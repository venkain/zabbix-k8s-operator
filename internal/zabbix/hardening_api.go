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
	"fmt"
	"net/http"
	"strconv"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

// Zabbix API surface for platform hardening. Wire shapes verified
// against Zabbix 8.0 on the lab: authentication.get returns every field
// as a quoted integer; authentication.update accepts unquoted integers
// and returns the updated field names; mfa.create requires
// type/name/hash_function/code_length for TOTP and returns quoted ids;
// mfa.delete takes a BARE ARRAY of ids (like token.generate) and
// returns the ids unquoted.

// paramMFAID is the authentication.update field for the active MFA
// method (the wire field arrives quoted in results, unquoted in
// updates).
const paramMFAID = "mfaid"

// Authentication is the subset of the authentication object the
// hardening reconciliation reads (authentication.get; integers arrive
// quoted — the `,string` tags match the other API results).
type Authentication struct {
	PasswdMinLength   int64  `json:"passwd_min_length,string"`
	PasswdCheckRules  int64  `json:"passwd_check_rules,string"`
	MFAStatus         int64  `json:"mfa_status,string"`
	MFAID             int64  `json:"mfaid,string"`
	LDAPAuthEnabled   int64  `json:"ldap_auth_enabled,string"`
	LDAPHost          string `json:"ldap_host"`
	LDAPPort          int64  `json:"ldap_port,string"`
	LDAPBindDN        string `json:"ldap_bind_dn"`
	LDAPSearch        string `json:"ldap_search"`
	LDAPUser          string `json:"ldap_user"`
	LDAPCaseSensitive int64  `json:"ldap_case_sensitive,string"`
}

// mfaMethod is one mfa.get entry in wire format.
type mfaMethod struct {
	ID           int64  `json:"mfaid,string"`
	Type         int64  `json:"type,string"`
	Name         string `json:"name"`
	HashFunction int64  `json:"hash_function,string"`
	CodeLength   int64  `json:"code_length,string"`
}

// mfaMethodsResult wraps mfa.create (ids quoted) and mfa.delete (ids
// unquoted — the API is inconsistent here, so the ids decode loosely).
type mfaMethodsResult struct {
	MFAIDs []any `json:"mfaids"`
}

// FetchAuthentication reads the authentication object.
func FetchAuthentication(ctx context.Context, httpClient *http.Client, endpoint, token string) (*Authentication, error) {
	raw, err := callJSONRPC[Authentication](ctx, httpClient, endpoint, token, "authentication.get", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("authentication.get: %w", err)
	}
	return &raw, nil
}

// updateAuthentication applies one authentication.update (params carry
// the changed fields only; the method returns the updated field names).
func updateAuthentication(ctx context.Context, httpClient *http.Client, endpoint, token string, params map[string]any) error {
	_, err := callJSONRPC[[]string](ctx, httpClient, endpoint, token, "authentication.update", params)
	if err != nil {
		return fmt.Errorf("authentication.update: %w", err)
	}
	return nil
}

// EnsurePasswordPolicy converges the password complexity settings to
// the desired policy. Diff-based: unchanged settings are never
// rewritten (authentication.update is a global write — a redundant one
// would churn the audit log). Returns whether a change was applied.
func EnsurePasswordPolicy(ctx context.Context, httpClient *http.Client, endpoint, token string, desired PasswordPolicy) (bool, error) {
	auth, err := FetchAuthentication(ctx, httpClient, endpoint, token)
	if err != nil {
		return false, err
	}
	if int(auth.PasswdMinLength) == desired.MinLength && int(auth.PasswdCheckRules) == desired.CheckRules {
		return false, nil
	}
	if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
		"passwd_min_length":  desired.MinLength,
		"passwd_check_rules": desired.CheckRules,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// EnsurePasswordPolicyStock resets the password policy to the stock
// values of a fresh installation — but ONLY when the live policy still
// carries the operator's recorded values. A policy a user re-tuned by
// hand in the frontend (or one that drifted between the check and the
// call) is never touched. Returns whether a reset was applied.
func EnsurePasswordPolicyStock(ctx context.Context, httpClient *http.Client, endpoint, token string, recorded PasswordPolicy) (bool, error) {
	auth, err := FetchAuthentication(ctx, httpClient, endpoint, token)
	if err != nil {
		return false, err
	}
	stock := PasswordPolicy{MinLength: StockPasswordMinLength, CheckRules: StockPasswordCheckRules}
	if int(auth.PasswdMinLength) == stock.MinLength && int(auth.PasswdCheckRules) == stock.CheckRules {
		return false, nil
	}
	if int(auth.PasswdMinLength) != recorded.MinLength || int(auth.PasswdCheckRules) != recorded.CheckRules {
		return false, nil
	}
	if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
		"passwd_min_length":  stock.MinLength,
		"passwd_check_rules": stock.CheckRules,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// FetchMFAMethods lists the MFA methods (mfa.get with default output
// — the fields the operator matches on are included).
func FetchMFAMethods(ctx context.Context, httpClient *http.Client, endpoint, token string) ([]mfaMethod, error) {
	raw, err := callJSONRPC[[]mfaMethod](ctx, httpClient, endpoint, token, "mfa.get", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("mfa.get: %w", err)
	}
	return raw, nil
}

// fetchMFAByName finds the MFA method by the operator's name, or nil.
func fetchMFAByName(ctx context.Context, httpClient *http.Client, endpoint, token, name string) (*mfaMethod, error) {
	methods, err := FetchMFAMethods(ctx, httpClient, endpoint, token)
	if err != nil {
		return nil, err
	}
	for i := range methods {
		if methods[i].Name == name {
			return &methods[i], nil
		}
	}
	return nil, nil
}

// EnsureTOTPMFA idempotently ensures the instance's TOTP method: found
// by name (created when absent, with the resolved hash/code-length
// parameters), then wired into the global authentication as the active
// MFA (mfa_status=1 + mfaid). Returns the method id and whether the
// authentication state changed.
func EnsureTOTPMFA(ctx context.Context, httpClient *http.Client, endpoint, token string, z *zabbixv1alpha1.Zabbix) (int64, bool, error) {
	name := MFAMethodName(z.Name)
	method, err := fetchMFAByName(ctx, httpClient, endpoint, token, name)
	if err != nil {
		return 0, false, err
	}
	if method == nil {
		created, err := callJSONRPC[mfaMethodsResult](ctx, httpClient, endpoint, token, "mfa.create", mfaMethodFor(z))
		if err != nil {
			return 0, false, fmt.Errorf("mfa.create: %w", err)
		}
		id, err := parseLooseIDs(created.MFAIDs, "mfaids")
		if err != nil {
			return 0, false, err
		}
		method = &mfaMethod{ID: id, Name: name}
	}

	auth, err := FetchAuthentication(ctx, httpClient, endpoint, token)
	if err != nil {
		return method.ID, false, err
	}
	if auth.MFAStatus == 1 && auth.MFAID == method.ID {
		return method.ID, false, nil
	}
	if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
		"mfa_status": 1,
		paramMFAID:   intToStringID(method.ID),
	}); err != nil {
		return method.ID, false, err
	}
	return method.ID, true, nil
}

// DisableMFA reverts the operator-managed MFA: the global
// authentication is switched off ONLY when it still points at the
// operator's method, then the method itself is deleted. Both steps
// tolerate absence (MFA already off, method already deleted).
func DisableMFA(ctx context.Context, httpClient *http.Client, endpoint, token, instance string) error {
	auth, err := FetchAuthentication(ctx, httpClient, endpoint, token)
	if err != nil {
		return err
	}
	if auth.MFAStatus != 0 {
		if auth.MFAID != 0 {
			// Only a link to the operator's own method is reverted; a
			// link to a user-configured method is left alone.
			if method, err := fetchMFAByName(ctx, httpClient, endpoint, token, MFAMethodName(instance)); err != nil {
				return err
			} else if method != nil && auth.MFAID == method.ID {
				if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
					"mfa_status": 0,
					paramMFAID:   0,
				}); err != nil {
					return err
				}
			}
		}
	} else if auth.MFAID != 0 {
		// Status off with a stale method id: clear the id so the
		// frontend does not carry a dangling reference.
		if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
			paramMFAID: 0,
		}); err != nil {
			return err
		}
	}

	method, err := fetchMFAByName(ctx, httpClient, endpoint, token, MFAMethodName(instance))
	if err != nil {
		return err
	}
	if method == nil {
		return nil
	}
	// mfa.delete takes a BARE ARRAY of ids (verified against 8.0).
	if _, err := callJSONRPC[mfaMethodsResult](ctx, httpClient, endpoint, token, "mfa.delete",
		[]string{intToStringID(method.ID)}); err != nil {
		return fmt.Errorf("mfa.delete: %w", err)
	}
	return nil
}

// EnsureLDAP converges LDAP authentication settings via
// authentication.update. Returns whether a change was applied.
func EnsureLDAP(ctx context.Context, httpClient *http.Client, endpoint, token string,
	ldap *zabbixv1alpha1.LDAPSpec, bindPassword string) (bool, error) {
	auth, err := FetchAuthentication(ctx, httpClient, endpoint, token)
	if err != nil {
		return false, err
	}

	desired := map[string]any{
		"ldap_auth_enabled":   boolToInt(ldap.Enabled),
		"ldap_host":           ldap.Host,
		"ldap_port":           ldap.Port,
		"ldap_bind_dn":        ldap.BindDN,
		"ldap_bind_password":  bindPassword,
		"ldap_search":         ldap.SearchBase,
		"ldap_user":           ldap.UserAttribute,
		"ldap_case_sensitive": boolToInt(ldap.CaseSensitive),
	}

	// Only check fields that are relevant. If LDAP is disabled, only
	// check ldap_auth_enabled.
	if !ldap.Enabled {
		if auth.LDAPAuthEnabled == 0 {
			return false, nil
		}
		if err := updateAuthentication(ctx, httpClient, endpoint, token, map[string]any{
			"ldap_auth_enabled": 0,
		}); err != nil {
			return false, err
		}
		return true, nil
	}

	// Compare current vs desired.
	if auth.LDAPAuthEnabled != boolToInt(ldap.Enabled) ||
		auth.LDAPHost != ldap.Host ||
		auth.LDAPPort != int64(ldap.Port) ||
		auth.LDAPBindDN != ldap.BindDN ||
		auth.LDAPSearch != ldap.SearchBase ||
		auth.LDAPUser != ldap.UserAttribute ||
		auth.LDAPCaseSensitive != boolToInt(ldap.CaseSensitive) {
		return false, updateAuthentication(ctx, httpClient, endpoint, token, desired)
	}
	return false, nil
}

// boolToInt converts a bool to int64 (0 or 1) for Zabbix API params.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// parseLooseIDs decodes a mutation id list whose members may arrive
// quoted or unquoted (mfa.create quotes them, mfa.delete does not).
func parseLooseIDs(ids []any, field string) (int64, error) {
	for _, raw := range ids {
		switch v := raw.(type) {
		case float64:
			return int64(v), nil
		case string:
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing %s id %q: %w", field, v, err)
			}
			return id, nil
		}
	}
	return 0, fmt.Errorf("%s response has no ids", field)
}
