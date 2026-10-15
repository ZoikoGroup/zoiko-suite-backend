package domain

// ZS-SVC-L-001 WFP-06 — Payroll Liability Interface.
//
// A liability is what payroll says is owed to a third party (tax authority, social
// insurer, pension or benefit provider, garnishment beneficiary) from an ACCEPTED
// result. WFP-06 does not decide the amount (the payroll engine did), does not move
// money, and never marks a liability paid from payment INTENT: settlement needs
// Banking evidence. Recognition is driven by a governed, effective-dated registry
// that names, per component kind and code, the class, the authority (a counterparty),
// the jurisdiction and the due-date rule. A component the registry does not cover is
// an exception, never a guess.

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	LiabRecognized       = "RECOGNIZED"
	LiabDue              = "DUE"
	LiabScheduled        = "SCHEDULED"
	LiabPartiallySettled = "PARTIALLY_SETTLED"
	LiabSettled          = "SETTLED"
	LiabReconciled       = "RECONCILED"
	LiabSuperseded       = "SUPERSEDED"

	// Exception state is orthogonal to lifecycle state: a liability can be DUE and
	// DISPUTED at once, and a liability under review cannot be scheduled.
	LiabExcNone           = "NONE"
	LiabExcReviewRequired = "REVIEW_REQUIRED"
	LiabExcDisputed       = "DISPUTED"

	SideEmployeeWithheld = "EMPLOYEE_WITHHELD"
	SideEmployer         = "EMPLOYER"

	DueDayOfFollowingMonth = "DAY_OF_FOLLOWING_MONTH"
	DueDaysAfterPayDate    = "DAYS_AFTER_PAY_DATE"

	// Exception codes. Stable: runbooks key off them.
	ExcUnmappedClass       = "UNMAPPED_LIABILITY_CLASS"
	ExcClassConflict       = "LIABILITY_CLASS_CONFLICT"
	ExcMissingExpected     = "MISSING_EXPECTED_LIABILITY"
	ExcEmployerNotComputed = "EMPLOYER_LIABILITIES_NOT_COMPUTED"
	ExcAuthorityInvalid    = "LIABILITY_AUTHORITY_INVALID"
	ExcGLMismatch          = "LIABILITY_GL_MISMATCH"

	ExcOpen     = "OPEN"
	ExcResolved = "RESOLVED"

	AmountDeclared         = "DECLARED_NOT_BANK_VERIFIED"
	AmountInstructionBound = "INSTRUCTION_BOUND"
)

const (
	EventPayrollLiabilityDue                  = "payroll_liability_due"
	EventPayrollLiabilityScheduled            = "payroll_liability_scheduled"
	EventPayrollLiabilityAdjusted             = "payroll_liability_adjusted"
	EventPayrollLiabilityDisputed             = "payroll_liability_disputed"
	EventPayrollLiabilityReconciliationFailed = "payroll_liability_reconciliation_failed"
)

var liabilityTypes = map[string]bool{"TAX": true, "SOCIAL": true, "PENSION": true, "BENEFIT": true, "GARNISHMENT": true, "OTHER": true}

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// ComponentKindIsLiability reports whether a result component of this kind can
// create a liability (everything except earnings).
func ComponentKindIsLiability(kind string) bool {
	return kind != KindEarning && validKind(kind)
}

// PartySideOf says whether a kind is withheld from the employee or borne by the employer.
func PartySideOf(kind string) string {
	if employerKinds[kind] {
		return SideEmployer
	}
	return SideEmployeeWithheld
}

// PayableKeyOf is the Wave 5 ledger mapping key the liability of a component kind
// posts to; reconciliation compares liabilities with those posted credits.
func PayableKeyOf(kind string) string {
	if employerKinds[kind] {
		return EmployerPayableKey(kind)
	}
	return EmployeePayableKey(kind)
}

// ── registry ─────────────────────────────────────────────────────────────────

