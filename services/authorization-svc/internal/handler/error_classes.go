package handler

// Governance Control Plane §16 names the stable error classes a caller may
// rely on. This service's error codes predate them and callers match on the
// codes, so the class is ADDED beside the code ("error_class") rather than
// replacing it. A code with no §16 counterpart — a validation error, a
// not-found — carries no class rather than a forced one.
var errorClasses = map[string]string{
	// CONTEXT_UNRESOLVED: no trusted principal / tenant / scope.
	"missing_principal":             "CONTEXT_UNRESOLVED",
	"missing_tenant_scope":          "CONTEXT_UNRESOLVED",
	"invalid_scope":                 "CONTEXT_UNRESOLVED",
	"tenant_scope_mismatch":         "CONTEXT_UNRESOLVED",
	"platform_scope_not_configured": "CONTEXT_UNRESOLVED",

	// AUTHORIZATION_DENIED.
	"authorization_denied":              "AUTHORIZATION_DENIED",
	"forbidden":                         "AUTHORIZATION_DENIED",
	"self_grant_not_allowed":            "AUTHORIZATION_DENIED",
	"self_delegation_not_allowed":       "AUTHORIZATION_DENIED",
	"delegator_must_be_caller":          "AUTHORIZATION_DENIED",
	"only_delegator_may_revoke":         "AUTHORIZATION_DENIED",
	"principal_not_active":              "AUTHORIZATION_DENIED",
	"protected_privilege_not_delegable": "AUTHORIZATION_DENIED",
	"checker_not_independent":           "AUTHORIZATION_DENIED",

	// SOD_CONFLICT.
	"sod_conflict":      "SOD_CONFLICT",
	"sod_self_interest": "SOD_CONFLICT",

	// APPROVAL_INVALIDATED: the approval being acted on is no longer valid.
	"approval_expired":     "APPROVAL_INVALIDATED",
	"approval_not_pending": "APPROVAL_INVALIDATED",

	// CONCURRENCY_CONFLICT: the object is not in the state the command assumed.
	"version_conflict":   "CONCURRENCY_CONFLICT",
	"invalid_state":      "CONCURRENCY_CONFLICT",
	"invalid_transition": "CONCURRENCY_CONFLICT",
	"already_revoked":    "CONCURRENCY_CONFLICT",
	"already_completed":  "CONCURRENCY_CONFLICT",

	// IDEMPOTENCY_MISMATCH (also set by internal/idempotency).
	"idempotency_mismatch":  "IDEMPOTENCY_MISMATCH",
	"idempotency_in_flight": "IDEMPOTENCY_MISMATCH",
}

// ErrorClass returns the §16 class for an error code, or "".
func ErrorClass(code string) string { return errorClasses[code] }

// withErrorClass adds error_class to an error body that has an "error" code
// with a §16 counterpart. Never mutates the caller's map.
func withErrorClass(status int, v any) any {
	if status < 400 {
		return v
	}
	switch m := v.(type) {
	case map[string]string:
		if c := errorClasses[m["error"]]; c != "" {
			out := make(map[string]string, len(m)+1)
			for k, val := range m {
				out[k] = val
			}
			out["error_class"] = c
			return out
		}
	case map[string]any:
		code, _ := m["error"].(string)
		if c := errorClasses[code]; c != "" {
			out := make(map[string]any, len(m)+1)
			for k, val := range m {
				out[k] = val
			}
			out["error_class"] = c
			return out
		}
	}
	return v
}
