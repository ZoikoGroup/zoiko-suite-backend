// Package integration is the shared runtime vocabulary for ZS-SVC-M-001
// (Integration, File Exchange & External Connectivity): the trusted
// integration context (§2.1), the stable error/quarantine taxonomy (§2.2) with
// RFC 9457 problem details, the multi-dimensional outcome and evidence record
// (§0, §1.1 invariant 25), sensitive-telemetry redaction (INT-CTRL-035) and a
// client for the contract registry (schema-registry-svc).
//
// It lives in services/eventing because that is the module every integration
// producer already adopts; it is a library, not a service.
package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Code is a stable INT error / quarantine code (ZS-SVC-M-001 §2.2). Codes are
// part of the external contract: they are never renumbered or reused.
type Code string

const (
	CodeContextInvalid      Code = "INT-CTX-001"    // invalid or unresolved tenant/context
	CodeResidencyDenied     Code = "INT-REG-002"    // residency/egress policy denied
	CodeAuthDenied          Code = "INT-AUTH-003"   // authentication/authorization denied
	CodeSchemaMismatch      Code = "INT-SCHEMA-004" // contract/schema/profile mismatch
	CodeIdempotencyConflict Code = "INT-IDEM-005"   // duplicate or conflicting idempotency subject
	CodeExternalPending     Code = "INT-EXT-006"    // external outcome pending/unknown
	CodeRateLimited         Code = "INT-RATE-007"   // provider/gateway rate limited
	CodeQuarantined         Code = "INT-QUAR-008"   // payload/message/item quarantined
	CodeSignatureInvalid    Code = "INT-SIG-009"    // signature/callback verification failed
	CodeCredentialInvalid   Code = "INT-CRED-010"   // credential/consent expired or revoked
	CodeReconciliationBreak Code = "INT-RECON-011"  // integration/domain reconciliation mismatch
	CodeVersionUnsupported  Code = "INT-VERS-012"   // unsupported/deprecated provider version
	CodeScopeDenied         Code = "INT-SCOPE-013"  // connector scope/purpose denied
	CodeFileIntegrity       Code = "INT-FILE-014"   // file manifest/hash/integrity failure
	CodeCallbackConflict    Code = "INT-CB-015"     // callback replay/out-of-order conflict
	CodeBackpressure        Code = "INT-BP-016"     // backpressure/admission protection active
)

// Info is the fixed meaning of a Code.
type Info struct {
	Title string
	// Status is the HTTP status a synchronous caller sees. INT-EXT-006 is 202:
	// an unknown external outcome is not a failure ("Timeout = failed" is a
	// prohibited anti-pattern, §16), so it must not be reported as one.
	Status int
	// Retryable says whether the same request may be sent again unchanged.
	// Pending/unknown outcomes are NOT retryable: the original attempt must be
	// resolved first (§1.1 invariant 9, §8 "Unknown outcomes").
	Retryable bool
	// Quarantine marks codes whose subject is held, not discarded (§1.1
	// invariant 20: no silent drop; every terminal failure is evidenced).
	Quarantine bool
}

var infos = map[Code]Info{
	CodeContextInvalid:      {"Invalid or unresolved tenant/context", http.StatusBadRequest, false, false},
	CodeResidencyDenied:     {"Residency or egress policy denied", http.StatusForbidden, false, false},
	CodeAuthDenied:          {"Authentication or authorization denied", http.StatusForbidden, false, false},
	CodeSchemaMismatch:      {"Contract, schema or profile mismatch", http.StatusUnprocessableEntity, false, false},
	CodeIdempotencyConflict: {"Duplicate or conflicting idempotency subject", http.StatusConflict, false, false},
	CodeExternalPending:     {"External outcome pending or unknown", http.StatusAccepted, false, false},
	CodeRateLimited:         {"Rate limited", http.StatusTooManyRequests, true, false},
	CodeQuarantined:         {"Item quarantined", http.StatusUnprocessableEntity, false, true},
	CodeSignatureInvalid:    {"Signature or callback verification failed", http.StatusUnauthorized, false, false},
	CodeCredentialInvalid:   {"Credential or consent expired or revoked", http.StatusUnauthorized, false, false},
	CodeReconciliationBreak: {"Integration and domain reconciliation mismatch", http.StatusConflict, false, false},
	CodeVersionUnsupported:  {"Unsupported or deprecated version", http.StatusBadRequest, false, false},
	CodeScopeDenied:         {"Connector scope or purpose denied", http.StatusForbidden, false, false},
	CodeFileIntegrity:       {"File manifest, hash or integrity failure", http.StatusUnprocessableEntity, false, true},
	CodeCallbackConflict:    {"Callback replay or out-of-order conflict", http.StatusConflict, false, false},
	CodeBackpressure:        {"Backpressure or admission protection active", http.StatusServiceUnavailable, true, false},
}

