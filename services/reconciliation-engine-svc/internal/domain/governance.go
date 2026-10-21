package domain

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Exception governance (A7) and materiality (A4).
//
// Every exception carries, from the moment it is raised, what ZS-CONTROL-001
// Invariant 7 and §21 require of it: an owner role (and later a named owner),
// a category, the assertion it impacts, a severity derived from its exposure
// against the run's pinned materiality (never operator-supplied), a due date
// from the run's pinned SLA, and an append-only lifecycle. Its states are
// ZS-SVC-Z-001's exception dimension: OPEN / INVESTIGATING / REMEDIATED /
// ACCEPTED_RISK / CLOSED.
//
// Only CLOSED resolves an exception, and only a manual match closes one. An
// ACCEPTED_RISK exception stays unresolved: it keeps the run's result at
// VARIANCE (ZS-SVC-Z-001 INV-19 "accepted risk never changes a failed rule to
// PASS"), and it is listed in the run's assessment and in any certification.

// ── Exception lifecycle ─────────────────────────────────────────────────────

const (
	ExceptionOpen          ExceptionStatus = "OPEN"
	ExceptionInvestigating ExceptionStatus = "INVESTIGATING"
	ExceptionRemediated    ExceptionStatus = "REMEDIATED"
	ExceptionAcceptedRisk  ExceptionStatus = "ACCEPTED_RISK"
	ExceptionClosed        ExceptionStatus = "CLOSED"
)

// exceptionEdges is the complete set of legal exception moves. CLOSED is
// reached only through a manual match (applyManualMatch), never through the
// transition command: closing needs evidence that the item reconciles.
var exceptionEdges = map[ExceptionStatus][]ExceptionStatus{
	ExceptionOpen:          {ExceptionInvestigating, ExceptionAcceptedRisk, ExceptionClosed},
	ExceptionInvestigating: {ExceptionRemediated, ExceptionAcceptedRisk, ExceptionClosed},
	// A remediation is recorded in the owning domain; it does not change this
	// run's frozen populations, so the exception stays unresolved until a new
	// run re-tests it (DQC-05 "Re-test"), or investigation resumes.
	ExceptionRemediated:   {ExceptionInvestigating, ExceptionClosed},
	ExceptionAcceptedRisk: {},
	ExceptionClosed:       {},
}

