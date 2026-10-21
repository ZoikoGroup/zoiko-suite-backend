package domain

// ZS-SVC-L-001 Wave 8 — payroll reconciliation (spec section 12), Banking outcome
// evidence, the close gate (WFP-CTRL-030) and the correction chain's completeness
// (WFP-CTRL-024).
//
// Everything here is pure: it compares populations the store has read, with exact
// rationals, and never decides an amount. A comparison that does not match becomes an
// exception that stays open until its cause clears (or an evidenced waiver by someone
// other than the principal who raised it).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// ── exception codes ──────────────────────────────────────────────────────────

const (
	ReconResultNotPosted       = "RESULT_NOT_POSTED"
	ReconGLMismatch            = "RESULT_GL_MISMATCH"
	ReconLiabilityMismatch     = "RESULT_LIABILITY_MISMATCH"
	ReconNetPayNotInstructed   = "NET_PAY_NOT_INSTRUCTED"
	ReconNetPayAmountMismatch  = "NET_PAY_AMOUNT_MISMATCH"
	ReconPaymentReturned       = "PAYMENT_RETURNED"
	ReconPaymentFailed         = "PAYMENT_FAILED"
	ReconOutcomeUnresolved     = "PAYMENT_OUTCOME_UNRESOLVED"
	ReconPaymentInFlight       = "PAYMENT_IN_FLIGHT"
	ReconNoBankingObservation  = "NO_BANKING_OBSERVATION"
	ReconSettlementUnbound     = "SETTLEMENT_NOT_BANK_BOUND"
	SeverityBlocking           = "BLOCKING"
	SeverityWarning            = "WARNING"
	ReconStateOpen             = "OPEN"
	ReconStateAssigned         = "ASSIGNED"
	ReconStateResolved         = "RESOLVED"
	ReconStateWaived           = "WAIVED"
	ReconOutcomeMatched        = "MATCHED"
	ReconOutcomeBreak          = "BREAK"
	ResolutionCauseCleared     = "CAUSE_CLEARED"
	ResolutionResultSuperseded = "RESULT_SUPERSEDED"
)

// Control ids (spec section 16) each assertion evidences.
const (
	CtrlPayrollToGL       = "WFP-CTRL-013"
	CtrlLiabilityToGL     = "WFP-CTRL-023"
	CtrlLiabilityComplete = "WFP-CTRL-014"
	CtrlNetPayToSettle    = "WFP-CTRL-022"
	CtrlPendingSuppress   = "WFP-CTRL-021"
	CtrlCloseGate         = "WFP-CTRL-030"
	CtrlCorrectionChain   = "WFP-CTRL-024"
)

// ── Banking outcome vocabulary ───────────────────────────────────────────────

const (
	OutcomeInFlight   = "IN_FLIGHT"
	OutcomeUnresolved = "UNRESOLVED"
	OutcomeSettled    = "SETTLED"
	OutcomeReturned   = "RETURNED"
	OutcomeFailed     = "FAILED"

	ObsSourceAttempt                   = "ADAPTER_ATTEMPT"
	ObsSourcePayment                   = "PAYMENT_STATUS"
	ObsSourceHandoff                   = "HANDOFF"
	PayReturned                        = "RETURNED"
	EventPayrollReconRunCompleted      = "payroll_reconciliation_run_completed"
	EventPayrollReconRunCertified      = "payroll_reconciliation_run_certified"
	EventPayrollReconExceptionResolved = "payroll_reconciliation_exception_resolved"
	EventPayrollReconExceptionWaived   = "payroll_reconciliation_exception_waived"
	EventPayrollBankingOutcomeObserved = "payroll_banking_outcome_observed"
	EventPayrollPaymentInstrReturned   = "payroll_payment_instruction_returned"
)

// OutcomeOfAttempt classifies the adapter's attempt status (BNK-06). Anything the
// adapter reports that is not known is UNRESOLVED, never assumed fine.
func OutcomeOfAttempt(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "PREPARED", "SUBMITTED":
		return OutcomeInFlight
	case "REJECTED_BEFORE_SUBMISSION", "CANCELLED":
		return OutcomeFailed
	default: // PENDING_UNKNOWN, QUARANTINED, anything new
		return OutcomeUnresolved
	}
}

