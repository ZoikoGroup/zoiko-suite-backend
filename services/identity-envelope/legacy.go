package idenv

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Legacy error bodies → RFC 9457 (Governance audit X9, AU-24).
//
// The estate's handlers answer errors as {"error": "<snake_code>", ...}, and
// callers (the console's readErrorDetail among them) branch on that code.
// WriteLegacyError renders the same body as an application/problem+json
// document: the RFC 9457 members are added, "error" (and "code") keep their
// values, every other member is carried as an extension, and error_class
// names the Governance Control Plane §16 stable class when the code has one.
// A code with no §16 counterpart (a validation error, a not-found) carries no
// class rather than a forced one.

var (
	classMu      sync.RWMutex
	errorClasses = map[string]string{
		// CONTEXT_UNRESOLVED: no trusted principal, tenant or scope.
		"identity_missing":            "CONTEXT_UNRESOLVED",
		"missing_principal":           "CONTEXT_UNRESOLVED",
		"principal_missing":           "CONTEXT_UNRESOLVED",
		"tenant_missing":              "CONTEXT_UNRESOLVED",
		"missing_tenant":              "CONTEXT_UNRESOLVED",
		"missing_tenant_scope":        "CONTEXT_UNRESOLVED",
		"invalid_tenant":              "CONTEXT_UNRESOLVED",
		"tenant_scope_mismatch":       "CONTEXT_UNRESOLVED",
		"legal_entity_mismatch":       "CONTEXT_UNRESOLVED",
		"legal_entity_not_found":      "CONTEXT_UNRESOLVED",
		"legal_entity_not_in_tenant":  "CONTEXT_UNRESOLVED",
		"envelope_incomplete":         "CONTEXT_UNRESOLVED",
		"identity_assertion_required": "CONTEXT_UNRESOLVED",
		"identity_assertion_invalid":  "CONTEXT_UNRESOLVED",
		"identity_audience_mismatch":  "CONTEXT_UNRESOLVED",
		"identity_mismatch":           "CONTEXT_UNRESOLVED",
		"session_revoked":             "CONTEXT_UNRESOLVED",
		"context_unresolved":          "CONTEXT_UNRESOLVED",

		// AUTHORIZATION_DENIED.
		"forbidden":              "AUTHORIZATION_DENIED",
		"authorization_denied":   "AUTHORIZATION_DENIED",
		"access_denied":          "AUTHORIZATION_DENIED",
		"not_authorized":         "AUTHORIZATION_DENIED",
		"wrong_approver":         "AUTHORIZATION_DENIED",
		"approver_not_eligible":  "AUTHORIZATION_DENIED",
		"approver_not_human":     "AUTHORIZATION_DENIED",
		"delegation_denied":      "AUTHORIZATION_DENIED",
		"step_up_required":       "AUTHORIZATION_DENIED",
		"authorization_required": "AUTHORIZATION_DENIED",

		// SOD_CONFLICT.
		"sod_conflict":              "SOD_CONFLICT",
		"self_approval_forbidden":   "SOD_CONFLICT",
		"self_approval_not_allowed": "SOD_CONFLICT",
		"self_approval_blocked":     "SOD_CONFLICT",
		"initiator_cannot_approve":  "SOD_CONFLICT",
		"maker_checker_violation":   "SOD_CONFLICT",
		"checker_not_independent":   "SOD_CONFLICT",

		// APPROVAL_REQUIRED / APPROVAL_INVALIDATED.
		"approval_required":    "APPROVAL_REQUIRED",
		"approval_invalidated": "APPROVAL_INVALIDATED",
		"approval_expired":     "APPROVAL_INVALIDATED",
		"approval_not_valid":   "APPROVAL_INVALIDATED",
		"approval_not_pending": "APPROVAL_INVALIDATED",
		"workflow_invalidated": "APPROVAL_INVALIDATED",
		"workflow_expired":     "APPROVAL_INVALIDATED",

		// CONCURRENCY_CONFLICT: the object is not in the state the command assumed.
		"version_mismatch":     "CONCURRENCY_CONFLICT",
		"version_conflict":     "CONCURRENCY_CONFLICT",
		"concurrency_conflict": "CONCURRENCY_CONFLICT",
		"precondition_failed":  "CONCURRENCY_CONFLICT",
		"stale_version":        "CONCURRENCY_CONFLICT",
		"invalid_transition":   "CONCURRENCY_CONFLICT",
		"invalid_state":        "CONCURRENCY_CONFLICT",
		"already_retired":      "CONCURRENCY_CONFLICT",
		"already_closed":       "CONCURRENCY_CONFLICT",
		"already_revoked":      "CONCURRENCY_CONFLICT",
		"already_completed":    "CONCURRENCY_CONFLICT",

		// IDEMPOTENCY_MISMATCH.
		"idempotency_mismatch":     "IDEMPOTENCY_MISMATCH",
		"idempotency_key_conflict": "IDEMPOTENCY_MISMATCH",
		"idempotency_conflict":     "IDEMPOTENCY_MISMATCH",
		"idempotency_in_flight":    "IDEMPOTENCY_MISMATCH",

		// The remaining §16 classes, under their own names.
		"policy_not_effective":      "POLICY_NOT_EFFECTIVE",
		"feature_disabled":          "FEATURE_DISABLED",
		"legal_hold_active":         "LEGAL_HOLD_ACTIVE",
		"retention_blocked":         "RETENTION_BLOCKED",
		"residency_denied":          "RESIDENCY_DENIED",
		"evidence_integrity_failed": "EVIDENCE_INTEGRITY_FAILED",
		"fingerprint_mismatch":      "EVIDENCE_INTEGRITY_FAILED",
		"contract_incompatible":     "CONTRACT_INCOMPATIBLE",
		"break_glass_required":      "BREAK_GLASS_REQUIRED",
		"break_glass_expired":       "BREAK_GLASS_EXPIRED",
	}
)

