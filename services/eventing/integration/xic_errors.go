package integration

import (
	"errors"
	"fmt"
	"net/http"
)

// XICCode is a stable ZS-SVC-T-001 error code (§14.2). The M-001 taxonomy (Code) and this one describe the same
// estate from two specifications; they are kept apart so neither is ever renumbered to fit the other. A code
// is part of the external contract: it is never renumbered or reused.
type XICCode string

const (
	XICConnectionNotActive      XICCode = "XIC-001" // connection draft/suspended/revoked
	XICContractVersionMismatch  XICCode = "XIC-002" // unsupported contract/source version
	XICSchemaVersionUnsupported XICCode = "XIC-003" // inbound source schema unsupported
	XICAuthenticationFailed     XICCode = "XIC-004" // OAuth/mTLS/HMAC/key invalid
	XICReplayWindowFailed       XICCode = "XIC-005" // timestamp/nonce/replay invalid
	XICDuplicateExchange        XICCode = "XIC-006" // source/idempotency duplicate
	XICMappingFailed            XICCode = "XIC-007" // deterministic mapping cannot produce a valid canonical payload
	XICTenantScopeMismatch      XICCode = "XIC-008" // connection/credential/object belongs to another tenant
	XICEgressTargetBlocked      XICCode = "XIC-009" // endpoint/redirect/DNS/IP violates egress policy
	XICOutboundUnknown          XICCode = "XIC-010" // provider outcome ambiguous
	XICCallbackInvalid          XICCode = "XIC-011" // signature/tenant/transition/dedupe failure
	XICRateLimited              XICCode = "XIC-012" // provider/Zoiko budget reached
	XICBatchIntegrityFailed     XICCode = "XIC-013" // checksum/manifest/encryption mismatch
	XICBatchPartial             XICCode = "XIC-014" // mixed valid/invalid rows
	XICReplayApprovalRequired   XICCode = "XIC-015" // replay exceeds risk threshold
	XICResidencyRouteBlocked    XICCode = "XIC-016" // provider/region transfer disallowed
	XICCertificationStale       XICCode = "XIC-017" // material change invalidated certification
	XICReconciliationVariance   XICCode = "XIC-018" // independent records do not match
)

// XICInfo is the fixed meaning of an XICCode.
type XICInfo struct {
	Name string
	// Title is the RFC 9457 title.
	Title string
	// Status is the HTTP status a synchronous caller sees.
	Status int
	// Behavior is the required behavior of §14.2, in the spec's own words, for runbooks and contract tests.
	Behavior string
	// Retryable says whether the same request may be sent again unchanged. An ambiguous outcome never is: the
	// original attempt is resolved first (I-14).
	Retryable bool
	// SecuritySignal marks codes that raise security telemetry in addition to the refusal.
	SecuritySignal bool
	// Quarantine marks codes whose subject is held, not discarded.
	Quarantine bool
}

var xicInfos = map[XICCode]XICInfo{
	XICConnectionNotActive:      {"CONNECTION_NOT_ACTIVE", "Connection is not active", http.StatusConflict, "Block.", false, false, false},
	XICContractVersionMismatch:  {"CONTRACT_VERSION_MISMATCH", "Contract version is not supported", http.StatusUnprocessableEntity, "Reject or quarantine.", false, false, true},
	XICSchemaVersionUnsupported: {"SCHEMA_VERSION_UNSUPPORTED", "Source schema version is not supported", http.StatusUnprocessableEntity, "Quarantine or reject; no guessed mapping.", false, false, true},
	XICAuthenticationFailed:     {"AUTHENTICATION_FAILED", "Authentication failed", http.StatusUnauthorized, "Reject and raise security telemetry.", false, true, false},
	XICReplayWindowFailed:       {"REPLAY_WINDOW_FAILED", "Replay window check failed", http.StatusUnauthorized, "Reject.", false, true, false},
	XICDuplicateExchange:        {"DUPLICATE_EXCHANGE", "Duplicate exchange", http.StatusConflict, "Return the original reference; no duplicate effect.", false, false, false},
	XICMappingFailed:            {"MAPPING_FAILED", "Mapping could not produce a valid canonical payload", http.StatusUnprocessableEntity, "Quarantine.", false, false, true},
	XICTenantScopeMismatch:      {"TENANT_SCOPE_MISMATCH", "Tenant scope mismatch", http.StatusForbidden, "Reject and raise an incident signal.", false, true, false},
	XICEgressTargetBlocked:      {"EGRESS_TARGET_BLOCKED", "Egress target blocked", http.StatusForbidden, "Block and raise security telemetry.", false, true, false},
	XICOutboundUnknown:          {"OUTBOUND_UNKNOWN", "Outbound outcome is unknown", http.StatusAccepted, "Resolve the original attempt; no blind new attempt.", false, false, false},
	XICCallbackInvalid:          {"CALLBACK_INVALID", "Callback is invalid", http.StatusUnauthorized, "Reject; retain metadata.", false, true, false},
	XICRateLimited:              {"RATE_LIMITED", "Rate limited", http.StatusTooManyRequests, "Back off and schedule the same attempt.", true, false, false},
	XICBatchIntegrityFailed:     {"BATCH_INTEGRITY_FAILED", "Batch integrity check failed", http.StatusUnprocessableEntity, "Reject the batch.", false, false, true},
	XICBatchPartial:             {"BATCH_PARTIAL", "Batch partially accepted", http.StatusMultiStatus, "Preserve row outcomes; reconcile.", false, false, false},
	XICReplayApprovalRequired:   {"REPLAY_APPROVAL_REQUIRED", "Replay needs approval", http.StatusAccepted, "Route to workflow approval.", false, false, false},
	XICResidencyRouteBlocked:    {"RESIDENCY_ROUTE_BLOCKED", "Residency route blocked", http.StatusForbidden, "Block before the payload leaves.", false, true, false},
	XICCertificationStale:       {"CERTIFICATION_STALE", "Certification is stale", http.StatusConflict, "Block production activation or operation as policy requires.", false, false, false},
	XICReconciliationVariance:   {"RECONCILIATION_VARIANCE", "Reconciliation variance", http.StatusConflict, "Open a variance and case; no silent patch.", false, false, false},
}

