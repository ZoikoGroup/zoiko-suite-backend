package kernel

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Reconciliation (standard §27): explicit, governed cases rather than silent edits. A run compares two populations at a stated
// scope and moment, records the difference, judges it by a versioned tolerance rule and, when it does not match, becomes a case
// with an owner, an SLA and a controlled way to be closed. The run itself never changes: its populations, totals and tolerance
// are fixed, and the ledger side is reproducible from the watermark it records.

// Reconciliation types supported by the kernel's current waves.
const (
	ReconAR           = "AR_GL"
	ReconAP           = "AP_GL"
	ReconBank         = "BANK_GL"
	ReconEventPosting = "EVENT_POSTING"
	ReconPayroll      = "PAYROLL_GL"
	ReconTax          = "TAX_GL"
	ReconAsset        = "ASSET_GL"
	ReconInventory    = "INVENTORY_GL"
	// the two reconciliations the kernel runs entirely from its own records: the reciprocal balances of two entities of a group, and
	// the ledger against a signed snapshot (the trial balance a report is made from)
	ReconIntercompany   = "INTERCOMPANY_GL"
	ReconLedgerSnapshot = "LEDGER_SNAPSHOT"
)

// ReconTypes lists the reconciliation types the kernel runs.
var ReconTypes = []string{ReconAR, ReconAP, ReconBank, ReconPayroll, ReconTax, ReconAsset, ReconInventory, ReconIntercompany, ReconLedgerSnapshot, ReconEventPosting}

const reconTypeList = "AR_GL, AP_GL, BANK_GL, PAYROLL_GL, TAX_GL, ASSET_GL, INVENTORY_GL, INTERCOMPANY_GL, LEDGER_SNAPSHOT or EVENT_POSTING"

// Case statuses.
const (
	ReconMatched           = "MATCHED"
	ReconWarning           = "WARNING"
	ReconBreak             = "BREAK"
	ReconAcceptedException = "ACCEPTED_EXCEPTION"
	ReconResolved          = "RESOLVED"
)

// controlOf maps a reconciliation type to the control type of the ledger accounts it reads and the side that is the
// balance of a normal position there.
var controlOf = map[string]struct {
	Control     string
	DebitNormal bool
}{
	ReconAR: {"AR", true}, ReconAP: {"AP", false}, ReconBank: {"BANK", true},
	// payroll and tax liabilities are credit-normal; the fixed-asset register (cost less accumulated depreciation) and inventory are debit-normal
	ReconPayroll: {"PAYROLL", false}, ReconTax: {"TAX", false}, ReconAsset: {"ASSET", true}, ReconInventory: {"INVENTORY", true},
}

// ResolutionTypes are the controlled reasons a case is closed for (§27.2).
var ResolutionTypes = []string{"TIMING", "MAPPING", "MISSING_EVENT", "DUPLICATE", "MANUAL_CORRECTION", "EXTERNAL_ISSUE", "OTHER"}

// Population is one side of a reconciliation as the run recorded it.
type Population struct {
	Source    string `json:"source"`
	Reference string `json:"reference,omitempty"`
	Watermark string `json:"watermark,omitempty"`
	Count     int    `json:"count"`
	Total     string `json:"total"`
	Currency  string `json:"currency,omitempty"`
	// Groups breaks the total down by a dimension (an asset class, a location): when a run groups, each group is compared as well as
	// the total, so two offsetting differences cannot hide inside a matching total.
	Groups map[string]string `json:"groups,omitempty"`
	Detail map[string]any    `json:"detail,omitempty"`
}

