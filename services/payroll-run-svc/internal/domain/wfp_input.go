package domain

// ZS-SVC-L-001 WFP-03 — Payroll Input Interface.
//
// An input line is a fact pulled from its authoritative source (the employment
// contract, compensation, benefits) together with its provenance, classified by
// an effective-dated code mapping. Nothing here calculates pay. Money is an
// exact decimal STRING; the sources expose float64, so the figure as it arrived
// is kept beside the decimal it became.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SourceBaseSalary      = "BASE_SALARY"
	SourceCompComponent   = "COMP_COMPONENT"
	SourceCompBonus       = "COMP_BONUS"
	SourceBenefitEmployee = "BENEFIT_EMPLOYEE"
	SourceBenefitEmployer = "BENEFIT_EMPLOYER"
	SourceManual          = "MANUAL"
)

var validInputSourceKinds = map[string]bool{
	SourceBaseSalary: true, SourceCompComponent: true, SourceCompBonus: true,
	SourceBenefitEmployee: true, SourceBenefitEmployer: true, SourceManual: true,
}

const (
	CategoryEarning       = "EARNING"
	CategoryDeduction     = "DEDUCTION"
	CategoryBenefit       = "BENEFIT"
	CategoryReimbursement = "REIMBURSEMENT"
	CategoryAdjustment    = "ADJUSTMENT"
)

var validCategories = map[string]bool{
	CategoryEarning: true, CategoryDeduction: true, CategoryBenefit: true,
	CategoryReimbursement: true, CategoryAdjustment: true,
}

const (
	TaxTaxable       = "TAXABLE"
	TaxNonTaxable    = "NON_TAXABLE"
	TaxPreTax        = "PRE_TAX"
	TaxPostTax       = "POST_TAX"
	TaxNotApplicable = "NOT_APPLICABLE"
)

var validTaxTreatments = map[string]bool{
	TaxTaxable: true, TaxNonTaxable: true, TaxPreTax: true, TaxPostTax: true, TaxNotApplicable: true,
}

const (
	InputBatchReceived   = "RECEIVED"
	InputBatchReady      = "READY"
	InputBatchFrozen     = "FROZEN"
	InputBatchConsumed   = "CONSUMED"
	InputBatchSuperseded = "SUPERSEDED"
	InputBatchRejected   = "REJECTED"
	InputBatchCancelled  = "CANCELLED"

	InputLineAccepted    = "ACCEPTED"
	InputLineQuarantined = "QUARANTINED"

	InputNormalizationVersion = "input-v1"
)

// Quarantine reasons. Stable codes: runbook WFP-RB-03 keys off them.
const (
	InReasonUnmapped             = "UNMAPPED_CODE"
	InReasonCategoryConflict     = "CATEGORY_CONFLICT"
	InReasonTaxabilityConflict   = "TAXABILITY_CONFLICT"
	InReasonInvalidAmount        = "INVALID_AMOUNT"
	InReasonNegativeAmount       = "NEGATIVE_AMOUNT"
	InReasonInvalidCurrency      = "INVALID_CURRENCY"
	InReasonCurrencyMismatch     = "CURRENCY_MISMATCH"
	InReasonAmbiguousPayCurrency = "AMBIGUOUS_PAY_CURRENCY"
	InReasonMissingProvenance    = "MISSING_PROVENANCE"
	InReasonMissingApproval      = "MISSING_APPROVAL_EVIDENCE"
	InReasonDuplicateCandidate   = "DUPLICATE_CANDIDATE"
	InReasonNoActiveContract     = "NO_ACTIVE_CONTRACT"
	InReasonWorkerContext        = "WORKER_CONTEXT_INVALID"
)

// ── mapping ──────────────────────────────────────────────────────────────────

type InputCodeMapping struct {
	MappingID      string    `json:"mapping_id"`
	TenantID       string    `json:"tenant_id"`
	SourceKind     string    `json:"source_kind"`
	SourceCode     string    `json:"source_code"`
	MappingVersion int       `json:"mapping_version"`
	PayrollCode    string    `json:"payroll_code"`
	Category       string    `json:"category"`
	TaxTreatment   string    `json:"tax_treatment"`
	Active         bool      `json:"active"`
	EffectiveFrom  string    `json:"effective_from"`
	RegisteredBy   string    `json:"registered_by"`
	CreatedAt      time.Time `json:"created_at"`
}