type LiabilityClass struct {
	ClassRowID              string    `json:"class_row_id"`
	TenantID                string    `json:"tenant_id"`
	LegalEntityID           string    `json:"legal_entity_id"`
	ComponentKind           string    `json:"component_kind"`
	ComponentCode           string    `json:"component_code"`
	ClassVersion            int       `json:"class_version"`
	ClassCode               string    `json:"class_code"`
	LiabilityType           string    `json:"liability_type"`
	AuthorityCounterpartyID string    `json:"authority_counterparty_id"`
	JurisdictionID          string    `json:"jurisdiction_id"`
	DueRuleKind             string    `json:"due_rule_kind"`
	DueRuleValue            int       `json:"due_rule_value"`
	Required                bool      `json:"required"`
	Active                  bool      `json:"active"`
	RuleRef                 string    `json:"rule_ref"`
	EffectiveFrom           string    `json:"effective_from"`
	RegisteredBy            string    `json:"registered_by"`
	CreatedAt               time.Time `json:"created_at"`
}

// RegisterLiabilityClassRequest deliberately has no tenant_id (WFP-NP-001).
type RegisterLiabilityClassRequest struct {
	LegalEntityID           string `json:"legal_entity_id"`
	ComponentKind           string `json:"component_kind"`
	ComponentCode           string `json:"component_code"`
	ClassCode               string `json:"class_code"`
	LiabilityType           string `json:"liability_type"`
	AuthorityCounterpartyID string `json:"authority_counterparty_id"`
	JurisdictionID          string `json:"jurisdiction_id"`
	DueRuleKind             string `json:"due_rule_kind"`
	DueRuleValue            int    `json:"due_rule_value"`
	Required                bool   `json:"required"`
	Active                  *bool  `json:"active,omitempty"`
	RuleRef                 string `json:"rule_ref"`
	EffectiveFrom           string `json:"effective_from"`
}

func (r *RegisterLiabilityClassRequest) Validate() error {
	if r.ComponentCode == "" {
		r.ComponentCode = "*"
	}
	for name, v := range map[string]string{
		"legal_entity_id": r.LegalEntityID, "class_code": r.ClassCode, "authority_counterparty_id": r.AuthorityCounterpartyID,
		"jurisdiction_id": r.JurisdictionID, "rule_ref": r.RuleRef,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !ComponentKindIsLiability(r.ComponentKind) {
		return fmt.Errorf("component_kind %q cannot create a liability", r.ComponentKind)
	}
	if r.ComponentCode != "*" && !codeRE.MatchString(r.ComponentCode) {
		return fmt.Errorf("component_code must be '*' or 1 to 64 of letters, digits, '_', '.', ':', '-'")
	}
	if !codeRE.MatchString(r.ClassCode) {
		return fmt.Errorf("class_code must be 1 to 64 of letters, digits, '_', '.', ':', '-'")
	}
	if !liabilityTypes[r.LiabilityType] {
		return fmt.Errorf("liability_type must be one of TAX, SOCIAL, PENSION, BENEFIT, GARNISHMENT, OTHER")
	}
	if err := ValidateDueRule(r.DueRuleKind, r.DueRuleValue); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", r.EffectiveFrom); err != nil {
		return fmt.Errorf("effective_from must be a date (YYYY-MM-DD)")
	}
	return nil
}

// ── due dates ────────────────────────────────────────────────────────────────

func ValidateDueRule(kind string, value int) error {
	switch kind {
	case DueDayOfFollowingMonth:
		if value < 1 || value > 28 {
			return fmt.Errorf("due_rule_value must be 1 to 28 for %s", kind)
		}
	case DueDaysAfterPayDate:
		if value < 0 || value > 120 {
			return fmt.Errorf("due_rule_value must be 0 to 120 for %s", kind)
		}
	default:
		return fmt.Errorf("due_rule_kind must be %s or %s", DueDayOfFollowingMonth, DueDaysAfterPayDate)
	}
	return nil
}