func ValidateExceptionTransition(from, to ExceptionStatus) error {
	for _, a := range exceptionEdges[from] {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("%w: exception %s -> %s", ErrInvalidTransition, from, to)
}

// Unresolved reports whether an exception still stands against its run.
func (s ExceptionStatus) Unresolved() bool { return s != ExceptionClosed }

// Working reports whether an exception is still being worked (it is neither
// resolved nor accepted as risk). A run with a working exception is ExceptionsOpen.
func (s ExceptionStatus) Working() bool { return s != ExceptionClosed && s != ExceptionAcceptedRisk }

// ── Category, assertion, severity ───────────────────────────────────────────

// Categories and assertions are ZS-CONTROL-001 §21's lists.
var exceptionCategories = map[string]bool{
	"MISSING": true, "DUPLICATE": true, "MISMATCH": true, "TIMING": true, "VALUATION": true, "CLASSIFICATION": true,
	"AUTHORIZATION": true, "DATA_QUALITY": true, "EXTERNAL_STATUS": true, "LATE_DATA": true, "CONTROL_EXECUTION_FAILURE": true,
}

var exceptionAssertions = map[string]bool{
	"COMPLETENESS": true, "ACCURACY": true, "EXISTENCE": true, "VALUATION": true, "RIGHTS_OBLIGATIONS": true,
	"CUTOFF": true, "CLASSIFICATION": true, "PRESENTATION": true,
}

// RuleClassification is the category and assertion an automatic exception's
// rule implies. Fixed here, so the same finding is always classified the same.
func RuleClassification(rule string) (category, assertion string) {
	switch rule {
	case RuleUnmatched:
		return "MISSING", "COMPLETENESS"
	case RuleMissingKey:
		return "DATA_QUALITY", "ACCURACY"
	case RuleCurrencyMismatch, RuleAmountTolerance:
		return "MISMATCH", "ACCURACY"
	case RuleDateTolerance:
		return "TIMING", "CUTOFF"
	case RuleBalance:
		return "MISMATCH", "COMPLETENESS"
	}
	return "CONTROL_EXECUTION_FAILURE", "ACCURACY"
}

// Severity is ZS-SVC-Z-001's DQC severity scale.
type Severity string

const (
	SeverityInfo    Severity = "INFO"
	SeverityWarning Severity = "WARNING"
	SeverityError   Severity = "ERROR"
	SeverityBlocker Severity = "BLOCKER"
)

// MaterialityClass is how an exposure compares with the pinned threshold for
// its currency.
type MaterialityClass string

const (
	Material   MaterialityClass = "MATERIAL"
	Immaterial MaterialityClass = "IMMATERIAL"
	// Unassessed: the run pins no threshold for this currency, and amounts in
	// another currency are never compared without an FX basis (B14).
	Unassessed MaterialityClass = "UNASSESSED"
)

// MaterialityPolicy is the per-currency materiality threshold, in minor units
// at the definition's amount scale (ZS-CONTROL-001 §11: materiality is a
// different control from tolerance). A currency with no threshold is
// UNASSESSED, never assumed immaterial.
type MaterialityPolicy struct {
	Currencies map[string]int64 `json:"currencies"`
}

func (p MaterialityPolicy) Validate() error {
	for c, v := range p.Currencies {
		if !currencyRe.MatchString(c) {
			return fmt.Errorf("%w: materiality currency %q must be an ISO 4217 code", ErrInvalidArgument, c)
		}
		if v <= 0 {
			return fmt.Errorf("%w: materiality threshold for %s must be positive", ErrInvalidArgument, c)
		}
	}
	return nil
}

// Classify compares an exposure (non-negative) with the currency's threshold.
func (p MaterialityPolicy) Classify(currency string, exposure *big.Int) MaterialityClass {
	t, ok := p.Currencies[currency]
	if !ok {
		return Unassessed
	}
	if exposure.Cmp(big.NewInt(t)) >= 0 {
		return Material
	}
	return Immaterial
}

// DeriveSeverity is policy-derived (§21), never operator-supplied — the
// pattern of financial-control-svc's DeriveSeverity, on DQC's scale:
//
//   - exposure >= threshold       => BLOCKER (individually material)
//   - exposure >= threshold / 10  => ERROR
//   - otherwise                   => WARNING
//   - no threshold for the currency => ERROR (cannot be assessed; it is not
//     assumed immaterial)
//
// INFO is never derived: an exception exists because something exceeded its
// tolerance. Age and recurrence do not rewrite a stored severity (DQC:
// "severity cannot be edited retroactively"); age is a derived attention signal.
func (p MaterialityPolicy) DeriveSeverity(currency string, exposure int64) (Severity, MaterialityClass) {
	t, ok := p.Currencies[currency]
	if !ok {
		return SeverityError, Unassessed
	}
	switch {
	case exposure >= t:
		return SeverityBlocker, Material
	case exposure >= t/10:
		return SeverityError, Immaterial
	}
	return SeverityWarning, Immaterial
}

// ExceptionPolicy is who owns a run's exceptions and how fast each severity
// must be dealt with (Invariant 7, §21 "Owner", "Due date / SLA").
type ExceptionPolicy struct {
	OwnerRole string           `json:"owner_role"`
	SLADays   map[Severity]int `json:"sla_days"`
}

var ownerRoleRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{1,63}$`)

func (p ExceptionPolicy) Validate() error {
	if !ownerRoleRe.MatchString(p.OwnerRole) {
		return fmt.Errorf("%w: exception_policy.owner_role is required (a role name, 2-64 characters)", ErrInvalidArgument)
	}
	for _, s := range []Severity{SeverityBlocker, SeverityError, SeverityWarning} {
		d, ok := p.SLADays[s]
		if !ok || d < 1 || d > 365 {
			return fmt.Errorf("%w: exception_policy.sla_days.%s must be between 1 and 365", ErrInvalidArgument, s)
		}
	}
	for s := range p.SLADays {
		if s != SeverityBlocker && s != SeverityError && s != SeverityWarning {
			return fmt.Errorf("%w: exception_policy.sla_days has no severity %q", ErrInvalidArgument, s)
		}
	}
	return nil
}

// DueAt is when an exception of this severity, raised at raisedAt, is due.
func (p ExceptionPolicy) DueAt(raisedAt time.Time, s Severity) time.Time {
	return raisedAt.Add(time.Duration(p.SLADays[s]) * 24 * time.Hour)
}

// CertificationProfile is the definition's explicit statement of what a
// certification may carry. PermitAcceptedRisk is ZS-SVC-Z-001 DQC-05's "unless
// certification profile explicitly permits and records governed risk
// acceptance"; it defaults to false.
type CertificationProfile struct {
	PermitAcceptedRisk bool `json:"permit_accepted_risk"`
}

// ── Commands ────────────────────────────────────────────────────────────────

var rootCauseRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,47}$`)

