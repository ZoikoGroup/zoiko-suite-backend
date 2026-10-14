package domain

// ZS-SVC-L-001 WFP-04 — Payroll Result Interface.
//
// A result is a gross-to-net outcome produced by a payroll engine. WFP-04 never
// recomputes it: it checks that the source run is final and evidenced, that the
// rule provenance and worker/entity context are real, and that the figures
// reconcile EXACTLY (decimal, no tolerance); then it keeps the result as
// immutable evidence. All money is a decimal string; sums use exact rationals.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ResultAccepted = "ACCEPTED"
	ResultRejected = "REJECTED"

	// SourceZoikoPayroll is the source system id of this platform's own payroll
	// calculation. It needs an ACTIVE PAYROLL_RESULT trust profile like any other.
	SourceZoikoPayroll = "zoiko-payroll"

	ZoikoCalculationRef = "zoiko-payroll-calc/1"
)

// Evidence flags the Zoiko path sets, because they are true of the calculation as
// it exists today and a reader of the evidence must be able to see them.
const (
	FlagPlaceholderTaxRate             = "PLACEHOLDER_TAX_RATE"
	FlagEmployerLiabilitiesNotComputed = "EMPLOYER_LIABILITIES_NOT_COMPUTED"
)

// Source run statuses. Only FINAL and APPROVED can be accepted.
var validSourceRunStatuses = map[string]bool{
	"DRAFT": true, "CALCULATED": true, "IN_REVIEW": true, "APPROVED": true, "FINAL": true, "CANCELLED": true,
}

func SourceRunIsFinal(status string) bool { return status == "FINAL" || status == "APPROVED" }

// Component kinds. Employee-side kinds reduce net pay; employer-side kinds are
// costs/liabilities on top of gross.
const (
	KindEarning = "EARNING"

	KindEmployeeDeduction = "EMPLOYEE_DEDUCTION"
	KindEmployeeTax       = "EMPLOYEE_TAX"
	KindEmployeeSocial    = "EMPLOYEE_SOCIAL"
	KindEmployeePension   = "EMPLOYEE_PENSION"
	KindEmployeeBenefit   = "EMPLOYEE_BENEFIT"

	KindEmployerTax     = "EMPLOYER_TAX"
	KindEmployerSocial  = "EMPLOYER_SOCIAL"
	KindEmployerPension = "EMPLOYER_PENSION"
	KindEmployerBenefit = "EMPLOYER_BENEFIT"
	KindEmployerOther   = "EMPLOYER_OTHER"
)

var employeeKinds = map[string]bool{
	KindEmployeeDeduction: true, KindEmployeeTax: true, KindEmployeeSocial: true, KindEmployeePension: true, KindEmployeeBenefit: true,
}

var employerKinds = map[string]bool{
	KindEmployerTax: true, KindEmployerSocial: true, KindEmployerPension: true, KindEmployerBenefit: true, KindEmployerOther: true,
}

func validKind(k string) bool { return k == KindEarning || employeeKinds[k] || employerKinds[k] }

// Rejection reason codes. Stable: runbook WFP-RB-04 keys off them.
const (
	RsRunNotFinal             = "RUN_NOT_FINAL"
	RsMissingApprovalEvidence = "MISSING_APPROVAL_EVIDENCE"
	RsMissingCalculationRef   = "MISSING_CALCULATION_EVIDENCE"
	RsEmptyResult             = "EMPTY_RESULT"
	RsDuplicateWorker         = "DUPLICATE_WORKER"
	RsWorkerContext           = "WORKER_CONTEXT_INVALID"
	RsCurrencyMismatch        = "CURRENCY_MISMATCH"
	RsAmbiguousPayCurrency    = "AMBIGUOUS_PAY_CURRENCY"
	RsGrossNotEarnings        = "GROSS_NOT_EQUAL_TO_EARNINGS"
	RsDeductionsMismatch      = "EMPLOYEE_DEDUCTIONS_MISMATCH"
	RsEmployerMismatch        = "EMPLOYER_LIABILITIES_MISMATCH"
	RsNetPayMismatch          = "NET_PAY_MISMATCH"
	RsNegativeNetPay          = "NEGATIVE_NET_PAY"
	RsRunTotalMismatch        = "RUN_TOTAL_MISMATCH"
	RsInputBatchInvalid       = "INPUT_BATCH_INVALID"
)