// RegisterErrorClasses adds service-specific code → §16 class mappings. Codes
// are matched case-insensitively.
func RegisterErrorClasses(m map[string]string) {
	classMu.Lock()
	defer classMu.Unlock()
	for code, class := range m {
		errorClasses[strings.ToLower(code)] = class
	}
}

// ErrorClass returns the §16 stable class for an error code, "" when it has
// none.
func ErrorClass(code string) string {
	classMu.RLock()
	defer classMu.RUnlock()
	return errorClasses[strings.ToLower(strings.TrimSpace(code))]
}

// ProblemFromLegacy converts a legacy error body. ok is false when v carries
// no string "error" (or "code") member, so it is not an error body this
// adapter understands.
func ProblemFromLegacy(status int, v any) (Problem, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Problem{}, false
	}
	body := map[string]any{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Problem{}, false
	}
	legacy, _ := body["error"].(string)
	code, _ := body["code"].(string)
	if legacy == "" && code == "" {
		return Problem{}, false
	}
	if code == "" {
		code = legacy
	}
	if legacy == "" {
		legacy = code
	}
	p := Problem{
		Status:    status,
		Code:      code,
		Error:     legacy,
		Retryable: status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests,
		Extra:     map[string]any{},
	}
	for _, k := range []string{"detail", "message"} {
		if s, ok := body[k].(string); ok && s != "" {
			p.Detail = s
			break
		}
	}
	if s, ok := body["title"].(string); ok {
		p.Title = s
	}
	if s, ok := body["type"].(string); ok && strings.Contains(s, "/") {
		p.Type = s
	}
	for k, val := range body {
		switch k {
		case "error", "code", "detail", "title", "type", "status":
			continue
		}
		p.Extra[k] = val
	}
	if _, set := p.Extra["error_class"]; !set {
		class := ErrorClass(legacy)
		if class == "" {
			class = ErrorClass(code)
		}
		if class != "" {
			p.Extra["error_class"] = class
		}
	}
	return p, true
}

// WriteLegacyError renders a legacy error body as application/problem+json and
// reports true. It writes nothing and reports false when status is not an
// error or v is not a legacy error body, so the caller writes it as before.
// r may be nil; the request and correlation ids are then omitted.
func WriteLegacyError(w http.ResponseWriter, r *http.Request, status int, v any) bool {
	if status < 400 {
		return false
	}
	p, ok := ProblemFromLegacy(status, v)
	if !ok {
		return false
	}
	WriteProblem(w, r, p)
	return true
}