// TransitionExceptionRequest moves an exception along its lifecycle. CLOSED is
// not a target: an exception closes only when a manual match reconciles it.
type TransitionExceptionRequest struct {
	ToState       ExceptionStatus `json:"to_state"`
	Reason        string          `json:"reason"`
	AuthorityRef  string          `json:"authority_ref,omitempty"` // ACCEPTED_RISK: the approval that authorizes it
	EvidenceRef   string          `json:"evidence_ref,omitempty"`  // REMEDIATED: the correction in the owning domain
	RootCauseCode string          `json:"root_cause_code,omitempty"`
	RootCauseNote string          `json:"root_cause_note,omitempty"`
}

func (r TransitionExceptionRequest) Validate() error {
	bad := func(m string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, m) }
	switch r.ToState {
	case ExceptionInvestigating, ExceptionRemediated, ExceptionAcceptedRisk:
	case ExceptionClosed:
		return bad("an exception is closed only by a manual match that reconciles it")
	default:
		return bad("to_state must be INVESTIGATING, REMEDIATED or ACCEPTED_RISK")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return bad("reason is required")
	}
	if len(r.Reason) > 2000 || len(r.AuthorityRef) > 500 || len(r.EvidenceRef) > 500 || len(r.RootCauseNote) > 2000 {
		return bad("reason, authority_ref, evidence_ref or root_cause_note is too long")
	}
	if r.RootCauseCode != "" && !rootCauseRe.MatchString(r.RootCauseCode) {
		return bad("root_cause_code must be UPPER_SNAKE_CASE, at most 48 characters")
	}
	switch r.ToState {
	case ExceptionRemediated:
		if strings.TrimSpace(r.EvidenceRef) == "" {
			return bad("evidence_ref is required: name the correction made in the owning domain")
		}
	case ExceptionAcceptedRisk:
		if strings.TrimSpace(r.AuthorityRef) == "" {
			return bad("authority_ref is required: accepted risk needs a named authority (DQC-05)")
		}
	}
	return nil
}

// CheckRootCause: a root cause is required to remediate or accept a
// significant (BLOCKER) exception (§21 "Root cause — required for
// significant/recurrent exceptions"). Recurrence across runs is not tracked
// yet (A4).
func (r TransitionExceptionRequest) CheckRootCause(sev Severity) error {
	if sev != SeverityBlocker || (r.ToState != ExceptionRemediated && r.ToState != ExceptionAcceptedRisk) {
		return nil
	}
	if r.RootCauseCode == "" || strings.TrimSpace(r.RootCauseNote) == "" {
		return fmt.Errorf("%w: root_cause_code and root_cause_note are required for a BLOCKER exception", ErrInvalidArgument)
	}
	return nil
}

// CheckIndependence: working an exception is its assigned owner's job; risk
// is accepted by someone other than the owner it excuses and the person who
// raised it (financial-control-svc's maker-checker on waivers).
func CheckIndependence(to ExceptionStatus, actor string, owner *string, raisedBy string) error {
	own := ""
	if owner != nil {
		own = *owner
	}
	switch to {
	case ExceptionInvestigating, ExceptionRemediated:
		if own == "" || actor != own {
			return ErrNotExceptionOwner
		}
	case ExceptionAcceptedRisk:
		if actor == own || actor == raisedBy {
			return ErrNotIndependent
		}
	}
	return nil
}

// CheckRevokerIndependence: the principal who certified a run cannot revoke
// that certification, the same independence a certifier has from the run's
// preparers (CheckCertifierIndependence; financial-control-svc's
// CheckRevokerIndependence). certifiers is everyone recorded as certifying the
// run — every actor of its CERTIFICATION -> CERTIFIED transitions, and the
// certification's sealer — not only the last of them.
func CheckRevokerIndependence(actor string, certifiers []string) error {
	for _, c := range certifiers {
		if actor == c {
			return ErrRevokerIsCertifier
		}
	}
	return nil
}

// CheckCertifierIndependence: a preparer cannot self-certify (ZS-CONTROL-001
// Invariant 12). preparers is everyone who prepared the run — started it,
// matched in it, or reperformed it — not only the last of them, so a trivial
// final step by someone else does not clear the way for the original preparer.
func CheckCertifierIndependence(actor string, preparers []string) error {
	for _, p := range preparers {
		if actor == p {
			return ErrNotIndependentCertifier
		}
	}
	return nil
}

type AssignExceptionRequest struct {
	OwnerPrincipal string `json:"owner_principal_id"`
	Reason         string `json:"reason"`
}