// DueDate applies the registry's due-date rule to the pay date. Day-of-month rules
// stop at 28 so every month has the day; the provenance (rule kind and value) is
// stored on the liability next to the date.
func DueDate(payDate, kind string, value int) (string, error) {
	if err := ValidateDueRule(kind, value); err != nil {
		return "", err
	}
	d, err := time.Parse("2006-01-02", payDate)
	if err != nil {
		return "", fmt.Errorf("pay date %q is not a date", payDate)
	}
	switch kind {
	case DueDayOfFollowingMonth:
		first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		return time.Date(first.Year(), first.Month(), value, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), nil
	default:
		return d.AddDate(0, 0, value).Format("2006-01-02"), nil
	}
}

// ── classification and the recognition plan ──────────────────────────────────

// ResolveClass finds the class row governing a component on the pay date: the
// latest row effective on that date for the exact code, else for '*'. If that
// latest row is retired (inactive) the component is UNMAPPED, not silently mapped
// to an older row.
func ResolveClass(classes []LiabilityClass, kind, code, payDate string) (*LiabilityClass, bool) {
	pick := func(wantCode string) (*LiabilityClass, bool) {
		var best *LiabilityClass
		found := false
		for i := range classes {
			c := &classes[i]
			if c.ComponentKind != kind || c.ComponentCode != wantCode || c.EffectiveFrom > payDate {
				continue
			}
			found = true
			if best == nil || c.EffectiveFrom > best.EffectiveFrom {
				best = c
			}
		}
		return best, found
	}
	if c, found := pick(code); found {
		if !c.Active {
			return nil, false
		}
		return c, true
	}
	if c, found := pick("*"); found && c.Active {
		return c, true
	}
	return nil, false
}

// LiabilityDraft is one liability the plan would recognize.
type LiabilityDraft struct {
	Class           LiabilityClass
	PartySide       string
	ComponentKinds  []string
	KindAmounts     map[string]*big.Rat
	AuthorityLabels []string
	SourceCount     int
	Amount          *big.Rat
	DueDate         string
}

// ExceptionDraft is one exception the plan raises.
type ExceptionDraft struct {
	Code       string
	SubjectKey string
	Amount     *string
	Detail     string
}

type RecognitionPlan struct {
	Liabilities []LiabilityDraft
	Exceptions  []ExceptionDraft
}

type PlanInput struct {
	PayDate    string
	Flags      []string
	Components []PayrollResultComponent
	Classes    []LiabilityClass
}