// OutcomeOfPayment classifies payment-status-svc's execution status (BNK-07).
func OutcomeOfPayment(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SETTLED":
		return OutcomeSettled
	case "RETURNED":
		return OutcomeReturned
	case "REJECTED", "CANCELLED":
		return OutcomeFailed
	case "PREPARED", "SUBMITTED", "ACCEPTED":
		return OutcomeInFlight
	default: // PENDING and anything unknown
		return OutcomeUnresolved
	}
}

// BankingObservation is one append-only fact about what Banking reported.
type BankingObservation struct {
	ObservationID string    `json:"observation_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	InstructionID string    `json:"instruction_id"`
	Source        string    `json:"source"`
	RawStatus     string    `json:"raw_status"`
	Outcome       string    `json:"outcome"`
	AttemptID     *string   `json:"attempt_id,omitempty"`
	PaymentID     *string   `json:"payment_id,omitempty"`
	Detail        *string   `json:"detail,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// ── run, exception ───────────────────────────────────────────────────────────

type ReconExceptionDraft struct {
	Code       string  `json:"code"`
	SubjectKey string  `json:"subject_key"`
	Severity   string  `json:"severity"`
	Amount     *string `json:"amount,omitempty"`
	Detail     string  `json:"detail"`
}

type AssertionLine struct {
	Key      string `json:"key"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Matches  bool   `json:"matches"`
}

type Assertion struct {
	Name       string                `json:"name"`
	Control    string                `json:"control"`
	Matched    bool                  `json:"matched"`
	Lines      []AssertionLine       `json:"lines"`
	Exceptions []ReconExceptionDraft `json:"exceptions"`
}

type ReconRun struct {
	ReconRunID      string          `json:"recon_run_id"`
	TenantID        string          `json:"tenant_id"`
	LegalEntityID   string          `json:"legal_entity_id"`
	RunRefID        string          `json:"run_ref_id"`
	ResultID        string          `json:"result_id"`
	Outcome         string          `json:"outcome"`
	Population      json.RawMessage `json:"population"`
	PopulationHash  string          `json:"population_hash"`
	Assertions      []Assertion     `json:"assertions"`
	ExceptionCount  int             `json:"exception_count"`
	ExecutedBy      string          `json:"executed_by"`
	ExecutedAt      time.Time       `json:"executed_at"`
	ReperformedFrom *string         `json:"reperformed_from,omitempty"`
	CertifiedBy     *string         `json:"certified_by,omitempty"`
	CertifiedAt     *time.Time      `json:"certified_at,omitempty"`
	CorrelationID   string          `json:"correlation_id"`
}

type ReconException struct {
	ExceptionID       string     `json:"exception_id"`
	TenantID          string     `json:"tenant_id"`
	LegalEntityID     string     `json:"legal_entity_id"`
	RunRefID          string     `json:"run_ref_id"`
	ResultID          string     `json:"result_id"`
	ReconRunID        string     `json:"recon_run_id"`
	Code              string     `json:"code"`
	SubjectKey        string     `json:"subject_key"`
	Severity          string     `json:"severity"`
	Amount            *string    `json:"amount,omitempty"`
	Detail            string     `json:"detail"`
	State             string     `json:"state"`
	Owner             *string    `json:"owner,omitempty"`
	Resolution        *string    `json:"resolution,omitempty"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	WaivedBy          *string    `json:"waived_by,omitempty"`
	WaiverReason      *string    `json:"waiver_reason,omitempty"`
	WaiverEvidenceRef *string    `json:"waiver_evidence_ref,omitempty"`
	RaisedBy          string     `json:"raised_by"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// ── pure comparisons ─────────────────────────────────────────────────────────

func ratOrZero(r *big.Rat) *big.Rat {
	if r == nil {
		return new(big.Rat)
	}
	return r
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// compareKeyed compares two keyed exact populations. Keys on either side are compared, so
// a key present on one side only is a difference. Each difference becomes one exception
// with the key as its subject.
func compareKeyed(name, control, code string, expected, actual map[string]*big.Rat, what string) Assertion {
	set := map[string]bool{}
	for k := range expected {
		set[k] = true
	}
	for k := range actual {
		set[k] = true
	}
	a := Assertion{Name: name, Control: control, Matched: true, Lines: []AssertionLine{}, Exceptions: []ReconExceptionDraft{}}
	for _, k := range sortedKeys(set) {
		e, ac := ratOrZero(expected[k]), ratOrZero(actual[k])
		ln := AssertionLine{Key: k, Expected: FormatDecimal(e), Actual: FormatDecimal(ac), Matches: e.Cmp(ac) == 0}
		a.Lines = append(a.Lines, ln)
		if !ln.Matches {
			a.Matched = false
			diff := new(big.Rat).Sub(ac, e)
			d := FormatDecimal(diff)
			a.Exceptions = append(a.Exceptions, ReconExceptionDraft{
				Code: code, SubjectKey: k, Severity: SeverityBlocking, Amount: &d,
				Detail: fmt.Sprintf("%s %s: expected %s, found %s", what, k, ln.Expected, ln.Actual),
			})
		}
	}
	return a
}

// ReconcileResultToGL compares the posting the accepted result implies (rebuilt from the
// stored result, not read from the subject) with the lines of its POSTED payroll journal
// subject (WFP-CTRL-013). No posted subject is an exception, not a pass.
func ReconcileResultToGL(expected []PostingLine, posted *AccountingBridge) Assertion {
	if posted == nil {
		return Assertion{Name: "RESULT_TO_GL", Control: CtrlPayrollToGL, Matched: false, Lines: []AssertionLine{},
			Exceptions: []ReconExceptionDraft{{Code: ReconResultNotPosted, SubjectKey: "result", Severity: SeverityBlocking,
				Detail: "the accepted result has no POSTED payroll journal subject"}}}
	}
	sum := func(lines []PostingLine) map[string]*big.Rat {
		m := map[string]*big.Rat{}
		for _, ln := range lines {
			for side, v := range map[string]string{"DR": ln.Debit, "CR": ln.Credit} {
				r, err := ParseDecimal(v)
				if err != nil || r.Sign() == 0 {
					continue
				}
				k := ln.MappingKey + "|" + side
				if m[k] == nil {
					m[k] = new(big.Rat)
				}
				m[k].Add(m[k], r)
			}
		}
		return m
	}
	return compareKeyed("RESULT_TO_GL", CtrlPayrollToGL, ReconGLMismatch, sum(expected), sum(posted.Lines), "posted journal line")
}

// ReconcileResultToLiabilities compares the payable credits the accepted result implies
// (everything except net pay) with the liabilities recognized from it (WFP-CTRL-023).
func ReconcileResultToLiabilities(expected []PostingLine, recognized map[string]*big.Rat) Assertion {
	exp := map[string]*big.Rat{}
	for _, ln := range expected {
		if !strings.HasSuffix(ln.MappingKey, "_PAYABLE") || ln.MappingKey == KeyNetPayable {
			continue
		}
		r, err := ParseDecimal(ln.Credit)
		if err != nil {
			continue
		}
		if exp[ln.MappingKey] == nil {
			exp[ln.MappingKey] = new(big.Rat)
		}
		exp[ln.MappingKey].Add(exp[ln.MappingKey], r)
	}
	return compareKeyed("RESULT_TO_LIABILITIES", CtrlLiabilityToGL, ReconLiabilityMismatch, exp, recognized, "recognized liability for")
}

// InstructionFact is the part of a payment instruction the reconciliation reads.
type InstructionFact struct {
	InstructionID string  `json:"instruction_id"`
	Type          string  `json:"type"`
	WorkerID      *string `json:"worker_id,omitempty"`
	LiabilityID   *string `json:"liability_id,omitempty"`
	State         string  `json:"state"`
	Amount        string  `json:"amount"`
	Version       int     `json:"version"`
	// LatestOutcome is the newest Banking observation's outcome, "" when none exists.
	LatestOutcome string `json:"latest_outcome"`
}

// InstructionIsLive is whether an instruction still stands for a payment that is owed.
func InstructionIsLive(state string) bool {
	switch state {
	case PayCancelled, PaySuperseded, PayReturned:
		return false
	}
	return true
}

// ReconcileNetPay compares each worker's accepted net pay with their live NET_PAY
// instructions (the first half of WFP-CTRL-022). A worker with net pay and no live
// instruction, or one whose instructions do not sum to net pay, is an exception.
func ReconcileNetPay(workerNet map[string]*big.Rat, instructions []InstructionFact) Assertion {
	instr := map[string]*big.Rat{}
	for _, i := range instructions {
		if i.Type != PayTypeNetPay || i.WorkerID == nil || !InstructionIsLive(i.State) {
			continue
		}
		amt, err := ParseDecimal(i.Amount)
		if err != nil {
			continue
		}
		if instr[*i.WorkerID] == nil {
			instr[*i.WorkerID] = new(big.Rat)
		}
		instr[*i.WorkerID].Add(instr[*i.WorkerID], amt)
	}
	expected := map[string]*big.Rat{}
	for w, n := range workerNet {
		if n != nil && n.Sign() > 0 {
			expected[w] = n
		}
	}
	a := compareKeyed("NET_PAY_TO_INSTRUCTIONS", CtrlNetPayToSettle, ReconNetPayAmountMismatch, expected, instr, "net pay instructed for worker")
	for i := range a.Exceptions {
		if instr[a.Exceptions[i].SubjectKey] == nil {
			a.Exceptions[i].Code = ReconNetPayNotInstructed
			a.Exceptions[i].Detail = "worker " + a.Exceptions[i].SubjectKey + " has net pay and no live payment instruction"
		}
	}
	return a
}

// ReconcileBankingOutcomes reads the newest Banking observation of every instruction that
// reached Banking (the second half of WFP-CTRL-022 and WFP-CTRL-021). Only SETTLED matches.
// A returned, failed or unresolved payment blocks; one still in flight, or with no
// observation yet, is a warning (it keeps the run from certifying but not the period from
// closing).
func ReconcileBankingOutcomes(instructions []InstructionFact) Assertion {
	a := Assertion{Name: "INSTRUCTIONS_TO_BANKING", Control: CtrlNetPayToSettle, Matched: true, Lines: []AssertionLine{}, Exceptions: []ReconExceptionDraft{}}
	sorted := append([]InstructionFact(nil), instructions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].InstructionID < sorted[j].InstructionID })
	for _, i := range sorted {
		if i.State != PaySubmitted && i.State != PayHandedOff {
			continue
		}
		ln := AssertionLine{Key: i.InstructionID, Expected: OutcomeSettled, Actual: i.LatestOutcome, Matches: i.LatestOutcome == OutcomeSettled}
		if ln.Actual == "" {
			ln.Actual = "NONE"
		}
		a.Lines = append(a.Lines, ln)
		if ln.Matches {
			continue
		}
		a.Matched = false
		amt := i.Amount
		d := ReconExceptionDraft{SubjectKey: i.InstructionID, Severity: SeverityBlocking, Amount: &amt}
		switch i.LatestOutcome {
		case OutcomeReturned:
			d.Code, d.Detail = ReconPaymentReturned, "Banking reported the payment RETURNED; reopen it explicitly"
		case OutcomeFailed:
			d.Code, d.Detail = ReconPaymentFailed, "Banking reported the payment failed or cancelled"
		case OutcomeUnresolved:
			d.Code, d.Detail = ReconOutcomeUnresolved, "Banking outcome is pending or unknown; resolve the original attempt, do not retry blindly"
		case OutcomeInFlight:
			d.Code, d.Severity, d.Detail = ReconPaymentInFlight, SeverityWarning, "payment is with Banking and not yet settled"
		default:
			d.Code, d.Severity, d.Detail = ReconNoBankingObservation, SeverityWarning, "no Banking observation has been recorded for this instruction yet"
		}
		a.Exceptions = append(a.Exceptions, d)
	}
	return a
}

// RunOutcome is MATCHED only when every assertion matched and nothing was raised.
func RunOutcome(as []Assertion) string {
	for _, a := range as {
		if !a.Matched || len(a.Exceptions) > 0 {
			return ReconOutcomeBreak
		}
	}
	return ReconOutcomeMatched
}

// CollectExceptions flattens the assertions' exceptions, deduplicated by code + subject.
func CollectExceptions(as []Assertion) []ReconExceptionDraft {
	seen := map[string]bool{}
	out := []ReconExceptionDraft{}
	for _, a := range as {
		for _, e := range a.Exceptions {
			k := e.Code + "\x00" + e.SubjectKey
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, e)
		}
	}
	return out
}

// ── frozen population ────────────────────────────────────────────────────────

type LiabilityFact struct {
	LiabilityID string            `json:"liability_id"`
	ClassCode   string            `json:"class_code"`
	State       string            `json:"state"`
	Amount      string            `json:"amount"`
	KindAmounts map[string]string `json:"kind_amounts"`
	Version     int               `json:"version"`
}

type PostingFact struct {
	BridgeID string        `json:"bridge_id"`
	State    string        `json:"state"`
	Lines    []PostingLine `json:"lines"`
}

// ReconPopulation is exactly what one reconciliation run compared. It is stored with the
// run and hashed, so a later reperformance can show what changed.
type ReconPopulation struct {
	ResultID          string            `json:"result_id"`
	ResultFingerprint string            `json:"result_fingerprint"`
	TotalGross        string            `json:"total_gross"`
	TotalNetPay       string            `json:"total_net_pay"`
	WorkerNet         map[string]string `json:"worker_net"`
	Posting           *PostingFact      `json:"posting,omitempty"`
	Liabilities       []LiabilityFact   `json:"liabilities"`
	Instructions      []InstructionFact `json:"instructions"`
}

// Hash is the SHA-256 of the canonical (sorted, struct-ordered) JSON of the population.
func (p ReconPopulation) Hash() (string, []byte, error) {
	sort.Slice(p.Liabilities, func(i, j int) bool { return p.Liabilities[i].LiabilityID < p.Liabilities[j].LiabilityID })
	sort.Slice(p.Instructions, func(i, j int) bool { return p.Instructions[i].InstructionID < p.Instructions[j].InstructionID })
	raw, err := json.Marshal(p) // map keys are sorted by encoding/json
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw, nil
}

// ── close gate (WFP-CTRL-030) ────────────────────────────────────────────────

type GateBlocker struct {
	Code     string `json:"code"`
	ResultID string `json:"result_id,omitempty"`
	RunRefID string `json:"run_ref_id,omitempty"`
	Subject  string `json:"subject,omitempty"`
	Detail   string `json:"detail"`
}

type CloseGate struct {
	LegalEntityID string        `json:"legal_entity_id"`
	Period        string        `json:"period_id"`
	Open          bool          `json:"open"`
	Results       int           `json:"accepted_results"`
	Blockers      []GateBlocker `json:"blockers"`
	Warnings      []GateBlocker `json:"warnings"`
	Control       string        `json:"control"`
}

const (
	GateNoReconRun          = "RECONCILIATION_NOT_RUN"
	GateReconBreak          = "RECONCILIATION_BREAK"
	GateNotCertified        = "RECONCILIATION_NOT_CERTIFIED"
	GateOpenException       = "OPEN_RECONCILIATION_EXCEPTION"
	GateOpenLiabException   = "OPEN_LIABILITY_EXCEPTION"
	GateNotPosted           = "PAYROLL_NOT_POSTED"
	GateUnresolvedPayment   = "UNRESOLVED_PAYMENT_OUTCOME"
	GateStaleReconciliation = "RECONCILIATION_STALE"
)

// ── correction chain completeness (WFP-CTRL-024) ─────────────────────────────

type CorrectionEffect struct {
	Effect   string `json:"effect"` // ACCOUNTING, LIABILITIES, PAYMENTS
	Complete bool   `json:"complete"`
	Detail   string `json:"detail"`
	Pending  int    `json:"pending"`
}

type CorrectionCompleteness struct {
	RunRefID string             `json:"run_ref_id"`
	Complete bool               `json:"complete"`
	Effects  []CorrectionEffect `json:"effects"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrReconResultNotAccepted  = errorString("reconciliation runs only on an ACCEPTED result")
	ErrReconRunNotFound        = errorString("reconciliation run not found")
	ErrReconExceptionNotFound  = errorString("reconciliation exception not found")
	ErrReconNotMatched         = errorString("only a MATCHED reconciliation run can be certified")
	ErrReconSoD                = errorString("separation of duties: the principal who executed a reconciliation run cannot certify it, and the principal who raised an exception cannot waive it")
	ErrReconAlreadyCertified   = errorString("this reconciliation run is already certified")
	ErrReconExceptionClosed    = errorString("this reconciliation exception is already closed")
	ErrInstructionNotReturned  = errorString("Banking has not reported this payment RETURNED")
	ErrOutcomeUnresolved       = errorString("the Banking outcome of this instruction is unresolved; resolve the original attempt first")
	ErrPaymentLinkMismatch     = errorString("the payment does not belong to this instruction")
	ErrReconRunStale           = errorString("the population changed since this reconciliation run (or a newer run exists); reperform it")
	ErrReturnedAfterSettlement = errorString("this payment already settled a liability; reversing a settlement is not modelled, adjust the liability explicitly")
)