func (r AssignExceptionRequest) Validate() error {
	if strings.TrimSpace(r.OwnerPrincipal) == "" || len(r.OwnerPrincipal) > 255 {
		return fmt.Errorf("%w: owner_principal_id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidArgument)
	}
	return nil
}

// ExceptionTransition is one row of an exception's append-only history.
type ExceptionTransition struct {
	TransitionID   int64     `json:"transition_id"`
	ExceptionID    string    `json:"exception_id"`
	FromState      string    `json:"from_state"`
	ToState        string    `json:"to_state"`
	OwnerPrincipal *string   `json:"owner_principal_id,omitempty"`
	Reason         string    `json:"reason"`
	AuthorityRef   *string   `json:"authority_ref,omitempty"`
	EvidenceRef    *string   `json:"evidence_ref,omitempty"`
	RootCauseCode  *string   `json:"root_cause_code,omitempty"`
	RootCauseNote  *string   `json:"root_cause_note,omitempty"`
	Actor          string    `json:"actor"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// Derive fills the non-authoritative, read-time fields (§7 attention
// signals, §20 aging). They are never stored, so they cannot overwrite an
// authoritative state.
func (e *ReconciliationException) Derive(now time.Time) {
	age := int(now.Sub(e.RaisedAt).Hours() / 24)
	e.AgeDays = &age
	e.Attention = nil
	if e.Status.Working() && e.DueAt != nil && now.After(*e.DueAt) {
		e.Attention = append(e.Attention, "OVERDUE", "SLA_BREACH")
	}
	if e.Materiality != nil && *e.Materiality == Material {
		e.Attention = append(e.Attention, "MATERIAL")
	}
	if e.Owner == nil && e.Status.Working() {
		e.Attention = append(e.Attention, "UNASSIGNED")
	}
}

// ── Aggregate assessment ────────────────────────────────────────────────────

// Exposure is one unresolved exception's contribution to the aggregate.
type Exposure struct {
	ExceptionID string
	Kind        ExceptionKind
	Status      ExceptionStatus
	Currency    string
	ItemID      string
	Counterpart string // the paired item, for a key-matched pair raised on both items
	Minor       int64
}

// CurrencyAssessment is the aggregate of one currency's unresolved exposure.
type CurrencyAssessment struct {
	Currency            string           `json:"currency"`
	UnresolvedCount     int              `json:"unresolved_count"`
	AcceptedRiskCount   int              `json:"accepted_risk_count"`
	ExposureMinorUnits  string           `json:"exposure_minor_units"` // decimal string: an aggregate can exceed int64
	AcceptedRiskMinor   string           `json:"accepted_risk_minor_units"`
	ThresholdMinorUnits *int64           `json:"threshold_minor_units,omitempty"`
	Assessment          MaterialityClass `json:"assessment"`
	AcceptedRisk        MaterialityClass `json:"accepted_risk_assessment"`
}

// AssessAggregate assesses unresolved exposure per currency, individually
// immaterial items included (§11 "assessed individually and in aggregate";
// scenario 25). Aggregation basis: item exceptions are summed, with a
// key-matched pair (raised on both of its items with the same exposure)
// counted once; a BALANCE exception is itself an aggregate of items already
// counted, so it is assessed on its own and the currency's aggregate is the
// larger of the item sum and the largest balance difference. Accepted-risk
// exposure is assessed separately, by the same basis.
func AssessAggregate(exps []Exposure, p MaterialityPolicy) []CurrencyAssessment {
	type acc struct {
		items, itemsAR, bal, balAR *big.Int
		n, nAR                     int
	}
	by := map[string]*acc{}
	seenPair := map[string]bool{}
	for _, e := range exps {
		if !e.Status.Unresolved() {
			continue
		}
		a := by[e.Currency]
		if a == nil {
			a = &acc{items: new(big.Int), itemsAR: new(big.Int), bal: new(big.Int), balAR: new(big.Int)}
			by[e.Currency] = a
		}
		ar := e.Status == ExceptionAcceptedRisk
		a.n++
		if ar {
			a.nAR++
		}
		v := big.NewInt(e.Minor)
		if e.Kind == ExceptionKindBalance {
			if v.Cmp(a.bal) > 0 {
				a.bal = v
			}
			if ar && v.Cmp(a.balAR) > 0 {
				a.balAR = v
			}
			continue
		}
		if e.Counterpart != "" {
			x, y := e.ItemID, e.Counterpart
			if y < x {
				x, y = y, x
			}
			key := x + "|" + y
			if ar {
				key = "AR|" + key
			}
			if seenPair[key] {
				continue
			}
			seenPair[key] = true
		}
		if ar {
			a.itemsAR.Add(a.itemsAR, v)
		} else {
			a.items.Add(a.items, v)
		}
	}
	out := make([]CurrencyAssessment, 0, len(by))
	for c, a := range by {
		total := new(big.Int).Add(a.items, a.itemsAR)
		if a.bal.Cmp(total) > 0 {
			total = a.bal
		}
		arTotal := new(big.Int).Set(a.itemsAR)
		if a.balAR.Cmp(arTotal) > 0 {
			arTotal = a.balAR
		}
		ca := CurrencyAssessment{Currency: c, UnresolvedCount: a.n, AcceptedRiskCount: a.nAR,
			ExposureMinorUnits: total.String(), AcceptedRiskMinor: arTotal.String(),
			Assessment: p.Classify(c, total), AcceptedRisk: p.Classify(c, arTotal)}
		if t, ok := p.Currencies[c]; ok {
			ca.ThresholdMinorUnits = &t
		}
		if a.nAR == 0 {
			ca.AcceptedRisk = Immaterial
		}
		out = append(out, ca)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// RunAssessment is what a run's result rests on: its unresolved exceptions by
// state, the accepted risks among them, the aggregate materiality per
// currency, and whether the run may be certified — and if not, why.
type RunAssessment struct {
	ResultState         ResultState             `json:"evaluated_result"`
	Unresolved          map[ExceptionStatus]int `json:"unresolved"`
	AcceptedRiskIDs     []string                `json:"accepted_risk_exception_ids"`
	Currencies          []CurrencyAssessment    `json:"currencies"`
	PermitAcceptedRisk  bool                    `json:"permit_accepted_risk"`
	Certifiable         bool                    `json:"certifiable"`
	CertificationBlocks []string                `json:"certification_blocks,omitempty"`
}

// Assess decides certifiability (DQC-03, DQC-05, INV-19, INV-26):
//   - with no unresolved exception the result is MATCH and it is certifiable;
//   - a working (OPEN / INVESTIGATING / REMEDIATED) exception blocks;
//   - with only accepted risks, the result stays VARIANCE and it is
//     certifiable only if the pinned profile permits accepted risk AND the
//     accepted exposure is IMMATERIAL in aggregate in every currency (a
//     material or unassessable accepted risk needs a correction and a new
//     run; accepting it would need an authority tier this service does not
//     have — A9).
func Assess(exps []Exposure, p MaterialityPolicy, profile CertificationProfile) RunAssessment {
	ra := RunAssessment{Unresolved: map[ExceptionStatus]int{}, AcceptedRiskIDs: []string{}, PermitAcceptedRisk: profile.PermitAcceptedRisk}
	working := 0
	for _, e := range exps {
		if !e.Status.Unresolved() {
			continue
		}
		ra.Unresolved[e.Status]++
		if e.Status == ExceptionAcceptedRisk {
			ra.AcceptedRiskIDs = append(ra.AcceptedRiskIDs, e.ExceptionID)
		} else {
			working++
		}
	}
	sort.Strings(ra.AcceptedRiskIDs)
	ra.Currencies = AssessAggregate(exps, p)
	ra.ResultState = EvaluatedResult(int64(working + len(ra.AcceptedRiskIDs)))
	if working > 0 {
		ra.CertificationBlocks = append(ra.CertificationBlocks, fmt.Sprintf("%d exception(s) are still being worked", working))
	}
	if len(ra.AcceptedRiskIDs) > 0 {
		if !profile.PermitAcceptedRisk {
			ra.CertificationBlocks = append(ra.CertificationBlocks, "the certification profile does not permit accepted risk")
		}
		for _, c := range ra.Currencies {
			if c.AcceptedRiskCount > 0 && c.AcceptedRisk != Immaterial {
				ra.CertificationBlocks = append(ra.CertificationBlocks,
					fmt.Sprintf("accepted risk in %s is %s in aggregate", c.Currency, c.AcceptedRisk))
			}
		}
	}
	ra.Certifiable = len(ra.CertificationBlocks) == 0
	return ra
}

// ExposureOf is |v|, saturating at MaxInt64: |MinInt64| does not fit an
// int64, and an exposure that large is material under any threshold anyway.
func ExposureOf(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}

// ItemExposure is what an automatic item exception puts at risk: for an amount
// outside tolerance, the difference between the paired items; for anything
// else (unmatched, missing key, currency or timing mismatch), the whole item.
func ItemExposure(e ItemException) int64 {
	if e.Detail.Rule == RuleAmountTolerance && e.Detail.DifferenceMinor != nil {
		return ExposureOf(*e.Detail.DifferenceMinor)
	}
	return ExposureOf(e.Item.Amount)
}
