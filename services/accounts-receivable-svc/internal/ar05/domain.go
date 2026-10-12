// Package ar05 implements AR-05 Electronic Invoice (spec ZS-SVC-C-001 section 8): render, validate and transmit an
// ISSUED document through a jurisdiction/network e-invoice profile while the source document stays authoritative.
//
// It owns the submission, the rendered artifact, validation results, submission attempts (each with a durable
// external_attempt_id), provider acknowledgements and the reconciliation history. It never edits the document: a
// rejected e-invoice moves ONLY the e-invoice dimension of the invoice (kit.Invoices.SetEInvoiceState).
//
// Honest boundary: there is no e-invoice network transport in this platform. Everything up to the wire (profile
// resolution, rendering, validation, attempt ledger, state machine, callback security, reconciliation) is implemented
// here; the wire itself is the Transport interface, whose default is UnconfiguredTransports (a typed error, never a
// fake success).
package ar05

import (
	"fmt"
	"strings"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture). Spec permissions
// einvoice.submit/retry/reconcile map to AR05_EINVOICE_SUBMIT / AR05_EINVOICE_RECONCILE; adapter.admin (restricted)
// to AR05_ADAPTER_ADMIN, which gates the controlled admin evidence path of ReconcileSubmissionState.
const (
	ActionSubmit       = "AR05_EINVOICE_SUBMIT"    // prepare, validate, submit, retry, cancel
	ActionReconcile    = "AR05_EINVOICE_RECONCILE" // reconcile by querying the provider
	ActionAdapterAdmin = "AR05_ADAPTER_ADMIN"      // manual resolution WITH evidence (also needs reason_code + evidence_id)
)

// Codes local to AR-05 beyond the shared spec section 14 classes.
const (
	CodeProviderNotConfigured   = "EINVOICE_PROVIDER_NOT_CONFIGURED"
	CodeCallbackUnauthenticated = "CALLBACK_UNAUTHENTICATED"
	CodeCallbackStale           = "CALLBACK_STALE"
	CodeCallbackReplayed        = "CALLBACK_REPLAYED"
	CodeCallbackMismatch        = "CALLBACK_MISMATCH"
)

const (
	AggregateType = "EInvoiceSubmission"
	maxListLimit  = 200
	maxText       = 2000
	maxAttempts   = 20
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventPrepared      = "ar.einvoice.prepared"
	EventSubmitted     = "ar.einvoice.submitted"
	EventAccepted      = "ar.einvoice.accepted"
	EventRejected      = "ar.einvoice.rejected"
	EventStateUnknown  = "ar.einvoice.state_unknown"
	EventReconciled    = "ar.einvoice.submission_reconciled"
	SchemaJSONArtifact = "zoiko.ar05.einvoice-json/1"
)

// State is the submission state. NOT_REQUIRED / REQUIRED exist only on the invoice's e-invoice dimension (no
// submission yet); a submission starts at PREPARED.
type State string

const (
	StatePrepared   State = "PREPARED"
	StateValidated  State = "VALIDATED"
	StateSubmitted  State = "SUBMITTED"
	StateUnknown    State = "PENDING_UNKNOWN"
	StateAccepted   State = "ACCEPTED"
	StateRejected   State = "REJECTED"
	StateCancelled  State = "CANCELLED"
	dimNotRequired        = "NOT_REQUIRED"
	dimRequired           = "REQUIRED"
	dimPrepared           = "PREPARED"
	dimSubmitted          = "SUBMITTED"
	dimPendingUnkwn       = "PENDING_UNKNOWN"
	dimAccepted           = "ACCEPTED"
	dimRejected           = "REJECTED"
)

// Terminal: the submission is finished; a late or conflicting provider report cannot change it.
func (s State) Terminal() bool {
	return s == StateAccepted || s == StateRejected || s == StateCancelled
}

// Dimension is the value mirrored onto the invoice's e-invoice dimension. VALIDATED is internal to AR-05 (the dimension
// has no such value) and a cancelled submission leaves the document still needing one.
func (s State) Dimension(stillRequired bool) string {
	switch s {
	case StatePrepared, StateValidated:
		return dimPrepared
	case StateSubmitted:
		return dimSubmitted
	case StateUnknown:
		return dimPendingUnkwn
	case StateAccepted:
		return dimAccepted
	case StateRejected:
		return dimRejected
	default: // CANCELLED
		if stillRequired {
			return dimRequired
		}
		return dimNotRequired
	}
}

// Transition names a state move.
type Transition string

