package outbound

import (
	"errors"
	"time"
)

// Callback receipt states (§4). Only the final ones are stored: RECEIVED and AUTHENTICATED are the pipeline's own
// steps, and a receipt is evidence of how a callback ended.
const (
	ReceiptReceived      = "RECEIVED"
	ReceiptAuthenticated = "AUTHENTICATED"
	ReceiptDuplicate     = "DUPLICATE"
	ReceiptApplied       = "APPLIED"
	ReceiptRejected      = "REJECTED"
)

// Authentication results of a receipt.
const (
	AuthOK          = "OK"
	AuthFailed      = "FAILED"
	AuthReplay      = "REPLAY"
	AuthUnsupported = "UNSUPPORTED"
)

// Why a callback was rejected or not applied. Stable machine values; they never carry a payload value.
const (
	CBAuthFailed         = "AUTH_FAILED"
	CBReplayWindow       = "REPLAY_WINDOW"
	CBConnectionInactive = "CONNECTION_NOT_ACTIVE"
	CBSchemaVersion      = "SCHEMA_VERSION_UNSUPPORTED"
	CBSchemaViolation    = "SCHEMA_VIOLATION"
	CBMediaType          = "MEDIA_TYPE_UNSUPPORTED"
	CBEncoding           = "UNSUPPORTED_ENCODING"
	CBClaimMismatch      = "CLAIM_MISMATCH"
	CBDedupeMissing      = "DEDUPE_KEY_MISSING"
	CBBodyChanged        = "BODY_CHANGED_UNDER_SAME_KEY"
	CBStatusUnknown      = "STATUS_NOT_RECOGNISED"
	CBReferenceMissing   = "REFERENCE_MISSING"
	CBUncorrelated       = "ATTEMPT_NOT_FOUND"
	CBReferenceMismatch  = "REFERENCE_MISMATCH"
	CBTransition         = "TRANSITION_NOT_ALLOWED"
	CBLateContradicts    = "LATE_CONTRADICTS_FINAL"
	CBAlreadyRecorded    = "OUTCOME_ALREADY_RECORDED"
	CBDuplicateEvent     = "DUPLICATE_EVENT"
)

// Errors of the callback side.
var (
	ErrReceiptNotFound = errors.New("callback receipt not found")
)

// Receipt is the independent evidence of one provider callback (§9.3): what arrived (hashes and metadata, never the
// body), whether the sender was authenticated, what the callback claimed, and what happened to the attempt. A
// receipt is written once and never changes.
type Receipt struct {
	ReceiptID           string    `json:"receipt_id"`
	TenantID            string    `json:"tenant_id"`
	ConnectionID        string    `json:"connection_id"`
	ConnectorID         string    `json:"connector_id"`
	OperationID         string    `json:"operation_id"`
	ContractVersionID   string    `json:"contract_version_id"`
	AttemptID           string    `json:"attempt_id,omitempty"`
	State               string    `json:"state"`
	AuthResult          string    `json:"auth_result"`
	AuthMechanism       string    `json:"auth_mechanism,omitempty"`
	KeyLabel            string    `json:"key_label,omitempty"`
	ReasonCode          string    `json:"reason_code,omitempty"`
	XICCode             string    `json:"xic_code,omitempty"`
	DedupeKey           string    `json:"-"`
	OriginalReceiptID   string    `json:"original_receipt_id,omitempty"`
	BodyHash            string    `json:"body_hash"`
	BodySize            int       `json:"body_size"`
	ProviderReference   string    `json:"provider_reference,omitempty"`
	ReportedStatus      string    `json:"reported_status,omitempty"`
	TargetState         string    `json:"target_state,omitempty"`
	AttemptStateBefore  string    `json:"attempt_state_before,omitempty"`
	AttemptStateAfter   string    `json:"attempt_state_after,omitempty"`
	NeedsReconciliation bool      `json:"needs_reconciliation"`
	SourceNetHash       string    `json:"source_net_hash,omitempty"`
	CorrelationID       string    `json:"correlation_id,omitempty"`
	ReceivedAt          time.Time `json:"received_at"`
}

// ReceiptFilter narrows a list of receipts.
type ReceiptFilter struct {
	State               string
	ConnectionID        string
	AttemptID           string
	NeedsReconciliation bool
	Limit               int
}