// Tolerance is a versioned threshold: a difference up to MatchAmount is a match, up to WarningAmount a warning, anything more a break.
type Tolerance struct {
	ToleranceRuleID    string    `json:"tolerance_rule_id"`
	TenantID           string    `json:"-"`
	ReconciliationType string    `json:"reconciliation_type"`
	LegalEntityID      string    `json:"legal_entity_id,omitempty"`
	MatchAmount        string    `json:"match_amount"`
	WarningAmount      string    `json:"warning_amount"`
	Version            int       `json:"version"`
	EffectiveFrom      time.Time `json:"effective_from"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
}

// ReconRun is a reconciliation run and, when it did not match, the case that follows from it.
type ReconRun struct {
	RunID            string     `json:"reconciliation_run_id"`
	TenantID         string     `json:"tenant_id"`
	Type             string     `json:"reconciliation_type"`
	LegalEntityID    string     `json:"legal_entity_id"`
	BookID           string     `json:"accounting_book_id"`
	FiscalPeriod     string     `json:"fiscal_period"`
	AsOf             string     `json:"as_of"`
	Currency         string     `json:"currency"`
	PopulationA      Population `json:"population_a"`
	PopulationB      Population `json:"population_b"`
	ExpectedTotal    string     `json:"expected_total"`
	ActualTotal      string     `json:"actual_total"`
	Difference       string     `json:"difference"`
	ToleranceRuleID  string     `json:"tolerance_rule_id,omitempty"`
	ToleranceVersion int        `json:"tolerance_version,omitempty"`
	Status           string     `json:"status"`
	Owner            string     `json:"owner,omitempty"`
	SLADueAt         *time.Time `json:"sla_due_at,omitempty"`
	ResolutionType   string     `json:"resolution_type,omitempty"`
	ResolutionNote   string     `json:"resolution_note,omitempty"`
	Evidence         []string   `json:"evidence"`
	ResolvedBy       string     `json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	ApprovedBy       string     `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
}

// ReconEvent is one entry of a case's history.
type ReconEvent struct {
	Kind       string         `json:"kind"`
	FromStatus string         `json:"from_status,omitempty"`
	ToStatus   string         `json:"to_status,omitempty"`
	Actor      string         `json:"actor"`
	Detail     map[string]any `json:"detail,omitempty"`
	At         time.Time      `json:"at"`
}

// LedgerPosition is what the ledger says about one control type in one book at a moment: the sums of the entries up to a
// watermark, so the same position can be reproduced later.
type LedgerPosition struct {
	Watermark    int64
	Count        int
	DebitTotal   string
	CreditTotal  string
	AccountCodes []string
	Groups       []GroupTotal
}

// PositionOpts widens or breaks down a ledger position. IncludeUnbooked adds the entity's entries that name no book (postings made
// before books existed, or by services that do not name one); GroupBy sums the position by a dimension.
type PositionOpts struct {
	IncludeUnbooked bool
	GroupBy         string
}

// GroupTotal is the ledger's debit and credit total for one value of the grouping dimension.
type GroupTotal struct {
	Key    string
	Debit  string
	Credit string
}

// EventPosting is the count of accounting events registered up to a moment and how many of them have a posting in the book.
type EventPosting struct {
	Registered int
	Posted     int
	Missing    []string
	At         time.Time
}

// ReconFilter selects runs.
type ReconFilter struct {
	Type, Status, LegalEntityID, BookID, FiscalPeriod string
	Limit                                             int
}

// ReconRepo is the persistence boundary of reconciliation.
type ReconRepo interface {
	LedgerPosition(ctx context.Context, tenant, entity, bookCode, controlType string, asOf time.Time, upToSeq int64, opts PositionOpts) (LedgerPosition, error)
	EventPosting(ctx context.Context, tenant, entity, bookID string, asOf time.Time, at time.Time) (EventPosting, error)
	// IntercompanyPosition sums the INTERCOMPANY-control entries of an entity in a book that name the counterparty entity, up to a date and,
	// when upToSeq is given, a watermark.
	IntercompanyPosition(ctx context.Context, tenant, entity, bookCode, counterparty string, asOf time.Time, upToSeq int64) (LedgerPosition, error)
	PickTolerance(ctx context.Context, tenant, reconType, entity string, asOf time.Time) (*Tolerance, error)
	InsertTolerance(ctx context.Context, t *Tolerance, out OutEvent) error
	ListTolerances(ctx context.Context, tenant, reconType string) ([]Tolerance, error)
	InsertRun(ctx context.Context, r *ReconRun, out *OutEvent) error
	GetRun(ctx context.Context, tenant, id string) (*ReconRun, error)
	ListRuns(ctx context.Context, tenant string, f ReconFilter) ([]ReconRun, error)
	RunEvents(ctx context.Context, tenant, id string) ([]ReconEvent, error)
	// AssignRun, AcceptRun and ResolveRun change how a case is handled, each in one transaction with its history entry and event.
	AssignRun(ctx context.Context, tenant, id, owner string, sla time.Time, actor string) (*ReconRun, error)
	AcceptRun(ctx context.Context, tenant, id, approver, note string, evidence []string, at time.Time) (*ReconRun, error)
	ResolveRun(ctx context.Context, tenant, id, resolver, resolutionType, note string, evidence []string, journalID string, at time.Time) (*ReconRun, error)
	CaseStatus(ctx context.Context, tenant, entity, bookID, period string) (*CaseStatus, error)
	CounterpartyBalances(ctx context.Context, tenant, entity, bookCode, controlType string, asOf time.Time) ([]CounterpartyBalance, error)
}

// CounterpartyBalance is the position of one counterparty (customer or supplier) on a control type.
type CounterpartyBalance struct {
	CounterpartyID string `json:"counterparty_id"`
	Entries        int    `json:"entries"`
	Debit          string `json:"debit_total"`
	Credit         string `json:"credit_total"`
	Balance        string `json:"balance"`
}

// CaseStatus is the reconciliation picture of one scope for the period-close gate: what is open, and whether posting has failed.
type CaseStatus struct {
	OpenBreaks      int               `json:"open_breaks"`
	OpenWarnings    int               `json:"open_warnings"`
	FailedPostings  int               `json:"failed_posting_executions"`
	LatestByType    map[string]string `json:"latest_status_by_type"`
	OpenCaseIDs     []string          `json:"open_case_ids"`
	Clear           bool              `json:"clear"`
	MissingRequired []string          `json:"required_types_not_run"`
}

// ReconService is the command side of reconciliation.
type ReconService struct {
	Repo  ReconRepo
	Books Repo
	Clock func() time.Time
	// Snapshots and Signer are what a LEDGER_SNAPSHOT run reads and verifies; without them that type is refused.
	Snapshots SnapshotRepo
	Signer    Signer
}

func (s *ReconService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// RunRequest asks for a reconciliation. PopulationA is what the subledger or the bank statement says; it is supplied by the
// system that owns it. The kernel reads its own side from the ledger. EVENT_POSTING needs no population: both sides are the kernel's own.
type RunRequest struct {
	Type         string      `json:"reconciliation_type"`
	BookID       string      `json:"book_id"`
	FiscalPeriod string      `json:"fiscal_period"`
	AsOf         string      `json:"as_of"`
	PopulationA  *Population `json:"population_a,omitempty"`
	Owner        string      `json:"owner,omitempty"`
	SLAHours     int         `json:"sla_hours,omitempty"`
	// GroupBy names a dimension to compare group by group (population_a.groups gives the subledger's figure for each value);
	// IncludeUnbooked also counts the entity's entries that name no book.
	GroupBy string `json:"group_by,omitempty"`
	// CounterpartyBookID is the other entity's book of an INTERCOMPANY_GL run; SnapshotID is the signed snapshot of a LEDGER_SNAPSHOT run.
	CounterpartyBookID string `json:"counterparty_book_id,omitempty"`
	SnapshotID         string `json:"snapshot_id,omitempty"`
	IncludeUnbooked    bool   `json:"include_unbooked,omitempty"`
}

func ratFixed(r *big.Rat) string { return r.FloatString(6) }

func parseAmount(field, s string) (*big.Rat, error) {
	r, ok := ParseDecimal(strings.TrimSpace(s))
	if !ok {
		return nil, exc(FamData, "RECON_INVALID", "%s is not a decimal amount", field).with("field", field)
	}
	if new(big.Rat).Mul(r, big.NewRat(1000000, 1)).Denom().Cmp(big.NewInt(1)) != 0 {
		return nil, exc(FamData, "RECON_INVALID", "%s has more than six decimals", field).with("field", field)
	}
	return r, nil
}

// Run performs one reconciliation and records it. The result is a MATCHED run, or a WARNING or BREAK case.
func (s *ReconService) Run(ctx context.Context, tenant string, a Actor, req RunRequest) (*ReconRun, error) {
	bad := func(f, why string) error { return exc(FamData, "RECON_INVALID", "%s: %s", f, why).with("field", f) }
	spec, supported := controlOf[req.Type]
	if !supported && !ownSided(req.Type) {
		return nil, bad("reconciliation_type", reconTypeList)
	}
	var snap *BalanceSnapshot
	if req.Type == ReconLedgerSnapshot {
		// the snapshot fixes the scope: its book, its date and its period
		if s.Snapshots == nil {
			return nil, exc(FamExt, "RECON_UNAVAILABLE", "signed snapshots are not available to reconcile against")
		}
		if _, err := uuid.Parse(req.SnapshotID); err != nil {
			return nil, bad("snapshot_id", "a retained signed snapshot")
		}
		var err error
		if snap, err = s.Snapshots.GetSnapshot(ctx, tenant, req.SnapshotID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, bad("snapshot_id", "no such snapshot")
			}
			return nil, err
		}
		if req.BookID != "" && req.BookID != snap.BookID {
			return nil, bad("book_id", "the snapshot is of another book")
		}
		req.BookID, req.AsOf, req.FiscalPeriod = snap.BookID, snap.AsOf, snap.FiscalPeriod
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, bad("book_id", "an accounting book id")
	}
	asOf, err := time.Parse("2006-01-02", req.AsOf)
	if err != nil {
		return nil, bad("as_of", "a date, YYYY-MM-DD")
	}
	if asOf.After(today(s.now())) {
		return nil, bad("as_of", "not in the future")
	}
	period := strings.TrimSpace(req.FiscalPeriod)
	if period == "" {
		period = FiscalPeriodOf(asOf)
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	run := &ReconRun{RunID: uuid.NewString(), TenantID: tenant, Type: req.Type, LegalEntityID: bk.LegalEntityID, BookID: bk.BookID, FiscalPeriod: period,
		AsOf: req.AsOf, Currency: bk.FunctionalCurrency, Evidence: []string{}, CreatedBy: a.PrincipalID, CreatedAt: now}

	var expected, actual *big.Rat
	groupWorst := new(big.Rat)
	forceBreak := false
	switch req.Type {
	case ReconIntercompany:
		if expected, actual, err = s.reconcileIntercompany(ctx, tenant, req, bk, asOf, run); err != nil {
			return nil, err
		}
	case ReconLedgerSnapshot:
		if expected, actual, groupWorst, forceBreak, err = s.reconcileLedgerSnapshot(ctx, tenant, bk, snap, run); err != nil {
			return nil, err
		}
	}
	if req.Type == ReconIntercompany || req.Type == ReconLedgerSnapshot {
		// both sides came from the kernel's own records
	} else if req.Type == ReconEventPosting {
		at := now
		ep, err := s.Repo.EventPosting(ctx, tenant, bk.LegalEntityID, bk.BookID, asOf, at)
		if err != nil {
			return nil, err
		}
		expected, actual = big.NewRat(int64(ep.Registered), 1), big.NewRat(int64(ep.Posted), 1)
		run.PopulationA = Population{Source: "ACCOUNTING_EVENTS", Watermark: at.Format(time.RFC3339Nano), Count: ep.Registered, Total: ratFixed(expected)}
		run.PopulationB = Population{Source: "EVENT_POSTING_LINKS", Watermark: at.Format(time.RFC3339Nano), Count: ep.Posted, Total: ratFixed(actual),
			Detail: map[string]any{"missing_event_ids": ep.Missing}}
		run.Currency = ""
	} else {
		if req.PopulationA == nil {
			return nil, bad("population_a", "the subledger or bank statement population is required")
		}
		pa := *req.PopulationA
		if strings.TrimSpace(pa.Source) == "" || strings.TrimSpace(pa.Reference) == "" {
			return nil, bad("population_a", "names its source and a reference (the subledger run or statement)")
		}
		if pa.Currency != "" && pa.Currency != bk.FunctionalCurrency {
			return nil, exc(FamFX, "RECON_CURRENCY", "population_a is in %s and the book is in %s: supply the population in the book currency", pa.Currency, bk.FunctionalCurrency)
		}
		if expected, err = parseAmount("population_a.total", pa.Total); err != nil {
			return nil, err
		}
		pa.Currency, pa.Total = bk.FunctionalCurrency, ratFixed(expected)
		opts := PositionOpts{IncludeUnbooked: req.IncludeUnbooked, GroupBy: strings.TrimSpace(req.GroupBy)}
		var groupsA map[string]*big.Rat
		if opts.GroupBy != "" {
			if !codeRE.MatchString(opts.GroupBy) {
				return nil, bad("group_by", "a dimension code")
			}
			if len(pa.Groups) == 0 {
				return nil, bad("population_a.groups", "the subledger's figure for each value of the dimension")
			}
			groupsA = map[string]*big.Rat{}
			sum := new(big.Rat)
			for k, v := range pa.Groups {
				g, gerr := parseAmount("population_a.groups."+k, v)
				if gerr != nil {
					return nil, gerr
				}
				groupsA[k] = g
				sum.Add(sum, g)
			}
			if sum.Cmp(expected) != 0 {
				return nil, bad("population_a.groups", "the groups must add up to the total")
			}
		} else if len(pa.Groups) > 0 {
			return nil, bad("group_by", "population_a carries groups, so the dimension they are grouped by is required")
		}
		pos, err := s.Repo.LedgerPosition(ctx, tenant, bk.LegalEntityID, bk.BookCode, spec.Control, asOf, 0, opts)
		if err != nil {
			return nil, err
		}
		dr, _ := new(big.Rat).SetString(pos.DebitTotal)
		cr, _ := new(big.Rat).SetString(pos.CreditTotal)
		if dr == nil || cr == nil {
			return nil, exc(FamData, "RECON_LEDGER", "the ledger position could not be read")
		}
		if spec.DebitNormal {
			actual = new(big.Rat).Sub(dr, cr)
		} else {
			actual = new(big.Rat).Sub(cr, dr)
		}
		run.PopulationA = pa
		run.PopulationB = Population{Source: "LEDGER", Watermark: fmt.Sprint(pos.Watermark), Count: pos.Count, Total: ratFixed(actual), Currency: bk.FunctionalCurrency,
			Detail: map[string]any{"control_type": spec.Control, "debit_total": pos.DebitTotal, "credit_total": pos.CreditTotal, "normal_side": map[bool]string{true: "DEBIT", false: "CREDIT"}[spec.DebitNormal], "account_codes": pos.AccountCodes,
				"include_unbooked": opts.IncludeUnbooked}}
		if opts.GroupBy != "" {
			rows, worstGroup := compareGroups(groupsA, pos.Groups, spec.DebitNormal)
			run.PopulationB.Detail["group_by"], run.PopulationB.Detail["groups"] = opts.GroupBy, rows
			groupWorst = worstGroup
		}
	}
	diff := new(big.Rat).Sub(expected, actual)
	run.ExpectedTotal, run.ActualTotal, run.Difference = ratFixed(expected), ratFixed(actual), ratFixed(diff)

	match, warn := new(big.Rat), new(big.Rat)
	if tol, err := s.Repo.PickTolerance(ctx, tenant, req.Type, bk.LegalEntityID, asOf); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	} else if tol != nil {
		match, _ = new(big.Rat).SetString(tol.MatchAmount)
		warn, _ = new(big.Rat).SetString(tol.WarningAmount)
		run.ToleranceRuleID, run.ToleranceVersion = tol.ToleranceRuleID, tol.Version
	}
	abs := new(big.Rat).Abs(diff)
	if groupWorst.Cmp(abs) > 0 {
		abs = groupWorst // the case is judged by its worst figure: a matching total does not excuse a group that is out
	}
	var out *OutEvent
	switch {
	case abs.Cmp(match) <= 0:
		run.Status = ReconMatched
	case abs.Cmp(warn) <= 0:
		run.Status = ReconWarning
	default:
		run.Status = ReconBreak
	}
	if forceBreak {
		run.Status = ReconBreak // a snapshot whose signature or content does not verify is not a report source, whatever its figures
	}
	if run.Status != ReconMatched {
		hours := req.SLAHours
		if hours <= 0 {
			hours = map[string]int{ReconBreak: 72, ReconWarning: 168}[run.Status]
		}
		due := now.Add(time.Duration(hours) * time.Hour)
		run.SLADueAt = &due
		run.Owner = strings.TrimSpace(req.Owner)
		if run.Owner == "" {
			run.Owner = a.PrincipalID
		}
		out = &OutEvent{Fact: "reconciliation.break-raised", AggregateType: "reconciliation-run", AggregateID: run.RunID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
			LegalEntityID: run.LegalEntityID, Data: map[string]any{"reconciliation_run_id": run.RunID, "reconciliation_type": run.Type, "status": run.Status, "book_id": run.BookID,
				"fiscal_period": run.FiscalPeriod, "difference": run.Difference, "currency": run.Currency, "owner": run.Owner, "sla_due_at": due}}
	}
	if err := s.Repo.InsertRun(ctx, run, out); err != nil {
		return nil, err
	}
	return run, nil
}

// compareGroups compares the subledger's figure for each group with the ledger's, and returns each group's difference with the largest
// absolute one. A group on only one side is compared with zero on the other.
func compareGroups(a map[string]*big.Rat, ledger []GroupTotal, debitNormal bool) ([]map[string]string, *big.Rat) {
	actual := map[string]*big.Rat{}
	for _, g := range ledger {
		dr, cr := dec(g.Debit), dec(g.Credit)
		if debitNormal {
			actual[g.Key] = new(big.Rat).Sub(dr, cr)
		} else {
			actual[g.Key] = new(big.Rat).Sub(cr, dr)
		}
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range actual {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	rows := make([]map[string]string, 0, len(sorted))
	worst := new(big.Rat)
	for _, k := range sorted {
		e, ac := a[k], actual[k]
		if e == nil {
			e = new(big.Rat)
		}
		if ac == nil {
			ac = new(big.Rat)
		}
		d := new(big.Rat).Sub(e, ac)
		if ad := new(big.Rat).Abs(d); ad.Cmp(worst) > 0 {
			worst = ad
		}
		label := k
		if label == "" {
			label = "(none)"
		}
		rows = append(rows, map[string]string{"key": label, "expected": ratFixed(e), "actual": ratFixed(ac), "difference": ratFixed(d)})
	}
	return rows, worst
}

// Assign gives an open case an owner and an SLA.
func (s *ReconService) Assign(ctx context.Context, tenant string, a Actor, id, owner string, slaHours int) (*ReconRun, error) {
	if strings.TrimSpace(owner) == "" || slaHours < 1 || slaHours > 24*90 {
		return nil, exc(FamData, "RECON_INVALID", "an owner and an SLA of 1 to 2160 hours")
	}
	return s.Repo.AssignRun(ctx, tenant, id, strings.TrimSpace(owner), s.now().Add(time.Duration(slaHours)*time.Hour), a.PrincipalID)
}

// Accept accepts a break or warning as an exception. It needs evidence, a reason and an approver other than the person who ran the
// reconciliation; the database holds that last rule itself.
func (s *ReconService) Accept(ctx context.Context, tenant string, a Actor, id, note string, evidence []string) (*ReconRun, error) {
	evidence = cleanRefs(evidence)
	if len(evidence) == 0 || strings.TrimSpace(note) == "" {
		return nil, exc(FamData, "RECON_INVALID", "accepting an exception needs evidence and a reason")
	}
	return s.Repo.AcceptRun(ctx, tenant, id, a.PrincipalID, strings.TrimSpace(note), evidence, s.now())
}

// Resolve closes a case with a controlled resolution type and evidence. A manual correction names the posted journal that made it.
func (s *ReconService) Resolve(ctx context.Context, tenant string, a Actor, id, resolutionType, note string, evidence []string, journalID string) (*ReconRun, error) {
	ok := false
	for _, t := range ResolutionTypes {
		ok = ok || t == resolutionType
	}
	evidence = cleanRefs(evidence)
	if !ok || len(evidence) == 0 {
		return nil, exc(FamData, "RECON_INVALID", "a resolution type (%s) and evidence", strings.Join(ResolutionTypes, ", "))
	}
	if resolutionType == "MANUAL_CORRECTION" {
		if _, err := uuid.Parse(journalID); err != nil {
			return nil, exc(FamData, "RECON_INVALID", "a manual correction names the posted journal that made it (journal_id)")
		}
	}
	return s.Repo.ResolveRun(ctx, tenant, id, a.PrincipalID, resolutionType, strings.TrimSpace(note), evidence, journalID, s.now())
}

// Verify recomputes the ledger side of a run from the watermark it recorded and reports whether it is the same. The ledger is
// append-only, so a run that cannot be reproduced means something other than the ledger was changed.
func (s *ReconService) Verify(ctx context.Context, tenant, id string) (map[string]any, error) {
	run, err := s.Repo.GetRun(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if run.Type == ReconIntercompany || run.Type == ReconLedgerSnapshot {
		if run.Type == ReconLedgerSnapshot && s.Snapshots == nil {
			return nil, exc(FamExt, "RECON_UNAVAILABLE", "signed snapshots are not available to verify against")
		}
		return s.verifyOwnSided(ctx, tenant, run)
	}
	asOf, _ := time.Parse("2006-01-02", run.AsOf)
	var recomputed *big.Rat
	groupsOK := true
	if run.Type == ReconEventPosting {
		at, perr := time.Parse(time.RFC3339Nano, run.PopulationA.Watermark)
		if perr != nil {
			return nil, exc(FamData, "RECON_INVALID", "the run carries no watermark")
		}
		ep, err := s.Repo.EventPosting(ctx, tenant, run.LegalEntityID, run.BookID, asOf, at)
		if err != nil {
			return nil, err
		}
		recomputed = big.NewRat(int64(ep.Posted), 1)
	} else {
		spec := controlOf[run.Type]
		bk, err := s.Books.GetBook(ctx, tenant, run.BookID)
		if err != nil {
			return nil, err
		}
		var wm int64
		fmt.Sscan(run.PopulationB.Watermark, &wm)
		opts := PositionOpts{}
		if v, ok := run.PopulationB.Detail["include_unbooked"].(bool); ok {
			opts.IncludeUnbooked = v
		}
		if v, ok := run.PopulationB.Detail["group_by"].(string); ok {
			opts.GroupBy = v
		}
		pos, err := s.Repo.LedgerPosition(ctx, tenant, run.LegalEntityID, bk.BookCode, spec.Control, asOf, wm, opts)
		if err != nil {
			return nil, err
		}
		dr, _ := new(big.Rat).SetString(pos.DebitTotal)
		cr, _ := new(big.Rat).SetString(pos.CreditTotal)
		if spec.DebitNormal {
			recomputed = new(big.Rat).Sub(dr, cr)
		} else {
			recomputed = new(big.Rat).Sub(cr, dr)
		}
		groupsOK = true
		if stored, ok := run.PopulationB.Detail["groups"].([]any); ok {
			again := map[string]string{}
			for _, g := range pos.Groups {
				label := g.Key
				if label == "" {
					label = "(none)"
				}
				a, c := dec(g.Debit), dec(g.Credit)
				if spec.DebitNormal {
					again[label] = ratFixed(new(big.Rat).Sub(a, c))
				} else {
					again[label] = ratFixed(new(big.Rat).Sub(c, a))
				}
			}
			for _, row := range stored {
				m, _ := row.(map[string]any)
				key, _ := m["key"].(string)
				actual, _ := m["actual"].(string)
				if again[key] != actual && !(again[key] == "" && dec(actual).Sign() == 0) {
					groupsOK = false
				}
				delete(again, key)
			}
			for _, v := range again {
				if dec(v).Sign() != 0 {
					groupsOK = false
				}
			}
		}
	}
	stored, _ := new(big.Rat).SetString(run.ActualTotal)
	return map[string]any{"reconciliation_run_id": run.RunID, "reproducible": stored.Cmp(recomputed) == 0 && groupsOK, "stored_actual_total": run.ActualTotal, "recomputed_actual_total": ratFixed(recomputed)}, nil
}

// CreateTolerance adds a new version of a tolerance rule.
func (s *ReconService) CreateTolerance(ctx context.Context, tenant string, a Actor, t Tolerance) (*Tolerance, error) {
	if _, ok := controlOf[t.ReconciliationType]; !ok && !ownSided(t.ReconciliationType) {
		return nil, exc(FamData, "RECON_INVALID", "reconciliation_type: %s", reconTypeList)
	}
	if t.LegalEntityID != "" {
		if _, err := uuid.Parse(t.LegalEntityID); err != nil {
			return nil, exc(FamData, "RECON_INVALID", "legal_entity_id is an entity id")
		}
	}
	m, err := parseAmount("match_amount", t.MatchAmount)
	if err != nil {
		return nil, err
	}
	w, err := parseAmount("warning_amount", t.WarningAmount)
	if err != nil {
		return nil, err
	}
	if m.Sign() < 0 || w.Cmp(m) < 0 {
		return nil, exc(FamData, "RECON_INVALID", "0 <= match_amount <= warning_amount")
	}
	t.ToleranceRuleID, t.TenantID, t.CreatedBy, t.CreatedAt = uuid.NewString(), tenant, a.PrincipalID, s.now()
	t.MatchAmount, t.WarningAmount = ratFixed(m), ratFixed(w)
	t.EffectiveFrom = today(t.EffectiveFrom)
	if t.EffectiveFrom.IsZero() || t.EffectiveFrom.Year() < 2000 {
		t.EffectiveFrom = today(s.now())
	}
	err = s.Repo.InsertTolerance(ctx, &t, OutEvent{Fact: "reconciliation.tolerance-set", AggregateType: "reconciliation-tolerance", AggregateID: t.ToleranceRuleID, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"tolerance_rule_id": t.ToleranceRuleID, "reconciliation_type": t.ReconciliationType, "legal_entity_id": t.LegalEntityID,
			"match_amount": t.MatchAmount, "warning_amount": t.WarningAmount, "effective_from": t.EffectiveFrom.Format("2006-01-02")}})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func cleanRefs(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && len(s) <= 500 && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
