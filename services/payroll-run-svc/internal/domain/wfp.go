package domain

// ZS-SVC-L-001 (Workforce & Payroll Accounting Interface) shared contract.
//
// WFP is a financial boundary, not a payroll engine: nothing in this file
// calculates pay. Money in WFP objects is always a decimal STRING, never a
// float64 — the legacy payroll_runs/pay_slips types above are unchanged.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ── Source trust profiles (WFP-D-01) ─────────────────────────────────────────

const (
	SourceKindZoikoPayroll     = "ZOIKO_PAYROLL"
	SourceKindExternalPayroll  = "EXTERNAL_PAYROLL_PROVIDER"
	SourceKindZoikoHR          = "ZOIKO_HR"
	SourceKindZoikoTime        = "ZOIKO_TIME"
	SourceKindApprovedExternal = "APPROVED_EXTERNAL"
)

var validSourceKinds = map[string]bool{
	SourceKindZoikoPayroll: true, SourceKindExternalPayroll: true, SourceKindZoikoHR: true,
	SourceKindZoikoTime: true, SourceKindApprovedExternal: true,
}

// Capabilities a source may be trusted for. One class of fact per capability:
// trusting a source for worker profiles says nothing about payroll results.
const (
	CapWorkerProfile = "WORKER_PROFILE"
	CapTimeLeave     = "TIME_LEAVE"
	CapPayrollInput  = "PAYROLL_INPUT"
	CapPayrollResult = "PAYROLL_RESULT"
)

var validCapabilities = map[string]bool{
	CapWorkerProfile: true, CapTimeLeave: true, CapPayrollInput: true, CapPayrollResult: true,
}

const (
	TrustPendingCertification = "PENDING_CERTIFICATION"
	TrustActive               = "ACTIVE"
	TrustSuspended            = "SUSPENDED"
	TrustRetired              = "RETIRED"
)