// PlanRecognition turns the accepted result's components into liabilities and
// exceptions. It is pure and deterministic: the same result and registry give the
// same plan, so recognition is idempotent. Liabilities group by class code +
// authority + liability type; rows of one group that disagree on side, jurisdiction
// or due rule are a CLASS_CONFLICT exception instead of a guess.
func PlanRecognition(in PlanInput) (RecognitionPlan, error) {
	type group struct {
		first   LiabilityClass
		side    string
		kinds   map[string]bool
		byKind  map[string]*big.Rat
		labels  map[string]bool
		sum     *big.Rat
		count   int
		conflct bool
	}
	groups := map[string]*group{}
	var order []string
	unmapped := map[string]*big.Rat{}
	present := map[string]bool{} // kind + "\x00" + code of every component, for completeness

	for _, c := range in.Components {
		if !ComponentKindIsLiability(c.Kind) {
			continue
		}
		amt, err := ParseDecimal(c.Amount)
		if err != nil {
			return RecognitionPlan{}, fmt.Errorf("component %s: %w", c.ComponentID, err)
		}
		present[c.Kind+"\x00"+c.Code] = true
		cls, ok := ResolveClass(in.Classes, c.Kind, c.Code, in.PayDate)
		if !ok {
			key := c.Kind + ":" + c.Code
			if unmapped[key] == nil {
				unmapped[key] = new(big.Rat)
			}
			unmapped[key].Add(unmapped[key], amt)
			continue
		}
		side := PartySideOf(c.Kind)
		gk := cls.ClassCode + "|" + cls.AuthorityCounterpartyID + "|" + cls.LiabilityType
		g := groups[gk]
		if g == nil {
			g = &group{first: *cls, side: side, kinds: map[string]bool{}, byKind: map[string]*big.Rat{}, labels: map[string]bool{}, sum: new(big.Rat)}
			groups[gk] = g
			order = append(order, gk)
		} else if g.side != side || g.first.JurisdictionID != cls.JurisdictionID ||
			g.first.DueRuleKind != cls.DueRuleKind || g.first.DueRuleValue != cls.DueRuleValue {
			g.conflct = true
		}
		g.kinds[c.Kind] = true
		if g.byKind[c.Kind] == nil {
			g.byKind[c.Kind] = new(big.Rat)
		}
		g.byKind[c.Kind].Add(g.byKind[c.Kind], amt)
		if c.AuthorityRef != nil && strings.TrimSpace(*c.AuthorityRef) != "" {
			g.labels[*c.AuthorityRef] = true
		}
		g.sum.Add(g.sum, amt)
		g.count++
	}

	var plan RecognitionPlan
	sort.Strings(order)
	for _, gk := range order {
		g := groups[gk]
		if g.conflct {
			total := FormatDecimal(g.sum)
			plan.Exceptions = append(plan.Exceptions, ExceptionDraft{Code: ExcClassConflict, SubjectKey: gk, Amount: &total,
				Detail: "components of one liability class disagree on side, jurisdiction or due-date rule; correct the registry"})
			continue
		}
		if g.sum.Sign() == 0 {
			continue // nothing owed; present, so completeness is satisfied
		}
		due, err := DueDate(in.PayDate, g.first.DueRuleKind, g.first.DueRuleValue)
		if err != nil {
			return RecognitionPlan{}, err
		}
		kinds := keys(g.kinds)
		plan.Liabilities = append(plan.Liabilities, LiabilityDraft{
			Class: g.first, PartySide: g.side, ComponentKinds: kinds, KindAmounts: g.byKind, AuthorityLabels: keys(g.labels),
			SourceCount: g.count, Amount: g.sum, DueDate: due,
		})
	}

	ukeys := make([]string, 0, len(unmapped))
	for k := range unmapped {
		ukeys = append(ukeys, k)
	}
	sort.Strings(ukeys)
	for _, k := range ukeys {
		total := FormatDecimal(unmapped[k])
		plan.Exceptions = append(plan.Exceptions, ExceptionDraft{Code: ExcUnmappedClass, SubjectKey: k, Amount: &total,
			Detail: "no active liability class covers this component on the pay date; register one and recognize again"})
	}

	// Completeness (WFP-CTRL-014): a class the entity marked required that the result
	// does not carry at all. For a '*' row any component of the kind satisfies it.
	seen := map[string]bool{}
	for _, c := range in.Classes {
		if !c.Required || !c.Active || c.EffectiveFrom > in.PayDate {
			continue
		}
		cur, ok := latestRow(in.Classes, c.ComponentKind, c.ComponentCode, in.PayDate)
		if !ok || !cur.Required || !cur.Active {
			continue
		}
		sk := cur.ComponentKind + ":" + cur.ComponentCode
		if seen[sk] {
			continue
		}
		seen[sk] = true
		if !classPresent(present, cur) {
			plan.Exceptions = append(plan.Exceptions, ExceptionDraft{Code: ExcMissingExpected, SubjectKey: sk,
				Detail: fmt.Sprintf("class %s is required for this entity but the result carries no %s component", cur.ClassCode, sk)})
		}
	}

	for _, f := range in.Flags {
		if f == FlagEmployerLiabilitiesNotComputed {
			plan.Exceptions = append(plan.Exceptions, ExceptionDraft{Code: ExcEmployerNotComputed, SubjectKey: "RESULT",
				Detail: "the payroll source did not compute employer liabilities for this result; employer-side liabilities are not recognized and must be added by a corrected result"})
		}
	}
	sort.SliceStable(plan.Exceptions, func(i, j int) bool {
		a, b := plan.Exceptions[i], plan.Exceptions[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.SubjectKey < b.SubjectKey
	})
	return plan, nil
}

func latestRow(classes []LiabilityClass, kind, code, payDate string) (LiabilityClass, bool) {
	var best LiabilityClass
	found := false
	for _, c := range classes {
		if c.ComponentKind == kind && c.ComponentCode == code && c.EffectiveFrom <= payDate &&
			(!found || c.EffectiveFrom > best.EffectiveFrom) {
			best, found = c, true
		}
	}
	return best, found
}

func classPresent(present map[string]bool, c LiabilityClass) bool {
	for k := range present {
		parts := strings.SplitN(k, "\x00", 2)
		if parts[0] == c.ComponentKind && (c.ComponentCode == "*" || parts[1] == c.ComponentCode) {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── liabilities ──────────────────────────────────────────────────────────────

type PayrollLiability struct {
	LiabilityID             string                `json:"liability_id"`
	TenantID                string                `json:"tenant_id"`
	LegalEntityID           string                `json:"legal_entity_id"`
	RunRefID                string                `json:"run_ref_id"`
	ResultID                string                `json:"result_id"`
	PayrollPeriodID         string                `json:"payroll_period_id"`
	ClassCode               string                `json:"class_code"`
	ClassRowID              string                `json:"class_row_id"`
	LiabilityType           string                `json:"liability_type"`
	PartySide               string                `json:"party_side"`
	AuthorityCounterpartyID string                `json:"authority_counterparty_id"`
	JurisdictionID          string                `json:"jurisdiction_id"`
	RuleRef                 string                `json:"rule_ref"`
	ComponentKinds          []string              `json:"component_kinds"`
	SourceComponentCount    int                   `json:"source_component_count"`
	AuthorityLabels         []string              `json:"authority_labels"`
	KindAmounts             map[string]string     `json:"kind_amounts"`
	Amount                  string                `json:"amount"`
	Currency                string                `json:"currency"`
	DueDate                 string                `json:"due_date"`
	DueRuleKind             string                `json:"due_rule_kind"`
	DueRuleValue            int                   `json:"due_rule_value"`
	State                   string                `json:"state"`
	ExceptionState          string                `json:"exception_state"`
	SettledAmount           string                `json:"settled_amount"`
	AdjustmentTotal         string                `json:"adjustment_total"`
	OutstandingAmount       string                `json:"outstanding_amount"`
	ScheduledFor            *string               `json:"scheduled_for,omitempty"`
	ScheduledBy             *string               `json:"scheduled_by,omitempty"`
	DisputeReason           *string               `json:"dispute_reason,omitempty"`
	ReconciliationState     string                `json:"reconciliation_state"`
	ReconciledAt            *time.Time            `json:"reconciled_at,omitempty"`
	ReconciliationDetail    []byte                `json:"reconciliation_detail,omitempty"`
	Version                 int                   `json:"version"`
	CorrelationID           string                `json:"correlation_id"`
	CreatedBy               string                `json:"created_by"`
	CreatedAt               time.Time             `json:"created_at"`
	UpdatedAt               time.Time             `json:"updated_at"`
	Settlements             []LiabilitySettlement `json:"settlements,omitempty"`
	Adjustments             []LiabilityAdjustment `json:"adjustments,omitempty"`
}

// Outstanding is amount + adjustments - settled, exact.
func Outstanding(amount, adjustments, settled string) (string, error) {
	a, err := ParseDecimal(amount)
	if err != nil {
		return "", err
	}
	adj, err := ParseDecimal(adjustments)
	if err != nil {
		return "", err
	}
	s, err := ParseDecimal(settled)
	if err != nil {
		return "", err
	}
	o := new(big.Rat).Add(a, adj)
	return FormatDecimal(o.Sub(o, s)), nil
}

type LiabilitySettlement struct {
	SettlementID   string    `json:"settlement_id"`
	LiabilityID    string    `json:"liability_id"`
	PaymentID      string    `json:"payment_id"`
	Amount         string    `json:"amount"`
	Currency       string    `json:"currency"`
	DeclaredBy     string    `json:"declared_by"`
	BankingStatus  string    `json:"banking_status"`
	FinalitySource *string   `json:"finality_source,omitempty"`
	AmountEvidence string    `json:"amount_evidence"`
	InstructionID  *string   `json:"instruction_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type LiabilityAdjustment struct {
	AdjustmentID string    `json:"adjustment_id"`
	LiabilityID  string    `json:"liability_id"`
	Delta        string    `json:"delta"`
	Reason       string    `json:"reason"`
	EvidenceRef  string    `json:"evidence_ref"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

type LiabilityException struct {
	ExceptionID   string     `json:"exception_id"`
	TenantID      string     `json:"tenant_id"`
	LegalEntityID string     `json:"legal_entity_id"`
	ResultID      string     `json:"result_id"`
	LiabilityID   *string    `json:"liability_id,omitempty"`
	Code          string     `json:"code"`
	SubjectKey    string     `json:"subject_key"`
	Amount        *string    `json:"amount,omitempty"`
	Detail        string     `json:"detail"`
	State         string     `json:"state"`
	Resolution    *string    `json:"resolution,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ── commands ─────────────────────────────────────────────────────────────────

type RecognizeLiabilitiesRequest struct {
	RunRefID string `json:"run_ref_id"`
}

type ScheduleLiabilityRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	ScheduledFor    string `json:"scheduled_for"`
}

type SettleLiabilityRequest struct {
	PaymentID string `json:"payment_id"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	// InstructionID binds the settlement to the approved payment instruction that paid it, so
	// the amount is the approved, fingerprinted figure and not the caller's declaration.
	InstructionID string `json:"instruction_id,omitempty"`
}

func (r SettleLiabilityRequest) Validate() error {
	if strings.TrimSpace(r.PaymentID) == "" {
		return fmt.Errorf("payment_id is required: settlement is evidenced by a Banking payment, never marked by hand")
	}
	a, err := ParseDecimal(r.Amount)
	if err != nil || a.Sign() <= 0 {
		return fmt.Errorf("amount must be a positive decimal with at most 4 places")
	}
	if len(r.Currency) != 3 || r.Currency != strings.ToUpper(r.Currency) {
		return fmt.Errorf("currency must be a 3-letter code")
	}
	return nil
}

type AdjustLiabilityRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Delta           string `json:"delta"`
	Reason          string `json:"reason"`
	EvidenceRef     string `json:"evidence_ref"`
}

func (r AdjustLiabilityRequest) Validate() error {
	d, err := ParseDecimal(r.Delta)
	if err != nil || d.Sign() == 0 {
		return fmt.Errorf("delta must be a non-zero decimal with at most 4 places")
	}
	if strings.TrimSpace(r.Reason) == "" || strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("reason and evidence_ref are required")
	}
	return nil
}

type DisputeLiabilityRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type LiabilityTransitionRequest struct {
	ExpectedVersion int `json:"expected_version"`
}

type SupersedeLiabilitiesRequest struct {
	RunRefID string `json:"run_ref_id"`
}

// ApplySettlement computes the settled total and state after a settlement of
// amount. It refuses to settle beyond what is owed (original amount plus explicit
// adjustments): an overpayment is a recovery question, not a liability state.
func ApplySettlement(owed, settledBefore, amount *big.Rat) (settled *big.Rat, state string, err error) {
	settled = new(big.Rat).Add(settledBefore, amount)
	if settled.Cmp(owed) > 0 {
		return nil, "", ErrOverSettlement
	}
	if settled.Cmp(owed) == 0 {
		return settled, LiabSettled, nil
	}
	return settled, LiabPartiallySettled, nil
}

// ── reconciliation to the ledger postings ────────────────────────────────────

// ReconcileLine compares one payable key's liabilities with what the payroll
// posting credited to it.
type ReconcileLine struct {
	MappingKey  string `json:"mapping_key"`
	Liabilities string `json:"liabilities"`
	Posted      string `json:"posted"`
	Matches     bool   `json:"matches"`
}

// ReconcileKeys compares liability totals per payable key with the posting's
// credits per key. Keys on either side are all compared, so a posted payable with
// no liability (or the reverse) is a difference.
func ReconcileKeys(liab map[string]*big.Rat, posted map[string]*big.Rat) []ReconcileLine {
	set := map[string]bool{}
	for k := range liab {
		set[k] = true
	}
	for k := range posted {
		set[k] = true
	}
	ks := make([]string, 0, len(set))
	for k := range set {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	zero := new(big.Rat)
	out := make([]ReconcileLine, 0, len(ks))
	for _, k := range ks {
		l, p := liab[k], posted[k]
		if l == nil {
			l = zero
		}
		if p == nil {
			p = zero
		}
		out = append(out, ReconcileLine{MappingKey: k, Liabilities: FormatDecimal(l), Posted: FormatDecimal(p), Matches: l.Cmp(p) == 0})
	}
	return out
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrLiabilityNotFound        = errorString("payroll liability not found")
	ErrLiabilityVersionConflict = errorString("stale expected_version: the liability changed since it was read")
	ErrLiabilityState           = errorString("the liability is not in a state that allows this command")
	ErrLiabilityNeedsReview     = errorString("a liability under review or in dispute cannot be scheduled")
	ErrLiabilityResultNotActive = errorString("the payroll result behind this liability is no longer ACCEPTED")
	ErrResultNotSuperseded      = errorString("the payroll result has not been superseded")
	ErrLiabilityHasSettlements  = errorString("a liability with settlements is adjusted, not superseded")
	ErrOverSettlement           = errorString("settlement would exceed what is owed (amount plus adjustments)")
	ErrAdjustBelowSettled       = errorString("adjustment would reduce what is owed below what is already settled")
	ErrSettlementConflict       = errorString("this payment is already applied to the liability with a different amount")
	ErrSettlementCurrency       = errorString("settlement currency differs from the liability currency")
	ErrLiabilityClassConflict   = errorString("a liability class with this identity already exists; register a later effective_from")
	ErrLiabilityClassOrder      = errorString("effective_from must be later than the latest registered row for this component")
	ErrLiabilityClassNotFound   = errorString("liability class not found")
	ErrAuthorityUnavailable     = errorString("counterparty-management-svc could not be reached to validate the authority")
	ErrAuthorityInvalid         = errorString("the authority counterparty does not exist, is inactive, or belongs to another legal entity")
	ErrBankingUnavailable       = errorString("payment-status-svc could not be reached; settlement is unknown, not assumed")
	ErrPaymentNotSettled        = errorString("Banking does not show this payment as SETTLED")
	ErrPaymentNotFound          = errorString("Banking has no record of this payment")
	ErrPaymentEntityMismatch    = errorString("the payment belongs to a different legal entity")
	ErrPaymentInConflict        = errorString("Banking shows an open status conflict on this payment")
	ErrInstructionMismatch      = errorString("the instruction does not match this settlement (liability, amount, currency, state or Banking source reference)")
	ErrNoPostedAccounting       = errorString("the result has no POSTED accounting subject to reconcile against")
)