const (
	TrValidate     Transition = "VALIDATE"      // PREPARED -> VALIDATED
	TrBeginAttempt Transition = "BEGIN_ATTEMPT" // VALIDATED -> PENDING_UNKNOWN (attempt recorded BEFORE the call: crash-safe)
	TrRetryAttempt Transition = "RETRY_ATTEMPT" // PENDING_UNKNOWN -> PENDING_UNKNOWN (only after reconciliation proved non-receipt)
	TrAck          Transition = "ACK"           // PENDING_UNKNOWN -> SUBMITTED (provider acknowledged receipt)
	TrNotSent      Transition = "NOT_SENT"      // PENDING_UNKNOWN -> VALIDATED (the transport guarantees nothing left)
	TrAccept       Transition = "ACCEPT"        // SUBMITTED | PENDING_UNKNOWN -> ACCEPTED
	TrReject       Transition = "REJECT"        // SUBMITTED | PENDING_UNKNOWN -> REJECTED
	TrReceived     Transition = "RECEIVED"      // PENDING_UNKNOWN -> SUBMITTED (provider confirms it holds the document)
	TrCancel       Transition = "CANCEL"        // PREPARED | VALIDATED -> CANCELLED (never transmitted)
)

var transitions = map[Transition]map[State]State{
	TrValidate:     {StatePrepared: StateValidated},
	TrBeginAttempt: {StateValidated: StateUnknown},
	TrRetryAttempt: {StateUnknown: StateUnknown},
	TrAck:          {StateUnknown: StateSubmitted},
	TrNotSent:      {StateUnknown: StateValidated},
	TrAccept:       {StateSubmitted: StateAccepted, StateUnknown: StateAccepted},
	TrReject:       {StateSubmitted: StateRejected, StateUnknown: StateRejected},
	TrReceived:     {StateUnknown: StateSubmitted},
	TrCancel:       {StatePrepared: StateCancelled, StateValidated: StateCancelled},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION.
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// Document types an e-invoice can carry (the jurisdiction registry's vocabulary).
const (
	DocInvoice    = kit.DocInvoice
	DocCreditNote = kit.DocCreditNote
	DocDebitNote  = kit.DocDebitNote
)

// Attempt outcomes (ar05_attempt_results.outcome); the newest result is the attempt's current outcome.
const (
	OutAcknowledged = "ACKNOWLEDGED"
	OutAccepted     = "ACCEPTED"
	OutRejected     = "REJECTED"
	OutAmbiguous    = "AMBIGUOUS"
	OutNotSent      = "NOT_SENT"
	OutNotReceived  = "NOT_RECEIVED"
	OutReceived     = "RECEIVED"
	OutAdminAccept  = "ADMIN_ACCEPTED"
	OutAdminReject  = "ADMIN_REJECTED"
	OutAdminNotRecv = "ADMIN_NOT_RECEIVED"
	OutSuperseded   = "SUPERSEDED"
	OutUnresolved   = "UNRESOLVED"
	OutInFlight     = "IN_FLIGHT" // derived: an attempt with no result yet
)

// Result sources.
const (
	SrcTransport      = "TRANSPORT"
	SrcCallback       = "CALLBACK"
	SrcReconciliation = "RECONCILIATION"
	SrcAdmin          = "ADMIN"
	SrcSystem         = "SYSTEM"
)

// Reconciliation methods.
const (
	MethodQuery = "QUERY_PROVIDER"
	MethodAdmin = "ADMIN_EVIDENCE"
)

// ── errors ───────────────────────────────────────────────────────────────────

// validationFailed is EINVOICE_VALIDATION_FAILED (422): wrong jurisdiction/network/profile, unsupported syntax, a
// document that no longer matches the bound content hash, or a structural failure.
func validationFailed(format string, a ...any) *kit.Problem {
	return kit.Unprocessable(kit.CodeEInvoiceValidationFailed, fmt.Sprintf(format, a...))
}

// stateUnknown is EXTERNAL_STATE_UNKNOWN (409): the provider-side state is ambiguous; nothing may be re-sent.
func stateUnknown(format string, a ...any) *kit.Problem {
	return kit.Conflict(kit.CodeExternalStateUnknown, fmt.Sprintf(format, a...))
}

func evidenceRequired() *kit.Problem {
	return kit.Err(400, kit.CodeEvidenceRequired, "evidence_id is required")
}

// ── profile (what jurisdiction-rules-svc says about the e-invoice channel) ────

// Dependency is a pinned schema / code list / business-rule package of a profile.
type Dependency struct {
	Kind      string  `json:"kind"`
	Ref       string  `json:"ref"`
	Version   string  `json:"version"`
	ValidFrom string  `json:"valid_from"`
	ValidTo   *string `json:"valid_to"`
}

// ValidOn reports whether the dependency is in force on day.
func (d Dependency) ValidOn(day time.Time) bool {
	from, err := time.Parse("2006-01-02", d.ValidFrom)
	if err != nil || day.Before(from) {
		return false
	}
	if d.ValidTo != nil {
		to, err := time.Parse("2006-01-02", *d.ValidTo)
		if err != nil || day.After(to) {
			return false
		}
	}
	return true
}

// MappingEntry maps a canonical field (e.g. "invoice.number") to the target syntax element (e.g. "cbc:ID").
type MappingEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Profile is the resolved e-invoice profile. Everything in it comes from the jurisdiction pack; nothing is defaulted.
type Profile struct {
	Jurisdiction    string         `json:"jurisdiction"` // as requested (the entity's jurisdiction)
	ProfileCode     string         `json:"profile_code"`
	Network         string         `json:"network"` // the profile's channel
	DocumentType    string         `json:"document_type"`
	ProfileVersion  string         `json:"profile_version"`
	Syntax          string         `json:"syntax"`
	Mode            string         `json:"transmission_mode"`
	Corrections     []string       `json:"corrections"`
	Dependencies    []Dependency   `json:"dependencies"`
	SemanticMapping []MappingEntry `json:"semantic_mapping"`
	LifecycleStates []string       `json:"lifecycle_states"`
	EffectiveAt     time.Time      `json:"effective_at"` // the instant the profile was resolved for (fixes which dependency versions are in force)

	// Provenance of the decision (pack release + rule), retained as evidence.
	RuleJurisdiction string `json:"rule_jurisdiction,omitempty"`
	PackRef          string `json:"pack_ref,omitempty"`
	PackVersion      string `json:"pack_version,omitempty"`
	PackVersionID    string `json:"pack_version_id,omitempty"`
	PackDigest       string `json:"pack_artifact_digest,omitempty"`
	RuleID           string `json:"rule_id,omitempty"`
	RuleDigest       string `json:"rule_content_digest,omitempty"`
	DecisionID       string `json:"decision_id,omitempty"`
}

// HasLifecycleState reports whether the registry lifecycle of the profile defines s.
func (p *Profile) HasLifecycleState(s string) bool {
	for _, x := range p.LifecycleStates {
		if x == s {
			return true
		}
	}
	return false
}

// DependenciesOn returns the schema/code-list/rule versions in force on day (the versions the artifact is validated against).
func (p *Profile) DependenciesOn(day time.Time) []Dependency {
	var out []Dependency
	for _, d := range p.Dependencies {
		if d.ValidOn(day) {
			out = append(out, d)
		}
	}
	return out
}

// MatchRequest checks a resolved profile against what the command asked for. Any mismatch is a wrong-profile refusal
// (spec negative path: wrong jurisdiction syntax/profile selected): EINVOICE_VALIDATION_FAILED, nothing recorded.
func (p *Profile) MatchRequest(jurisdiction, docType, profileCode, network string) error {
	switch {
	case !strings.EqualFold(p.Jurisdiction, jurisdiction):
		return validationFailed("the profile was resolved for jurisdiction %q, not %q", p.Jurisdiction, jurisdiction)
	case p.DocumentType != docType:
		return validationFailed("profile %s is for %s documents, not %s", p.ProfileCode, p.DocumentType, docType)
	case profileCode != "" && p.ProfileCode != profileCode:
		return validationFailed("the resolved profile is %s, not %s", p.ProfileCode, profileCode)
	case network != "" && !strings.EqualFold(p.Network, network):
		return validationFailed("profile %s belongs to network %s, not %s", p.ProfileCode, p.Network, network)
	case p.Syntax == "" || p.Mode == "" || p.Network == "" || p.ProfileVersion == "":
		return validationFailed("profile %s is incomplete (syntax, mode, network and version are required)", p.ProfileCode)
	}
	return nil
}

// ── source documents ─────────────────────────────────────────────────────────

// SourceLine is one frozen line of the source document.
type SourceLine struct {
	LineNumber  int
	Description string
	Quantity    kit.Amount
	UnitPrice   kit.Amount
	Net         kit.Amount
	Tax         kit.Amount
	TaxCode     string
}

// SourceDocument is the immutable issued representation an e-invoice is built from.
type SourceDocument struct {
	DocumentType       string
	DocumentID         string
	LegalEntityID      string
	CustomerPartyID    string
	Number             string
	Currency           string
	Net, Tax, Gross    kit.Amount
	IssueDate, DueDate time.Time
	TaxDeterminationID string
	TaxRulePackVersion string
	ContentHash        string
	Lines              []SourceLine
}

// SourceFromIssuedInvoice adapts the kit view of an issued invoice.
func SourceFromIssuedInvoice(in *kit.IssuedInvoice) *SourceDocument {
	d := &SourceDocument{DocumentType: DocInvoice, DocumentID: in.InvoiceID, LegalEntityID: in.LegalEntityID, CustomerPartyID: in.CustomerPartyID,
		Number: in.InvoiceNumber, Currency: in.Currency, Net: in.Net, Tax: in.Tax, Gross: in.Gross, IssueDate: in.IssueDate, DueDate: in.DueDate,
		TaxDeterminationID: in.TaxDeterminationID, TaxRulePackVersion: in.TaxRulePackVersion, ContentHash: in.ContentHash}
	for _, l := range in.Lines {
		d.Lines = append(d.Lines, SourceLine{LineNumber: l.LineNumber, Description: l.Description, Quantity: l.Quantity, UnitPrice: l.UnitPrice,
			Net: l.Net, Tax: l.Tax, TaxCode: l.TaxCode})
	}
	return d
}

// ── pure input validation ────────────────────────────────────────────────────

func trimmed(s string) string { return strings.TrimSpace(s) }

func tooLong(s string, n int) bool { return len(s) > n }