type RegisterInputCodeMappingRequest struct {
	SourceKind    string `json:"source_kind"`
	SourceCode    string `json:"source_code"`
	PayrollCode   string `json:"payroll_code"`
	Category      string `json:"category"`
	TaxTreatment  string `json:"tax_treatment"`
	Active        *bool  `json:"active,omitempty"` // default true
	EffectiveFrom string `json:"effective_from"`
}

func (r RegisterInputCodeMappingRequest) IsActive() bool { return r.Active == nil || *r.Active }

func (r RegisterInputCodeMappingRequest) Validate() error {
	if !validInputSourceKinds[r.SourceKind] {
		return fmt.Errorf("source_kind must be one of BASE_SALARY, COMP_COMPONENT, COMP_BONUS, BENEFIT_EMPLOYEE, BENEFIT_EMPLOYER, MANUAL")
	}
	if strings.TrimSpace(r.SourceCode) == "" {
		return fmt.Errorf("source_code is required")
	}
	if strings.TrimSpace(r.PayrollCode) == "" {
		return fmt.Errorf("payroll_code is required")
	}
	if !validCategories[r.Category] {
		return fmt.Errorf("category must be one of EARNING, DEDUCTION, BENEFIT, REIMBURSEMENT, ADJUSTMENT")
	}
	if !validTaxTreatments[r.TaxTreatment] {
		return fmt.Errorf("tax_treatment must be one of TAXABLE, NON_TAXABLE, PRE_TAX, POST_TAX, NOT_APPLICABLE")
	}
	if _, err := time.Parse("2006-01-02", r.EffectiveFrom); err != nil {
		return fmt.Errorf("effective_from must be YYYY-MM-DD")
	}
	return nil
}

func MappingKey(kind, code string) string { return kind + "|" + code }

// ── batch & line ─────────────────────────────────────────────────────────────