// ── request ──────────────────────────────────────────────────────────────────

type ResultComponentInput struct {
	Code         string  `json:"code"`
	Kind         string  `json:"kind"`
	Amount       string  `json:"amount"`
	AuthorityRef *string `json:"authority_ref,omitempty"`
}

type ResultWorkerInput struct {
	WorkerID            string                 `json:"worker_id"`
	Currency            string                 `json:"currency"`
	Gross               string                 `json:"gross"`
	TaxablePay          string                 `json:"taxable_pay"`
	EmployeeDeductions  string                 `json:"employee_deductions"`
	EmployerLiabilities string                 `json:"employer_liabilities"`
	NetPay              string                 `json:"net_pay"`
	Components          []ResultComponentInput `json:"components"`
}

// ResultTotalsInput is what the SOURCE declares for the whole run. It is compared
// with the sum of the worker rows, never trusted in place of them.
type ResultTotalsInput struct {
	WorkerCount         int    `json:"worker_count"`
	Gross               string `json:"gross"`
	TaxablePay          string `json:"taxable_pay"`
	EmployeeDeductions  string `json:"employee_deductions"`
	EmployerLiabilities string `json:"employer_liabilities"`
	NetPay              string `json:"net_pay"`
}

// ReceivePayrollResultRequest deliberately has no tenant_id: the tenant is the
// authenticated one, never a body field (WFP-NP-001).
type ReceivePayrollResultRequest struct {
	LegalEntityID     string              `json:"legal_entity_id"`
	SourceSystemID    string              `json:"source_system_id"`
	SourceRunID       string              `json:"source_run_id"`
	SourceRunVersion  int                 `json:"source_run_version"`
	RunKind           string              `json:"run_kind"`
	PayrollPeriodID   string              `json:"payroll_period_id"`
	PayDate           string              `json:"pay_date"`
	RuleManifestID    string              `json:"rule_manifest_id"`
	SourceRunStatus   string              `json:"source_run_status"`
	SourceApprovalRef string              `json:"source_approval_ref,omitempty"`
	CalculationRef    string              `json:"calculation_ref,omitempty"`
	Currency          string              `json:"currency"`
	InputBatchID      *string             `json:"input_batch_id,omitempty"`
	PayrollRunID      *string             `json:"payroll_run_id,omitempty"`
	EvidenceFlags     []string            `json:"evidence_flags,omitempty"`
	Workers           []ResultWorkerInput `json:"workers"`
	RunTotals         ResultTotalsInput   `json:"run_totals"`
}

// RunRef is the run-identity part of the request.
func (r ReceivePayrollResultRequest) RunRef() RegisterPayrollRunRefRequest {
	return RegisterPayrollRunRefRequest{
		LegalEntityID: r.LegalEntityID, SourceSystemID: r.SourceSystemID, SourceRunID: r.SourceRunID,
		SourceRunVersion: r.SourceRunVersion, RunKind: r.RunKind, PayrollPeriodID: r.PayrollPeriodID,
		PayDate: r.PayDate, RuleManifestID: r.RuleManifestID, PayrollRunID: r.PayrollRunID,
	}
}

const (
	MaxResultWorkers    = 1000
	maxComponentsPerRow = 200
)

var decimalRE = regexp.MustCompile(`^-?\d{1,14}(\.\d{1,4})?$`)

// maxWorkerIntDigits bounds a single worker figure so the sum of MaxResultWorkers of
// them still fits NUMERIC(18,4); run-level totals may use the full width.
const maxWorkerIntDigits = 10