// Codes returns every defined code, for contract tests and documentation.
func Codes() []Code {
	out := make([]Code, 0, len(infos))
	for c := range infos {
		out = append(out, c)
	}
	return out
}

// Valid reports whether c is a defined code.
func (c Code) Valid() bool { _, ok := infos[c]; return ok }

// Info returns the fixed meaning of c. An undefined code yields a 500 info so a
// typo can never masquerade as a client error.
func (c Code) Info() Info {
	if i, ok := infos[c]; ok {
		return i
	}
	return Info{Title: "Undefined integration error code", Status: http.StatusInternalServerError}
}

// ProblemTypePrefix prefixes the RFC 9457 "type" URI. A URN rather than an
// HTTP URL: it is a stable identifier, not a document location.
const ProblemTypePrefix = "urn:zoikosuite:problem:"

// ProblemContentType is the RFC 9457 media type.
const ProblemContentType = "application/problem+json"

// Error is an integration failure carrying a stable Code. It never embeds the
// offending payload: Detail is a short human sentence and must not contain
// secrets or sensitive bodies (INT-CTRL-035).
type Error struct {
	Code          Code
	Detail        string
	CorrelationID string
	// RetryAfterSeconds is surfaced as Retry-After for retryable codes. Zero
	// means "not advised".
	RetryAfterSeconds int
	Cause             error
}

// New builds an Error.
func New(code Code, detail string) *Error { return &Error{Code: code, Detail: detail} }

// Newf builds an Error with a formatted detail.
func Newf(code Code, format string, a ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// Wrap attaches an underlying cause. The cause is available to errors.Is/As
// and logs, but is never serialized into the problem body.
func Wrap(code Code, detail string, cause error) *Error {
	return &Error{Code: code, Detail: detail, Cause: cause}
}

// WithCorrelation returns e tagged with a correlation id.
func (e *Error) WithCorrelation(id string) *Error { e.CorrelationID = id; return e }

// WithRetryAfter sets the advised delay in seconds.
func (e *Error) WithRetryAfter(seconds int) *Error { e.RetryAfterSeconds = seconds; return e }

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is matches on Code, so errors.Is(err, integration.New(CodeRateLimited, ""))
// answers "is this a rate-limit failure" regardless of detail text.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// As extracts the *Error from err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Problem is the RFC 9457 body, with ZoikoSuite extension members.
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	Code          Code   `json:"code"`
	Retryable     bool   `json:"retryable"`
	Quarantine    bool   `json:"quarantine,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Problem renders e as problem details. instance is the request path or
// resource the problem occurred on and may be empty.
func (e *Error) Problem(instance string) Problem {
	i := e.Code.Info()
	return Problem{
		Type:          ProblemTypePrefix + string(e.Code),
		Title:         i.Title,
		Status:        i.Status,
		Detail:        e.Detail,
		Instance:      instance,
		Code:          e.Code,
		Retryable:     i.Retryable,
		Quarantine:    i.Quarantine,
		CorrelationID: e.CorrelationID,
	}
}

// WriteProblem writes err as application/problem+json. A non-integration error
// is reported as an opaque 500 with no detail: an unclassified internal error
// must not leak its text to an external caller.
func WriteProblem(w http.ResponseWriter, instance string, err error) {
	e, ok := As(err)
	if !ok {
		p := Problem{
			Type:   "about:blank",
			Title:  http.StatusText(http.StatusInternalServerError),
			Status: http.StatusInternalServerError,
		}
		writeProblemBody(w, p, 0)
		return
	}
	writeProblemBody(w, e.Problem(instance), e.RetryAfterSeconds)
}

func writeProblemBody(w http.ResponseWriter, p Problem, retryAfter int) {
	w.Header().Set("Content-Type", ProblemContentType)
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