type InputBatch struct {
	BatchID              string     `json:"batch_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	PayrollPeriodID      string     `json:"payroll_period_id"`
	PeriodStart          string     `json:"period_start"`
	PeriodEnd            string     `json:"period_end"`
	BatchVersion         int        `json:"batch_version"`
	OnInvalid            string     `json:"on_invalid"`
	State                string     `json:"state"`
	RejectionReason      *string    `json:"rejection_reason,omitempty"`
	WorkerCount          int        `json:"worker_count"`
	LineCount            int        `json:"line_count"`
	AcceptedLineCount    int        `json:"accepted_line_count"`
	QuarantinedLineCount int        `json:"quarantined_line_count"`
	SourceHash           *string    `json:"source_hash,omitempty"`
	NormalizationVersion string     `json:"normalization_version"`
	SupersedesBatchID    *string    `json:"supersedes_batch_id,omitempty"`
	PayrollRunID         *string    `json:"payroll_run_id,omitempty"`
	Version              int        `json:"version"`
	FrozenAt             *time.Time `json:"frozen_at,omitempty"`
	FrozenBy             *string    `json:"frozen_by,omitempty"`
	ConsumedAt           *time.Time `json:"consumed_at,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedBy            string     `json:"created_by"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type InputLine struct {
	LineID          string    `json:"line_id"`
	BatchID         string    `json:"batch_id"`
	WorkerID        string    `json:"worker_id"`
	SourceKind      string    `json:"source_kind"`
	SourceLineID    string    `json:"source_line_id"`
	SourceRef       *string   `json:"source_ref,omitempty"`
	InputCode       string    `json:"input_code"`
	PayrollCode     *string   `json:"payroll_code,omitempty"`
	Category        string    `json:"category"`
	TaxTreatment    *string   `json:"tax_treatment,omitempty"`
	MappingID       *string   `json:"mapping_id,omitempty"`
	MappingVersion  *int      `json:"mapping_version,omitempty"`
	Amount          string    `json:"amount"`
	Currency        string    `json:"currency"`
	EffectiveDate   string    `json:"effective_date"`
	ApprovalRef     *string   `json:"approval_ref,omitempty"`
	Reason          *string   `json:"reason,omitempty"`
	SourceAmountRaw string    `json:"source_amount_raw"`
	State           string    `json:"state"`
	Quarantine      *string   `json:"quarantine_reason,omitempty"`
	LineHash        string    `json:"line_hash"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
}

type InputControlTotal struct {
	Category    string `json:"category"`
	Currency    string `json:"currency"`
	LineCount   int    `json:"line_count"`
	TotalAmount string `json:"total_amount"`
}

type CreateInputBatchRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	PayrollPeriodID string `json:"payroll_period_id"`
	PeriodStart     string `json:"period_start"`
	PeriodEnd       string `json:"period_end"`
	OnInvalid       string `json:"on_invalid,omitempty"`
}

func (r CreateInputBatchRequest) WithDefaults() CreateInputBatchRequest {
	if r.OnInvalid == "" {
		r.OnInvalid = OnInvalidRejectBatch // the conservative policy
	}
	return r
}

func (r CreateInputBatchRequest) Validate() error {
	if strings.TrimSpace(r.LegalEntityID) == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if strings.TrimSpace(r.PayrollPeriodID) == "" {
		return fmt.Errorf("payroll_period_id is required")
	}
	from, err := time.Parse("2006-01-02", r.PeriodStart)
	if err != nil {
		return fmt.Errorf("period_start must be YYYY-MM-DD")
	}
	to, err := time.Parse("2006-01-02", r.PeriodEnd)
	if err != nil {
		return fmt.Errorf("period_end must be YYYY-MM-DD")
	}
	if to.Before(from) {
		return fmt.Errorf("period_end must not be before period_start")
	}
	if r.OnInvalid != "" && r.OnInvalid != OnInvalidQuarantineLines && r.OnInvalid != OnInvalidRejectBatch {
		return fmt.Errorf("on_invalid must be QUARANTINE_LINES or REJECT_BATCH")
	}
	return nil
}

type InputBatchTransitionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
	OnInvalid       string `json:"on_invalid,omitempty"` // supersede only
	PayrollRunID    string `json:"payroll_run_id,omitempty"`
}

// OnInvalid policies (shared vocabulary with WFP-02).
const (
	OnInvalidQuarantineLines = "QUARANTINE_LINES"
	OnInvalidRejectBatch     = "REJECT_BATCH"
)

// ── pulled source data ───────────────────────────────────────────────────────

type SourceBase struct {
	ContractID   string
	Amount       float64
	Currency     string
	PayFrequency string
}

type SourceComponent struct {
	StructureID string
	Code, Name  string
	Type        string // EARNING | DEDUCTION
	IsTaxable   bool
	Amount      float64
	Currency    string
}

type SourceBonus struct {
	GrantID, BonusType string
	Amount             float64
	Currency           string
	GrantDate          string
	ApprovedBy         *string
}

type SourceElection struct {
	ElectionID, PlanID, PlanType string
	TaxTreatment                 string // PRE_TAX | POST_TAX
	Currency                     string
	EmployeeAmount               float64
	EmployerAmount               float64
}

// SourceManualInput is an APPROVED manual input (maker-checker) for one worker. It is pulled into a batch like
// any other source and classified by the governed mapping of source kind MANUAL and its input code.
type SourceManualInput struct {
	ManualID, Code, Category string
	Amount                   float64
	Currency, EffectiveDate  string
	Reason, EvidenceRef      string
	ApprovedBy               string
}

// WorkerContext is employee-master-svc's verdict (WFP-01) for one worker.
type WorkerContext struct {
	Valid         bool
	Reason        string // first reason when not valid
	PayCurrencies []string
	// PayrollJurisdictions are the distinct payroll jurisdictions of the worker's active financial
	// profile on the as-of date. Exactly one is needed to compute tax; none or several is refused.
	PayrollJurisdictions []string
}

// WorkerSource is everything pulled for one worker. A nil Base means the worker
// has no active contract: a state to report, not to paper over.
type WorkerSource struct {
	WorkerID   string
	Context    WorkerContext
	Base       *SourceBase
	Components []SourceComponent
	Bonuses    []SourceBonus
	Elections  []SourceElection
	Manuals    []SourceManualInput
}

// FormatAmount writes a source float as an exact 4-decimal string and returns
// the figure as it arrived. ok is false for NaN/Inf.
func FormatAmount(f float64) (text, raw string, ok bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0.0000", fmt.Sprint(f), false
	}
	return strconv.FormatFloat(f, 'f', 4, 64), strconv.FormatFloat(f, 'f', -1, 64), true
}

var currencyShape = func(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func strp(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// candidate is one pre-classification input from a source.
type candidate struct {
	kind, sourceLineID, sourceRef, code string
	expectedCategory                    string
	sourceTax                           string // "" when the source gives no classification
	amount                              float64
	currency, date                      string
	approvalRef                         *string
	needsApproval                       bool
	reason                              string
}

// BuildInputLines turns pulled source data into classified lines. Nothing is
// repaired or defaulted: a line that cannot be classified is QUARANTINED with its
// reason and stays visible. mappings are those in force on the period end.
func BuildInputLines(sources []WorkerSource, mappings map[string]InputCodeMapping, periodEnd string) []InputLine {
	var out []InputLine
	for _, w := range sources {
		var cands []candidate
		if w.Base == nil {
			out = append(out, exceptionLine(w.WorkerID, SourceBaseSalary, "BASE:"+w.WorkerID+":none", "BASE", periodEnd, InReasonNoActiveContract))
		} else {
			cands = append(cands, candidate{
				kind: SourceBaseSalary, sourceLineID: "BASE:" + w.WorkerID + ":" + w.Base.ContractID, sourceRef: w.Base.ContractID,
				code: "BASE", expectedCategory: CategoryEarning, amount: w.Base.Amount, currency: w.Base.Currency, date: periodEnd,
				reason: "pay_frequency=" + w.Base.PayFrequency,
			})
		}
		for _, c := range w.Components {
			tax := TaxNonTaxable
			if c.IsTaxable {
				tax = TaxTaxable
			}
			if c.Type == CategoryDeduction {
				tax = "" // the source's meaning of the flag on a deduction is not asserted
			}
			cands = append(cands, candidate{
				kind: SourceCompComponent, sourceLineID: "COMP:" + w.WorkerID + ":" + c.StructureID + ":" + c.Code, sourceRef: c.StructureID,
				code: c.Code, expectedCategory: c.Type, sourceTax: tax, amount: c.Amount, currency: c.Currency, date: periodEnd, reason: c.Name,
			})
		}
		for _, b := range w.Bonuses {
			cands = append(cands, candidate{
				kind: SourceCompBonus, sourceLineID: "BONUS:" + b.GrantID, sourceRef: b.GrantID, code: b.BonusType,
				expectedCategory: CategoryEarning, amount: b.Amount, currency: b.Currency, date: b.GrantDate,
				approvalRef: b.ApprovedBy, needsApproval: true, reason: "BONUS:" + b.BonusType,
			})
		}
		for _, e := range w.Elections {
			// A zero contribution is no input; it is skipped, not recorded as a line.
			if e.EmployeeAmount == 0 && e.EmployerAmount == 0 {
				continue
			}
			cands = append(cands, candidate{
				kind: SourceBenefitEmployee, sourceLineID: "BEN-EE:" + e.ElectionID, sourceRef: e.ElectionID, code: e.PlanType,
				expectedCategory: CategoryDeduction, sourceTax: e.TaxTreatment, amount: e.EmployeeAmount, currency: e.Currency,
				date: periodEnd, reason: "plan=" + e.PlanID,
			})
			cands = append(cands, candidate{
				kind: SourceBenefitEmployer, sourceLineID: "BEN-ER:" + e.ElectionID, sourceRef: e.ElectionID, code: e.PlanType,
				expectedCategory: CategoryBenefit, amount: e.EmployerAmount, currency: e.Currency, date: periodEnd, reason: "plan=" + e.PlanID,
			})
		}
		for _, m := range w.Manuals {
			// A manual input's provenance is its proposal, and it always carries its approver.
			approver := m.ApprovedBy
			cands = append(cands, candidate{
				kind: SourceManual, sourceLineID: "MANUAL:" + m.ManualID, sourceRef: m.ManualID, code: m.Code,
				expectedCategory: m.Category, amount: m.Amount, currency: m.Currency, date: m.EffectiveDate,
				approvalRef: &approver, needsApproval: true, reason: "MANUAL:" + m.Reason + " [evidence=" + m.EvidenceRef + "]",
			})
		}
		for _, c := range cands {
			out = append(out, classify(w, c, mappings))
		}
	}
	flagDuplicateBonuses(out)
	sort.Slice(out, func(i, j int) bool { return out[i].SourceLineID < out[j].SourceLineID })
	return out
}

func exceptionLine(worker, kind, sourceLineID, code, date, reason string) InputLine {
	l := InputLine{
		LineID: uuid.NewString(), WorkerID: worker, SourceKind: kind, SourceLineID: sourceLineID, InputCode: code,
		Category: CategoryEarning, Amount: "0.0000", Currency: "XXX", EffectiveDate: date, SourceAmountRaw: "0",
		State: InputLineQuarantined, Quarantine: &reason,
	}
	l.LineHash = LineHash(l)
	return l
}

// classify applies the rules in a fixed order, so the most fundamental failure
// is the one recorded.
func classify(w WorkerSource, c candidate, mappings map[string]InputCodeMapping) InputLine {
	text, raw, finite := FormatAmount(c.amount)
	l := InputLine{
		LineID: uuid.NewString(), WorkerID: w.WorkerID, SourceKind: c.kind, SourceLineID: c.sourceLineID,
		SourceRef: strp(c.sourceRef), InputCode: c.code, Category: c.expectedCategory, Amount: text, Currency: c.currency,
		EffectiveDate: c.date, ApprovalRef: c.approvalRef, Reason: strp(c.reason), SourceAmountRaw: raw,
		State: InputLineAccepted,
	}
	quarantine := func(reason string) InputLine {
		l.State, l.Quarantine = InputLineQuarantined, &reason
		l.LineHash = LineHash(l)
		return l
	}

	if !w.Context.Valid {
		reason := w.Context.Reason
		if reason == "" {
			reason = InReasonWorkerContext
		}
		return quarantine(reason)
	}
	if l.SourceRef == nil {
		return quarantine(InReasonMissingProvenance)
	}
	m, ok := mappings[MappingKey(c.kind, c.code)]
	if !ok || !m.Active {
		return quarantine(InReasonUnmapped)
	}
	l.PayrollCode, l.MappingID, l.MappingVersion = &m.PayrollCode, &m.MappingID, &m.MappingVersion
	l.Category, l.TaxTreatment = m.Category, &m.TaxTreatment

	if m.Category != c.expectedCategory {
		return quarantine(InReasonCategoryConflict)
	}
	if c.sourceTax != "" && c.sourceTax != m.TaxTreatment {
		return quarantine(InReasonTaxabilityConflict)
	}
	if !finite {
		return quarantine(InReasonInvalidAmount)
	}
	if c.amount < 0 {
		return quarantine(InReasonNegativeAmount)
	}
	if !currencyShape(c.currency) {
		return quarantine(InReasonInvalidCurrency)
	}
	switch len(w.Context.PayCurrencies) {
	case 0:
	case 1:
		if w.Context.PayCurrencies[0] != c.currency {
			return quarantine(InReasonCurrencyMismatch) // never converted
		}
	default:
		return quarantine(InReasonAmbiguousPayCurrency)
	}
	if c.needsApproval && (c.approvalRef == nil || strings.TrimSpace(*c.approvalRef) == "") {
		return quarantine(InReasonMissingApproval)
	}
	l.LineHash = LineHash(l)
	return l
}

// flagDuplicateBonuses quarantines later bonus lines that share a business
// subject with an earlier one under a different source id (WFP-NP-008). The
// earliest source id is kept; the rest go to review.
func flagDuplicateBonuses(lines []InputLine) {
	sort.Slice(lines, func(i, j int) bool { return lines[i].SourceLineID < lines[j].SourceLineID })
	seen := map[string]bool{}
	for i := range lines {
		l := &lines[i]
		if l.SourceKind != SourceCompBonus || l.State != InputLineAccepted || l.PayrollCode == nil {
			continue
		}
		key := l.WorkerID + "|" + *l.PayrollCode + "|" + l.Amount + "|" + l.Currency + "|" + l.EffectiveDate
		if seen[key] {
			r := InReasonDuplicateCandidate
			l.State, l.Quarantine = InputLineQuarantined, &r
			l.LineHash = LineHash(*l)
			continue
		}
		seen[key] = true
	}
}

// LineHash identifies the material content of a line (not its row id), so a
// changed source is detectable after the snapshot was taken.
func LineHash(l InputLine) string {
	h := sha256.New()
	deref := func(p *string) string {
		if p == nil {
			return "\x00nil"
		}
		return *p
	}
	ver := "\x00nil"
	if l.MappingVersion != nil {
		ver = strconv.Itoa(*l.MappingVersion)
	}
	for _, f := range []string{
		l.SourceLineID, l.WorkerID, l.SourceKind, l.InputCode, deref(l.PayrollCode), l.Category, deref(l.TaxTreatment),
		deref(l.MappingID), ver, l.Amount, l.Currency, l.EffectiveDate, deref(l.ApprovalRef), deref(l.SourceRef), l.State, deref(l.Quarantine),
	} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// InputSourceHash is the batch control hash over the ACCEPTED lines, sorted by
// source line so discovery order never changes it.
func InputSourceHash(lines []InputLine) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.State == InputLineAccepted {
			parts = append(parts, l.SourceLineID+"="+l.LineHash)
		}
	}
	sort.Strings(parts)
	h := sha256.New()
	for _, s := range parts {
		fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Source-change findings.
const (
	InputIssueSourceMissing = "SOURCE_MISSING"
	InputIssueSourceChanged = "SOURCE_CHANGED_SINCE_SNAPSHOT"
	InputIssueNewSourceLine = "NEW_SOURCE_LINE"
)

type InputIssue struct {
	SourceLineID string `json:"source_line_id"`
	Issue        string `json:"issue"`
}

// DiffInputLines compares a stored batch's ACCEPTED population with a fresh pull
// of the same sources. Any difference means the batch no longer describes the
// source and must be superseded, not frozen.
func DiffInputLines(stored, fresh []InputLine) []InputIssue {
	old, cur := map[string]string{}, map[string]string{}
	for _, l := range stored {
		if l.State == InputLineAccepted {
			old[l.SourceLineID] = l.LineHash
		}
	}
	for _, l := range fresh {
		if l.State == InputLineAccepted {
			cur[l.SourceLineID] = l.LineHash
		}
	}
	var issues []InputIssue
	for id, h := range old {
		switch ch, ok := cur[id]; {
		case !ok:
			issues = append(issues, InputIssue{id, InputIssueSourceMissing})
		case ch != h:
			issues = append(issues, InputIssue{id, InputIssueSourceChanged})
		}
	}
	for id := range cur {
		if _, ok := old[id]; !ok {
			issues = append(issues, InputIssue{id, InputIssueNewSourceLine})
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].SourceLineID < issues[j].SourceLineID })
	return issues
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrInputBatchNotFound        = errorString("payroll input batch not found")
	ErrInputBatchVersion         = errorString("stale expected_version: the batch changed since it was read")
	ErrInputLiveBatchExists      = errorString("a live input batch already exists for this entity and period; supersede it instead")
	ErrInputBatchNotReady        = errorString("only a READY batch can be frozen or cancelled")
	ErrInputBatchNotFrozen       = errorString("only a FROZEN batch can be consumed by a payroll run")
	ErrInputNotSupersedable      = errorString("only a READY or FROZEN batch can be superseded; a consumed batch is corrected through payroll correction")
	ErrInputSourceChanged        = errorString("source data changed since the snapshot; supersede the batch")
	ErrInputSupersedeWouldReject = errorString("the replacement batch would be REJECTED; the existing batch is left untouched")
	ErrMappingNotFound           = errorString("payroll input code mapping not found")
	ErrMappingRegression         = errorString("effective_from must be later than the latest mapping for this source code")
	ErrRunPeriodMismatch         = errorString("the payroll run does not match this batch's legal entity and period")
	ErrRunAlreadyLinked          = errorString("this payroll run already consumed an input batch")
	ErrTooManyWorkers            = errorString("legal entity has more workers than a synchronous input batch supports")
	ErrSourceUnavailable         = errorString("a payroll input source could not be reached")
)