type SourceTrustProfile struct {
	ProfileID           string     `json:"profile_id"`
	TenantID            string     `json:"tenant_id"`
	SourceSystemID      string     `json:"source_system_id"`
	SourceKind          string     `json:"source_kind"`
	Status              string     `json:"status"`
	AllowedCapabilities []string   `json:"allowed_capabilities"`
	CertificationRef    *string    `json:"certification_ref,omitempty"`
	CertifiedAt         *time.Time `json:"certified_at,omitempty"`
	Version             int        `json:"version"`
	CreatedBy           string     `json:"created_by"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// Allows reports whether the profile currently authorizes the capability.
// Only an ACTIVE profile ever does.
func (p *SourceTrustProfile) Allows(capability string) bool {
	if p == nil || p.Status != TrustActive {
		return false
	}
	for _, c := range p.AllowedCapabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// trustEdges is the only way a trust profile moves. RETIRED is terminal, and a
// profile can only become ACTIVE by naming certification evidence.
var trustEdges = map[string][]string{
	TrustPendingCertification: {TrustActive, TrustRetired},
	TrustActive:               {TrustSuspended, TrustRetired},
	TrustSuspended:            {TrustActive, TrustRetired},
}

func TrustTransitionAllowed(from, to string) bool {
	for _, t := range trustEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

type CreateSourceTrustProfileRequest struct {
	SourceSystemID      string   `json:"source_system_id"`
	SourceKind          string   `json:"source_kind"`
	AllowedCapabilities []string `json:"allowed_capabilities"`
}

// SourceTrustTransitionRequest drives activate / suspend / retire. The
// expected_version is mandatory: a stale caller cannot silently overwrite a
// newer profile state.
type SourceTrustTransitionRequest struct {
	ExpectedVersion  int    `json:"expected_version"`
	CertificationRef string `json:"certification_ref,omitempty"` // required to activate
	Reason           string `json:"reason,omitempty"`
}

func (r CreateSourceTrustProfileRequest) Validate() error {
	if strings.TrimSpace(r.SourceSystemID) == "" {
		return fmt.Errorf("source_system_id is required")
	}
	if !validSourceKinds[r.SourceKind] {
		return fmt.Errorf("source_kind %q is not one of ZOIKO_PAYROLL, EXTERNAL_PAYROLL_PROVIDER, ZOIKO_HR, ZOIKO_TIME, APPROVED_EXTERNAL", r.SourceKind)
	}
	if len(r.AllowedCapabilities) == 0 {
		return fmt.Errorf("allowed_capabilities must not be empty")
	}
	seen := map[string]bool{}
	for _, c := range r.AllowedCapabilities {
		if !validCapabilities[c] {
			return fmt.Errorf("allowed_capabilities contains unknown capability %q", c)
		}
		if seen[c] {
			return fmt.Errorf("allowed_capabilities repeats %q", c)
		}
		seen[c] = true
	}
	return nil
}

// ── Rule manifests ───────────────────────────────────────────────────────────

type PayrollRuleManifest struct {
	ManifestID      string    `json:"manifest_id"`
	TenantID        string    `json:"tenant_id"`
	SourceSystemID  string    `json:"source_system_id"`
	JurisdictionID  string    `json:"jurisdiction_id"`
	ManifestVersion string    `json:"manifest_version"`
	ManifestDigest  string    `json:"manifest_digest"`
	EffectiveFrom   string    `json:"effective_from"` // YYYY-MM-DD
	EffectiveTo     *string   `json:"effective_to,omitempty"`
	EvidenceRef     string    `json:"evidence_ref"`
	RegisteredBy    string    `json:"registered_by"`
	CreatedAt       time.Time `json:"created_at"`
}

type RegisterRuleManifestRequest struct {
	SourceSystemID  string  `json:"source_system_id"`
	JurisdictionID  string  `json:"jurisdiction_id"`
	ManifestVersion string  `json:"manifest_version"`
	ManifestDigest  string  `json:"manifest_digest"`
	EffectiveFrom   string  `json:"effective_from"`
	EffectiveTo     *string `json:"effective_to,omitempty"`
	EvidenceRef     string  `json:"evidence_ref"`
}

func (r RegisterRuleManifestRequest) Validate() error {
	for name, v := range map[string]string{
		"source_system_id": r.SourceSystemID, "jurisdiction_id": r.JurisdictionID,
		"manifest_version": r.ManifestVersion, "evidence_ref": r.EvidenceRef,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(r.ManifestDigest) != 64 || strings.Trim(r.ManifestDigest, "0123456789abcdef") != "" {
		return fmt.Errorf("manifest_digest must be a lowercase hex SHA-256 (64 characters)")
	}
	from, err := time.Parse("2006-01-02", r.EffectiveFrom)
	if err != nil {
		return fmt.Errorf("effective_from must be YYYY-MM-DD")
	}
	if r.EffectiveTo != nil {
		to, err := time.Parse("2006-01-02", *r.EffectiveTo)
		if err != nil {
			return fmt.Errorf("effective_to must be YYYY-MM-DD")
		}
		if !to.After(from) {
			return fmt.Errorf("effective_to must be after effective_from")
		}
	}
	return nil
}

// ── Orthogonal payroll financial states (spec §2.3) ──────────────────────────
// Seven dimensions that MUST NOT be collapsed into one "payroll status".

const (
	AcceptanceReceived    = "RECEIVED"
	AcceptanceValidating  = "VALIDATING"
	AcceptanceAccepted    = "ACCEPTED"
	AcceptanceRejected    = "REJECTED"
	AcceptanceQuarantined = "QUARANTINED"
	AcceptanceSuperseded  = "SUPERSEDED"
)

// acceptanceEdges is the only way an acceptance state moves. ACCEPTED is
// immutable apart from being superseded; REJECTED and SUPERSEDED are terminal.
var acceptanceEdges = map[string][]string{
	AcceptanceReceived:    {AcceptanceValidating, AcceptanceRejected, AcceptanceQuarantined},
	AcceptanceValidating:  {AcceptanceAccepted, AcceptanceRejected, AcceptanceQuarantined},
	AcceptanceQuarantined: {AcceptanceValidating, AcceptanceRejected},
	AcceptanceAccepted:    {AcceptanceSuperseded},
}

func AcceptanceTransitionAllowed(from, to string) bool {
	for _, t := range acceptanceEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// StateDimensions lists the canonical values of the other six dimensions.
// Waves 3-8 transition within them; defining them once here keeps every
// WFP module on the same vocabulary.
var StateDimensions = map[string][]string{
	"input":               {"DRAFT", "VALIDATED", "FROZEN", "SUPERSEDED"},
	"accounting":          {"NOT_REQUIRED", "PENDING", "EVENT_CREATED", "POSTED", "REJECTED", "CORRECTED"},
	"liability":           {"RECOGNIZED", "DUE", "SCHEDULED", "PAID", "RECONCILED", "DISPUTED", "ADJUSTED"},
	"payment_instruction": {"DRAFT", "VALIDATED", "APPROVED", "SUBMITTED_TO_BANKING", "CANCELLED", "SUPERSEDED"},
	"bank_settlement":     {"NOT_SUBMITTED", "PENDING_UNKNOWN", "ACCEPTED", "SETTLED", "REJECTED", "RETURNED", "REVERSED"},
	"exception":           {"NONE", "REVIEW_REQUIRED", "BLOCKED", "INVESTIGATION", "RESOLVED"},
}

// ── Payroll run reference ────────────────────────────────────────────────────

const (
	RunKindRegular    = "REGULAR"
	RunKindOffCycle   = "OFF_CYCLE"
	RunKindRetro      = "RETRO"
	RunKindCorrection = "CORRECTION"
)

var validRunKinds = map[string]bool{
	RunKindRegular: true, RunKindOffCycle: true, RunKindRetro: true, RunKindCorrection: true,
}

type PayrollRunRef struct {
	RunRefID            string    `json:"run_ref_id"`
	TenantID            string    `json:"tenant_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	SourceSystemID      string    `json:"source_system_id"`
	SourceRunID         string    `json:"source_run_id"`
	SourceRunVersion    int       `json:"source_run_version"`
	RunKind             string    `json:"run_kind"`
	PayrollPeriodID     string    `json:"payroll_period_id"`
	PayDate             string    `json:"pay_date"`
	RuleManifestID      string    `json:"rule_manifest_id"`
	PayrollRunID        *string   `json:"payroll_run_id,omitempty"`
	AcceptanceState     string    `json:"acceptance_state"`
	SupersedesRunRefID  *string   `json:"supersedes_run_ref_id,omitempty"`
	IdentityFingerprint string    `json:"identity_fingerprint"`
	ResultFingerprint   *string   `json:"result_fingerprint,omitempty"`
	CorrelationID       string    `json:"correlation_id"`
	CausationID         *string   `json:"causation_id,omitempty"`
	Version             int       `json:"version"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// RegisterPayrollRunRefRequest deliberately has NO tenant_id: the tenant is the
// authenticated one, never a body field (WFP-NP-001).
type RegisterPayrollRunRefRequest struct {
	LegalEntityID    string  `json:"legal_entity_id"`
	SourceSystemID   string  `json:"source_system_id"`
	SourceRunID      string  `json:"source_run_id"`
	SourceRunVersion int     `json:"source_run_version"`
	RunKind          string  `json:"run_kind"`
	PayrollPeriodID  string  `json:"payroll_period_id"`
	PayDate          string  `json:"pay_date"`
	RuleManifestID   string  `json:"rule_manifest_id"`
	PayrollRunID     *string `json:"payroll_run_id,omitempty"`
	CausationID      *string `json:"causation_id,omitempty"`
}

func (r RegisterPayrollRunRefRequest) Validate() error {
	for name, v := range map[string]string{
		"legal_entity_id": r.LegalEntityID, "source_system_id": r.SourceSystemID,
		"source_run_id": r.SourceRunID, "payroll_period_id": r.PayrollPeriodID,
		"rule_manifest_id": r.RuleManifestID,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if r.SourceRunVersion < 1 {
		return fmt.Errorf("source_run_version must be >= 1")
	}
	if !validRunKinds[r.RunKind] {
		return fmt.Errorf("run_kind %q is not one of REGULAR, OFF_CYCLE, RETRO, CORRECTION", r.RunKind)
	}
	if _, err := time.Parse("2006-01-02", r.PayDate); err != nil {
		return fmt.Errorf("pay_date must be YYYY-MM-DD")
	}
	return nil
}

// RunIdentityFingerprint is the canonical identity of a payroll run version
// (spec §2.2), before a result exists: tenant + legal entity + source system +
// source run + source version + period + pay date + rule manifest.
//
// Every field is length-prefixed, so ("ab","c") and ("a","bc") cannot collide.
// The result fingerprint joins the identity at WFP-04 acceptance and is stored
// separately, set once.
func RunIdentityFingerprint(tenantID string, r RegisterPayrollRunRefRequest) string {
	h := sha256.New()
	for _, f := range []string{
		tenantID, r.LegalEntityID, r.SourceSystemID, r.SourceRunID,
		strconv.Itoa(r.SourceRunVersion), r.PayrollPeriodID, r.PayDate, r.RuleManifestID,
	} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EffectiveOn reports whether the manifest covers the given date.
// effective_to is exclusive. A manifest not yet effective on the pay date is
// refused just as an expired one is: "use current rules" is never a fallback
// for a historical run (WFP-NP-037/038).
func (m *PayrollRuleManifest) EffectiveOn(date string) bool {
	if date < m.EffectiveFrom {
		return false
	}
	return m.EffectiveTo == nil || date < *m.EffectiveTo
}

// ── Canonical events (spec §3.2) ─────────────────────────────────────────────

const (
	EventWorkerFinancialProfileSynced    = "worker_financial_profile_synced"
	EventTimeLeaveBatchAccepted          = "time_leave_financial_batch_accepted"
	EventPayrollInputBatchFrozen         = "payroll_input_batch_frozen"
	EventPayrollResultReceived           = "payroll_result_received"
	EventPayrollResultAccepted           = "payroll_result_accepted"
	EventPayrollResultRejected           = "payroll_result_rejected"
	EventPayrollResultSuperseded         = "payroll_result_superseded"
	EventPayrollAccountingEventCreated   = "payroll_accounting_event_created"
	EventPayrollAccountingRejected       = "payroll_accounting_rejected"
	EventPayrollLiabilityRecognized      = "payroll_liability_recognized"
	EventPayrollLiabilitySettled         = "payroll_liability_settled"
	EventPayrollPaymentInstrApproved     = "payroll_payment_instruction_approved"
	EventPayrollPaymentInstrInvalidated  = "payroll_payment_instruction_invalidated"
	EventPayrollPaymentSubmitted         = "payroll_payment_submitted_to_banking"
	EventPayrollReconExceptionCreated    = "payroll_reconciliation_exception_created"
	EventPayrollEvidencePackageCertified = "payroll_evidence_package_certified"

	// Foundation events for the objects this wave introduces. They are not in
	// the spec's §3.2 list; they exist because a trust or rule-manifest change
	// is itself auditable evidence.
	EventSourceTrustProfileChanged = "wfp_source_trust_profile_changed"
	EventRuleManifestRegistered    = "wfp_rule_manifest_registered"

	// WFP-03 payroll input (spec 3.2 names payroll_input_batch_frozen only; the
	// WFP-03 contract lists the rest).
	EventInputCreated           = "payroll_input_created"
	EventInputBatchValidated    = "payroll_input_batch_validated"
	EventInputSuperseded        = "payroll_input_superseded"
	EventInputLineQuarantined   = "payroll_input_line_quarantined"
	EventInputBatchRejected     = "payroll_input_batch_rejected"
	EventInputBatchCancelled    = "payroll_input_batch_cancelled"
	EventInputBatchConsumed     = "payroll_input_batch_consumed"
	EventInputMappingRegistered = "payroll_input_code_mapping_registered"
	EventManualInputProposed    = "payroll_manual_input_proposed"
	EventManualInputApproved    = "payroll_manual_input_approved"
	EventManualInputRejected    = "payroll_manual_input_rejected"
	EventManualInputWithdrawn   = "payroll_manual_input_withdrawn"

	// Legacy payroll run lifecycle (delivered through the outbox since Wave 4).
	EventRunInitiated  = "payroll.run.initiated"
	EventRunCalculated = "payroll.run.calculated"
	EventRunCompleted  = "payroll.run.completed"
	EventRunBlocked    = "payroll.run.blocked"
)

// EventCatalogue is every WFP event type an outbox row may carry.
var EventCatalogue = map[string]bool{
	EventWorkerFinancialProfileSynced: true, EventTimeLeaveBatchAccepted: true,
	EventPayrollInputBatchFrozen: true, EventPayrollResultReceived: true,
	EventPayrollResultAccepted: true, EventPayrollResultRejected: true,
	EventPayrollResultSuperseded: true, EventPayrollAccountingEventCreated: true,
	EventPayrollAccountingRejected: true, EventPayrollLiabilityRecognized: true,
	EventPayrollLiabilitySettled: true, EventPayrollPaymentInstrApproved: true,
	EventPayrollPaymentInstrInvalidated: true, EventPayrollPaymentSubmitted: true,
	EventPayrollReconExceptionCreated: true, EventPayrollEvidencePackageCertified: true,
	EventSourceTrustProfileChanged: true, EventRuleManifestRegistered: true,
	EventInputCreated: true, EventInputBatchValidated: true, EventInputSuperseded: true,
	EventPayrollAccountingSubmitted: true, EventPayrollAccountingPosted: true, EventPayrollAccountingCorrected: true,
	EventInputLineQuarantined: true, EventInputBatchRejected: true, EventInputBatchCancelled: true,
	EventInputBatchConsumed: true, EventInputMappingRegistered: true,
	EventManualInputProposed: true, EventManualInputApproved: true, EventManualInputRejected: true, EventManualInputWithdrawn: true,
	EventPayrollPaymentInstrCreated: true, EventPayrollPaymentInstrCancelled: true, EventPayrollPaymentInstrSuperseded: true,
	EventPayrollLiabilityDue: true, EventPayrollLiabilityScheduled: true, EventPayrollLiabilityAdjusted: true,
	EventPayrollLiabilityDisputed: true, EventPayrollLiabilityReconciliationFailed: true,
	EventRunInitiated: true, EventRunCalculated: true, EventRunCompleted: true, EventRunBlocked: true,
	EventPayrollReconRunCompleted: true, EventPayrollReconRunCertified: true, EventPayrollReconExceptionResolved: true,
	EventPayrollReconExceptionWaived: true, EventPayrollBankingOutcomeObserved: true, EventPayrollPaymentInstrReturned: true,
}

// OutboxEvent is one event to be written in the caller's transaction.
type OutboxEvent struct {
	TenantID  string
	ObjectID  string
	EventType string
	Payload   []byte // the fully built envelope
}

// ── Errors ───────────────────────────────────────────────────────────────────

var (
	ErrVersionConflict        = errorString("stale expected_version: the object changed since it was read")
	ErrTrustProfileNotFound   = errorString("source trust profile not found")
	ErrTrustProfileConflict   = errorString("a trust profile for this source system already exists with different content")
	ErrSourceNotTrusted       = errorString("source system has no ACTIVE trust profile covering this capability")
	ErrCertificationRequired  = errorString("certification_ref is required to activate a source trust profile")
	ErrInvalidTrustTransition = errorString("trust profile transition is not allowed from its current status")
	ErrManifestNotFound       = errorString("payroll rule manifest not found")
	ErrManifestConflict       = errorString("a rule manifest with this identity already exists")
	ErrManifestNotEffective   = errorString("rule manifest is not effective on the pay date")
	ErrManifestSourceMismatch = errorString("rule manifest belongs to a different source system")
	ErrRunRefNotFound         = errorString("payroll run reference not found")
	ErrRunIdentityConflict    = errorString("this source run version already exists with different identity; a changed run must use a new source_run_version")
	ErrPriorVersionMissing    = errorString("source_run_version > 1 requires the previous version to be registered")
	ErrInvalidAcceptanceMove  = errorString("acceptance state transition is not allowed")
)