// XICCodes returns every defined code, in numeric order, for contract tests and documentation.
func XICCodes() []XICCode {
	out := make([]XICCode, 0, len(xicInfos))
	for i := 1; i <= len(xicInfos); i++ {
		out = append(out, XICCode(fmt.Sprintf("XIC-%03d", i)))
	}
	return out
}

// Valid reports whether c is a defined code.
func (c XICCode) Valid() bool { _, ok := xicInfos[c]; return ok }

// Info returns the fixed meaning of c. An undefined code yields a 500 so a typo can never read as a client error.
func (c XICCode) Info() XICInfo {
	if i, ok := xicInfos[c]; ok {
		return i
	}
	return XICInfo{Name: "UNDEFINED", Title: "Undefined integration error code", Status: http.StatusInternalServerError}
}

// XICError is an exchange-control failure carrying a stable XICCode. Detail is a short sentence that never holds
// credentials, internal hostnames, stack traces or payload fragments (§14.3).
type XICError struct {
	Code         XICCode
	Detail       string
	ConnectionID string // only when safe to disclose to the caller
	Remediation  string
	Cause        error
}

// NewXIC builds an XICError.
func NewXIC(code XICCode, detail string) *XICError { return &XICError{Code: code, Detail: detail} }

// Wrap attaches an underlying cause. It is visible to errors.Is/As and logs, never to the problem body.
func (e *XICError) Wrap(cause error) *XICError { e.Cause = cause; return e }

// WithConnection names the connection when it is safe to disclose.
func (e *XICError) WithConnection(id string) *XICError { e.ConnectionID = id; return e }

// WithRemediation sets the advice returned to the caller.
func (e *XICError) WithRemediation(s string) *XICError { e.Remediation = s; return e }

func (e *XICError) Error() string {
	if e.Detail == "" {
		return string(e.Code) + " " + e.Code.Info().Name
	}
	return string(e.Code) + " " + e.Code.Info().Name + ": " + e.Detail
}

func (e *XICError) Unwrap() error { return e.Cause }

// AsXIC extracts an XICError from err.
func AsXIC(err error) (*XICError, bool) {
	var x *XICError
	if errors.As(err, &x) {
		return x, true
	}
	return nil, false
}

// XICProblem renders the RFC 9457 application/problem+json body of §14.3: a stable type URI, title, status,
// the controlled XIC code, trace_id, connection_id when safe, and remediation. Nothing else is ever added.
func XICProblem(e *XICError, traceID string) map[string]any {
	info := e.Code.Info()
	body := map[string]any{
		"type":      ProblemTypePrefix + string(e.Code),
		"title":     info.Title,
		"status":    info.Status,
		"code":      string(e.Code),
		"name":      info.Name,
		"detail":    e.Detail,
		"retryable": info.Retryable,
	}
	if traceID != "" {
		body["trace_id"] = traceID
	}
	if e.ConnectionID != "" {
		body["connection_id"] = e.ConnectionID
	}
	if e.Remediation != "" {
		body["remediation"] = e.Remediation
	}
	return body
}