// ParseDecimal reads an exact decimal string (at most 4 places).
func ParseDecimal(s string) (*big.Rat, error) {
	if !decimalRE.MatchString(s) {
		return nil, fmt.Errorf("%q is not a decimal with at most 4 places", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal", s)
	}
	return r, nil
}

// FormatDecimal writes a rational that has at most 4 decimal places exactly.
func FormatDecimal(r *big.Rat) string { return r.FloatString(4) }

func normDec(s string) string {
	r, err := ParseDecimal(s)
	if err != nil {
		return s
	}
	return FormatDecimal(r)
}

func nonNegDecimal(field, s string, allowNegative bool) error {
	r, err := ParseDecimal(s)
	if err != nil {
		return fmt.Errorf("%s: %v", field, err)
	}
	if !strings.HasPrefix(field, "run_totals.") {
		whole := strings.TrimPrefix(strings.SplitN(s, ".", 2)[0], "-")
		if len(whole) > maxWorkerIntDigits {
			return fmt.Errorf("%s has more than %d integer digits", field, maxWorkerIntDigits)
		}
	}
	if r.Sign() < 0 && !allowNegative {
		return fmt.Errorf("%s must not be negative", field)
	}
	return nil
}

// Validate is the structural check (HTTP 400, nothing stored): shapes and formats.
// Whether the figures reconcile is a control, evaluated by EvaluateResult.
func (r ReceivePayrollResultRequest) Validate() error {
	if err := r.RunRef().Validate(); err != nil {
		return err
	}
	if !currencyShape(r.Currency) {
		return fmt.Errorf("currency must be an ISO 4217 code (three uppercase letters)")
	}
	if !validSourceRunStatuses[r.SourceRunStatus] {
		return fmt.Errorf("source_run_status must be one of DRAFT, CALCULATED, IN_REVIEW, APPROVED, FINAL, CANCELLED")
	}
	if r.InputBatchID != nil {
		if _, err := uuid.Parse(*r.InputBatchID); err != nil {
			return fmt.Errorf("input_batch_id must be a UUID")
		}
	}
	if len(r.EvidenceFlags) > 10 {
		return fmt.Errorf("evidence_flags allows at most 10 entries")
	}
	for _, f := range r.EvidenceFlags {
		if strings.TrimSpace(f) == "" || len(f) > 64 {
			return fmt.Errorf("evidence_flags entries must be 1 to 64 characters")
		}
	}
	if len(r.Workers) > MaxResultWorkers {
		return fmt.Errorf("a result carries at most %d workers", MaxResultWorkers)
	}
	for i, w := range r.Workers {
		at := fmt.Sprintf("workers[%d]", i)
		if _, err := uuid.Parse(w.WorkerID); err != nil {
			return fmt.Errorf("%s.worker_id must be a UUID", at)
		}
		if !currencyShape(w.Currency) {
			return fmt.Errorf("%s.currency must be an ISO 4217 code", at)
		}
		for name, v := range map[string]string{
			"gross": w.Gross, "taxable_pay": w.TaxablePay, "employee_deductions": w.EmployeeDeductions, "employer_liabilities": w.EmployerLiabilities,
		} {
			if err := nonNegDecimal(at+"."+name, v, false); err != nil {
				return err
			}
		}
		// A negative net pay is representable so it can be REJECTED with evidence
		// (NEGATIVE_NET_PAY) instead of being unparseable.
		if err := nonNegDecimal(at+".net_pay", w.NetPay, true); err != nil {
			return err
		}
		if len(w.Components) > maxComponentsPerRow {
			return fmt.Errorf("%s.components allows at most %d entries", at, maxComponentsPerRow)
		}
		for j, c := range w.Components {
			cat := fmt.Sprintf("%s.components[%d]", at, j)
			if strings.TrimSpace(c.Code) == "" {
				return fmt.Errorf("%s.code is required", cat)
			}
			if !validKind(c.Kind) {
				return fmt.Errorf("%s.kind %q is not a known component kind", cat, c.Kind)
			}
			if err := nonNegDecimal(cat+".amount", c.Amount, false); err != nil {
				return err
			}
		}
	}
	t := r.RunTotals
	if t.WorkerCount < 0 {
		return fmt.Errorf("run_totals.worker_count must not be negative")
	}
	for name, v := range map[string]string{
		"gross": t.Gross, "taxable_pay": t.TaxablePay, "employee_deductions": t.EmployeeDeductions, "employer_liabilities": t.EmployerLiabilities,
	} {
		if err := nonNegDecimal("run_totals."+name, v, false); err != nil {
			return err
		}
	}
	return nonNegDecimal("run_totals.net_pay", t.NetPay, true)
}

// ── evaluation ───────────────────────────────────────────────────────────────

// ResultWorkerContext is employee-master-svc's verdict for one worker.
type ResultWorkerContext struct {
	Valid         bool
	Reason        string
	PayCurrencies []string
}

type ResultReason struct {
	Code     string `json:"code"`
	WorkerID string `json:"worker_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type ComputedTotals struct {
	WorkerCount         int    `json:"worker_count"`
	Gross               string `json:"gross"`
	TaxablePay          string `json:"taxable_pay"`
	EmployeeDeductions  string `json:"employee_deductions"`
	EmployerLiabilities string `json:"employer_liabilities"`
	NetPay              string `json:"net_pay"`
}

// NormalizedWorker is a worker row with every amount in canonical 4-decimal form,
// ready to persist.
type NormalizedWorker struct {
	WorkerID            string
	Currency            string
	Gross, TaxablePay   string
	EmployeeDeductions  string
	EmployerLiabilities string
	NetPay              string
	Components          []ResultComponentInput
}

type ResultEvaluation struct {
	Reasons     []ResultReason
	Totals      ComputedTotals
	Workers     []NormalizedWorker
	Fingerprint string
}

func (e ResultEvaluation) Accepted() bool { return len(e.Reasons) == 0 }

// EvaluateResult runs every control. It is pure: the worker contexts and the
// input-batch finding are supplied by the caller. inputBatchReason is "" when the
// request names no input batch or the batch is valid.
func EvaluateResult(tenantID string, req ReceivePayrollResultRequest, ctxs map[string]ResultWorkerContext, inputBatchReason string) ResultEvaluation {
	var ev ResultEvaluation
	add := func(code, worker, detail string) {
		ev.Reasons = append(ev.Reasons, ResultReason{Code: code, WorkerID: worker, Detail: detail})
	}

	if !SourceRunIsFinal(req.SourceRunStatus) {
		add(RsRunNotFinal, "", "source_run_status="+req.SourceRunStatus)
	}
	if strings.TrimSpace(req.SourceApprovalRef) == "" {
		add(RsMissingApprovalEvidence, "", "")
	}
	if strings.TrimSpace(req.CalculationRef) == "" {
		add(RsMissingCalculationRef, "", "")
	}
	if len(req.Workers) == 0 {
		add(RsEmptyResult, "", "")
	}
	if inputBatchReason != "" {
		add(RsInputBatchInvalid, "", inputBatchReason)
	}

	zero := func() *big.Rat { return new(big.Rat) }
	gross, taxable, ded, emp, net := zero(), zero(), zero(), zero(), zero()
	seen := map[string]bool{}

	workers := append([]ResultWorkerInput(nil), req.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerID < workers[j].WorkerID })

	for _, w := range workers {
		g, _ := ParseDecimal(w.Gross)
		tp, _ := ParseDecimal(w.TaxablePay)
		d, _ := ParseDecimal(w.EmployeeDeductions)
		el, _ := ParseDecimal(w.EmployerLiabilities)
		n, _ := ParseDecimal(w.NetPay)
		gross.Add(gross, g)
		taxable.Add(taxable, tp)
		ded.Add(ded, d)
		emp.Add(emp, el)
		net.Add(net, n)

		if seen[w.WorkerID] {
			add(RsDuplicateWorker, w.WorkerID, "")
		}
		seen[w.WorkerID] = true

		if c, ok := ctxs[w.WorkerID]; !ok || !c.Valid {
			reason := RsWorkerContext
			if ok && c.Reason != "" {
				reason = c.Reason
			}
			add(RsWorkerContext, w.WorkerID, reason)
		} else {
			switch len(c.PayCurrencies) {
			case 0:
			case 1:
				if c.PayCurrencies[0] != w.Currency {
					add(RsCurrencyMismatch, w.WorkerID, "worker pay currency "+c.PayCurrencies[0]+", result "+w.Currency)
				}
			default:
				add(RsAmbiguousPayCurrency, w.WorkerID, "")
			}
		}
		if w.Currency != req.Currency {
			add(RsCurrencyMismatch, w.WorkerID, "result currency "+req.Currency+", worker row "+w.Currency)
		}

		earn, empSide, erSide := zero(), zero(), zero()
		nw := NormalizedWorker{
			WorkerID: w.WorkerID, Currency: w.Currency, Gross: FormatDecimal(g), TaxablePay: FormatDecimal(tp),
			EmployeeDeductions: FormatDecimal(d), EmployerLiabilities: FormatDecimal(el), NetPay: FormatDecimal(n),
		}
		for _, c := range w.Components {
			a, _ := ParseDecimal(c.Amount)
			switch {
			case c.Kind == KindEarning:
				earn.Add(earn, a)
			case employeeKinds[c.Kind]:
				empSide.Add(empSide, a)
			case employerKinds[c.Kind]:
				erSide.Add(erSide, a)
			}
			nw.Components = append(nw.Components, ResultComponentInput{Code: c.Code, Kind: c.Kind, Amount: FormatDecimal(a), AuthorityRef: c.AuthorityRef})
		}
		ev.Workers = append(ev.Workers, nw)

		if earn.Cmp(g) != 0 {
			add(RsGrossNotEarnings, w.WorkerID, fmt.Sprintf("earnings %s, gross %s", FormatDecimal(earn), FormatDecimal(g)))
		}
		if empSide.Cmp(d) != 0 {
			add(RsDeductionsMismatch, w.WorkerID, fmt.Sprintf("components %s, declared %s", FormatDecimal(empSide), FormatDecimal(d)))
		}
		if erSide.Cmp(el) != 0 {
			add(RsEmployerMismatch, w.WorkerID, fmt.Sprintf("components %s, declared %s", FormatDecimal(erSide), FormatDecimal(el)))
		}
		expected := new(big.Rat).Sub(g, d)
		if expected.Sign() < 0 {
			add(RsNegativeNetPay, w.WorkerID, "gross-to-net is negative: "+FormatDecimal(expected))
		} else if expected.Cmp(n) != 0 {
			add(RsNetPayMismatch, w.WorkerID, fmt.Sprintf("gross-deductions %s, declared net %s", FormatDecimal(expected), FormatDecimal(n)))
		}
	}

	ev.Totals = ComputedTotals{
		WorkerCount: len(workers), Gross: FormatDecimal(gross), TaxablePay: FormatDecimal(taxable),
		EmployeeDeductions: FormatDecimal(ded), EmployerLiabilities: FormatDecimal(emp), NetPay: FormatDecimal(net),
	}
	t := req.RunTotals
	if t.WorkerCount != ev.Totals.WorkerCount {
		add(RsRunTotalMismatch, "", fmt.Sprintf("worker_count declared %d, workers %d", t.WorkerCount, ev.Totals.WorkerCount))
	}
	for _, f := range []struct{ name, declared, computed string }{
		{"gross", t.Gross, ev.Totals.Gross}, {"taxable_pay", t.TaxablePay, ev.Totals.TaxablePay},
		{"employee_deductions", t.EmployeeDeductions, ev.Totals.EmployeeDeductions},
		{"employer_liabilities", t.EmployerLiabilities, ev.Totals.EmployerLiabilities}, {"net_pay", t.NetPay, ev.Totals.NetPay},
	} {
		if normDec(f.declared) != f.computed {
			add(RsRunTotalMismatch, "", fmt.Sprintf("%s declared %s, workers sum %s", f.name, normDec(f.declared), f.computed))
		}
	}

	ev.Fingerprint = ResultFingerprint(tenantID, req)
	return ev
}

// ResultFingerprint is the identity of a result's content (spec 2.2): the same
// submission always has the same fingerprint, however its amounts are written
// ("100" and "100.00" are one figure) and in whatever order workers and
// components arrive. Any material change changes it.
func ResultFingerprint(tenantID string, req ReceivePayrollResultRequest) string {
	h := sha256.New()
	put := func(s string) { fmt.Fprintf(h, "%d:%s|", len(s), s) }
	deref := func(p *string) string {
		if p == nil {
			return "\x00nil"
		}
		return *p
	}
	for _, f := range []string{
		tenantID, req.LegalEntityID, req.SourceSystemID, req.SourceRunID, fmt.Sprint(req.SourceRunVersion), req.RunKind,
		req.PayrollPeriodID, req.PayDate, req.RuleManifestID, req.SourceRunStatus, req.SourceApprovalRef, req.CalculationRef,
		req.Currency, deref(req.InputBatchID),
		normDec(req.RunTotals.Gross), normDec(req.RunTotals.TaxablePay), normDec(req.RunTotals.EmployeeDeductions),
		normDec(req.RunTotals.EmployerLiabilities), normDec(req.RunTotals.NetPay), fmt.Sprint(req.RunTotals.WorkerCount),
	} {
		put(f)
	}
	flags := append([]string(nil), req.EvidenceFlags...)
	sort.Strings(flags)
	for _, f := range flags {
		put("flag:" + f)
	}
	workers := append([]ResultWorkerInput(nil), req.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerID < workers[j].WorkerID })
	for _, w := range workers {
		for _, f := range []string{
			w.WorkerID, w.Currency, normDec(w.Gross), normDec(w.TaxablePay), normDec(w.EmployeeDeductions),
			normDec(w.EmployerLiabilities), normDec(w.NetPay),
		} {
			put(f)
		}
		comps := make([]string, 0, len(w.Components))
		for _, c := range w.Components {
			comps = append(comps, c.Kind+"|"+c.Code+"|"+normDec(c.Amount)+"|"+deref(c.AuthorityRef))
		}
		sort.Strings(comps)
		for _, c := range comps {
			put("c:" + c)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── persisted objects ────────────────────────────────────────────────────────

type PayrollResult struct {
	ResultID                 string            `json:"result_id"`
	TenantID                 string            `json:"tenant_id"`
	RunRefID                 string            `json:"run_ref_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	Outcome                  string            `json:"outcome"`
	RejectionReasons         []ResultReason    `json:"rejection_reasons"`
	Currency                 string            `json:"currency"`
	SourceRunStatus          string            `json:"source_run_status"`
	SourceApprovalRef        *string           `json:"source_approval_ref,omitempty"`
	CalculationRef           *string           `json:"calculation_ref,omitempty"`
	InputBatchID             *string           `json:"input_batch_id,omitempty"`
	EvidenceFlags            []string          `json:"evidence_flags"`
	WorkerCount              int               `json:"worker_count"`
	TotalGross               string            `json:"total_gross"`
	TotalTaxablePay          string            `json:"total_taxable_pay"`
	TotalEmployeeDeductions  string            `json:"total_employee_deductions"`
	TotalEmployerLiabilities string            `json:"total_employer_liabilities"`
	TotalNetPay              string            `json:"total_net_pay"`
	DeclaredTotals           ResultTotalsInput `json:"declared_totals"`
	ResultFingerprint        string            `json:"result_fingerprint"`
	ReceivedBy               string            `json:"received_by"`
	CorrelationID            string            `json:"correlation_id"`
	CreatedAt                time.Time         `json:"created_at"`
}

type PayrollResultWorker struct {
	WorkerID            string `json:"worker_id"`
	Gross               string `json:"gross"`
	TaxablePay          string `json:"taxable_pay"`
	EmployeeDeductions  string `json:"employee_deductions"`
	EmployerLiabilities string `json:"employer_liabilities"`
	NetPay              string `json:"net_pay"`
	Currency            string `json:"currency"`
}

type PayrollResultComponent struct {
	ComponentID  string  `json:"component_id"`
	WorkerID     string  `json:"worker_id"`
	Sequence     int     `json:"sequence"`
	Code         string  `json:"code"`
	Kind         string  `json:"kind"`
	Amount       string  `json:"amount"`
	AuthorityRef *string `json:"authority_ref,omitempty"`
}

type PayrollCorrectionChain struct {
	ChainID             string    `json:"chain_id"`
	OriginalRunRefID    string    `json:"original_run_ref_id"`
	SupersedingRunRefID string    `json:"superseding_run_ref_id"`
	OriginalResultID    string    `json:"original_result_id"`
	SupersedingResultID string    `json:"superseding_result_id"`
	Reason              string    `json:"reason"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
}

// ── Zoiko path ───────────────────────────────────────────────────────────────

// ZoikoResultParams are the caller's choices when promoting a run to a result.
type ZoikoResultParams struct {
	PayrollPeriodID  string
	RuleManifestID   string
	SourceRunID      string // the run's own id, or the ORIGINAL run's id for a correction
	SourceRunVersion int
	RunKind          string
	InputBatchID     *string
}

// BuildZoikoResultRequest builds a WFP-04 submission from a COMPLETED Zoiko run
// and its payslips. Nothing is recalculated and nothing is repaired: the figures
// are the run's, written as exact decimals; whatever does not reconcile is left
// for the controls to reject, and what the calculation does not establish is
// flagged rather than implied.
func BuildZoikoResultRequest(run PayrollRun, slips []PaySlip, p ZoikoResultParams) (ReceivePayrollResultRequest, error) {
	if run.IsShadowRun {
		return ReceivePayrollResultRequest{}, ErrShadowRunNotPayable
	}
	if run.Status != "COMPLETED" {
		return ReceivePayrollResultRequest{}, ErrRunNotFinal
	}

	approval := ""
	switch {
	case run.GovernanceDecisionID != nil && strings.TrimSpace(*run.GovernanceDecisionID) != "":
		approval = "governance:" + *run.GovernanceDecisionID
	case run.SnapshotHash != nil && *run.SnapshotHash != "":
		approval = "finalized:" + *run.SnapshotHash
	}

	currency := ""
	if len(slips) > 0 {
		currency = slips[0].Currency
	}
	sorted := append([]PaySlip(nil), slips...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].EmployeeID < sorted[j].EmployeeID })

	dec := func(f float64) *big.Rat {
		text, _, _ := FormatAmount(f)
		r, _ := ParseDecimal(text)
		return r
	}
	req := ReceivePayrollResultRequest{
		LegalEntityID: run.LegalEntityID, SourceSystemID: SourceZoikoPayroll, SourceRunID: p.SourceRunID,
		SourceRunVersion: p.SourceRunVersion, RunKind: p.RunKind, PayrollPeriodID: p.PayrollPeriodID, PayDate: run.PayDate,
		RuleManifestID: p.RuleManifestID, SourceRunStatus: "FINAL", SourceApprovalRef: approval,
		CalculationRef: ZoikoCalculationRef, Currency: currency, InputBatchID: p.InputBatchID, PayrollRunID: &run.RunID,
		EvidenceFlags: []string{FlagPlaceholderTaxRate, FlagEmployerLiabilitiesNotComputed},
	}

	taxable := new(big.Rat)
	for _, s := range sorted {
		gross, tax, ben, net, tp := dec(s.GrossPay), dec(s.TaxWithheld), dec(s.BenefitsDeductions), dec(s.NetPay), dec(s.TaxableAmount)
		taxable.Add(taxable, tp)

		w := ResultWorkerInput{
			WorkerID: s.EmployeeID, Currency: s.Currency, Gross: FormatDecimal(gross), TaxablePay: FormatDecimal(tp),
			EmployeeDeductions: FormatDecimal(new(big.Rat).Add(tax, ben)), EmployerLiabilities: FormatDecimal(new(big.Rat)),
			NetPay: FormatDecimal(net),
		}
		earned := new(big.Rat)
		for _, it := range s.Items {
			a := dec(it.Amount)
			kind := KindEarning
			if it.ComponentType == "DEDUCTION" {
				kind = KindEmployeeDeduction
			} else {
				earned.Add(earned, a)
			}
			w.Components = append(w.Components, ResultComponentInput{Code: it.ComponentCode, Kind: kind, Amount: FormatDecimal(a)})
		}
		// Base pay is what remains of gross once the component earnings are
		// accounted for. If components exceed gross the base is left at zero and
		// the gross-vs-earnings control rejects the result; nothing is clamped quietly.
		base := new(big.Rat).Sub(gross, earned)
		if base.Sign() < 0 {
			base = new(big.Rat)
		}
		w.Components = append(w.Components, ResultComponentInput{Code: "BASE", Kind: KindEarning, Amount: FormatDecimal(base)})
		if tax.Sign() > 0 {
			w.Components = append(w.Components, ResultComponentInput{Code: "TAX_WITHHELD", Kind: KindEmployeeTax, Amount: FormatDecimal(tax)})
		}
		req.Workers = append(req.Workers, w)
	}

	req.RunTotals = ResultTotalsInput{
		WorkerCount: run.EmployeeCount, Gross: FormatDecimal(dec(run.TotalGrossPay)), TaxablePay: FormatDecimal(taxable),
		EmployeeDeductions:  FormatDecimal(new(big.Rat).Add(dec(run.TotalTaxDeductions), dec(run.TotalOtherDeductions))),
		EmployerLiabilities: FormatDecimal(new(big.Rat)), NetPay: FormatDecimal(dec(run.TotalNetPay)),
	}
	return req, nil
}

// ── events & errors ──────────────────────────────────────────────────────────

var (
	ErrResultNotFound            = errorString("payroll result not found")
	ErrResultFingerprintConflict = errorString("this source run version already has a result with different content; a changed result must use a new source_run_version")
	ErrRunNotFinal               = errorString("the payroll run is not final; only a COMPLETED run can be promoted to a result")
	ErrShadowRunNotPayable       = errorString("a shadow run is a comparison, not a payroll result")
	ErrTooManyResultWorkers      = errorString("the result carries more workers than one submission supports")
	ErrCorrectionBaseMissing     = errorString("the run being corrected has no registered result")
)
