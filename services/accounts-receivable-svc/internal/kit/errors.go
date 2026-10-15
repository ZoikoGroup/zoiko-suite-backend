package kit

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Stable error classes (spec §14). Every AR module answers with these codes in
// the "code" field so callers and certification tests can assert on them.
const (
	CodeCustomerProfileInactive    = "CUSTOMER_PROFILE_INACTIVE"
	CodeCreditHoldActive           = "CREDIT_HOLD_ACTIVE"
	CodeQuoteExpired               = "QUOTE_EXPIRED"
	CodeOrderNotBillable           = "ORDER_NOT_BILLABLE"
	CodeTaxDeterminationMissing    = "TAX_DETERMINATION_MISSING"
	CodeInvoiceApprovalInvalidated = "INVOICE_APPROVAL_INVALIDATED"
	CodeInvoiceNumberCollision     = "INVOICE_NUMBER_COLLISION"
	CodeInvoiceAlreadyIssued       = "INVOICE_ALREADY_ISSUED"
	CodeEInvoiceValidationFailed   = "EINVOICE_VALIDATION_FAILED"
	CodeExternalStateUnknown       = "EXTERNAL_STATE_UNKNOWN"
	CodeCorrectionExceedsEligible  = "CORRECTION_EXCEEDS_ELIGIBLE_AMOUNT"
	CodeReceivableResidualInvalid  = "RECEIVABLE_RESIDUAL_INVALID"
	CodeSettlementNotConfirmed     = "SETTLEMENT_NOT_CONFIRMED"
	CodeCashApplicationDuplicate   = "CASH_APPLICATION_DUPLICATE"
	CodeApplicationOutOfBalance    = "APPLICATION_OUT_OF_BALANCE"
	CodeCollectionHoldActive       = "COLLECTION_HOLD_ACTIVE"
	CodeRefundDestinationChanged   = "REFUND_DESTINATION_CHANGED"
	CodeRefundStateUnknown         = "REFUND_STATE_UNKNOWN"
	CodeWriteOffApprovalRequired   = "WRITEOFF_APPROVAL_REQUIRED"
	CodeARGLReconciliationBreak    = "AR_GL_RECONCILIATION_BREAK"

	// Platform-wide classes, same spellings as accounts-payable-svc.
	CodeValidationFailed       = "VALIDATION_FAILED"
	CodeNotFound               = "NOT_FOUND"
	CodeStaleVersion           = "STALE_VERSION"
	CodeExpectedVersionMissing = "EXPECTED_VERSION_REQUIRED"
	CodeIdempotencyRequired    = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyReused      = "IDEMPOTENCY_KEY_REUSED"
	CodeInvalidTransition      = "INVALID_STATE_TRANSITION"
	CodeForbidden              = "FORBIDDEN"
	CodeSegregationOfDuties    = "SEGREGATION_OF_DUTIES"
	CodeTenantScope            = "TENANT_SCOPE_VIOLATION"
	CodeDependencyUnavailable  = "DEPENDENCY_UNAVAILABLE"
	CodeImmutable              = "DOCUMENT_IMMUTABLE"
	CodePeriodClosed           = "PERIOD_CLOSED"
	CodeReasonRequired         = "REASON_REQUIRED"
	CodeEvidenceRequired       = "EVIDENCE_REQUIRED"
)

// Problem is a domain error carrying its HTTP status and stable code. Store and
// domain layers return *Problem (or wrap one); handlers hand it to WriteErr.
type Problem struct {
	Status int
	Code   string
	Msg    string
}

func (p *Problem) Error() string { return p.Code + ": " + p.Msg }

// Err builds a Problem.
func Err(status int, code, msg string) *Problem {
	return &Problem{Status: status, Code: code, Msg: msg}
}

func Invalid(msg string) *Problem        { return Err(http.StatusBadRequest, CodeValidationFailed, msg) }
func NotFound(msg string) *Problem       { return Err(http.StatusNotFound, CodeNotFound, msg) }
func Conflict(code, msg string) *Problem { return Err(http.StatusConflict, code, msg) }
func Unprocessable(code, msg string) *Problem {
	return Err(http.StatusUnprocessableEntity, code, msg)
}
func Forbidden(code, msg string) *Problem { return Err(http.StatusForbidden, code, msg) }
func Unavailable(msg string) *Problem {
	return Err(http.StatusServiceUnavailable, CodeDependencyUnavailable, msg)
}

// ErrStaleVersion is returned when expected_version no longer matches.
var ErrStaleVersion = Err(http.StatusConflict, CodeStaleVersion, "the record changed since it was read; re-read and retry with the current version")

// ErrExpectedVersionRequired is returned when a stateful command omits it.
var ErrExpectedVersionRequired = Err(http.StatusBadRequest, CodeExpectedVersionMissing, "expected_version is required for this command")

type errorBody struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError writes the platform error shape: {"error": slug, "code": CLASS, "detail": ...}.
func WriteError(w http.ResponseWriter, status int, code, detail string) {
	WriteJSON(w, status, errorBody{Error: slug(code), Code: code, Detail: detail})
}

// WriteErr maps any error to a response: a *Problem keeps its status and code;
// anything else is an opaque 503 so a fault stays loud and no internals leak.
func WriteErr(w http.ResponseWriter, err error) {
	var p *Problem
	if errors.As(err, &p) {
		WriteError(w, p.Status, p.Code, p.Msg)
		return
	}
	WriteError(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "")
}

func slug(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
