// Package arctl is the AR-to-GL control module (spec ZS-SVC-C-001 invariants 15 and 17, section 15 ARControlLink, section 17
// and the section 18 negative paths #21, #35, #36). It owns no receivable truth: it reads the AR modules and the general
// ledger, compares them EXACTLY, and records what it found in append-only tables.
//
// Rules this package keeps:
//   - population completeness is checked document by document, never by a headline total (#21);
//   - every accounting-relevant business fact must have a POSTED queue row (#35);
//   - the subledger is tied to the GL control account(s) per currency; a difference is a finding with both amounts and is
//     NEVER plugged; if the GL cannot be read the run is UNVERIFIED, never a pass;
//   - AR owns no tolerance (#36): it compares exactly and only records the tolerance policy id/version the caller supplied;
//   - a stored run is immutable (trigger) and reproducible: Evaluate(stored inputs) yields the stored findings.
package arctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authz actions (reads use the settled AR read posture: tenant-scoped, no action).
const (
	ActionRunReconciliation   = "AR_CTL_RUN_RECONCILIATION"
	ActionAcknowledgeFinding  = "AR_CTL_ACKNOWLEDGE_FINDING"
	ActionPopulationRead      = "AR_CONTROL_POPULATION_READ" // the existing legacy action, reused by the served populations
	DefaultPostingPendingTTL  = 15 * time.Minute
	DefaultPEPendingEInvoice  = 24 * time.Hour
	MaxFindingsPerRunResponse = 1000
)

// GL mapping key of the AR control account (registered in general-ledger-svc).
const MappingARReceivable = "AR_RECEIVABLE_STANDARD"

// Run results.
const (
	ResultPass       = "PASS"
	ResultBreak      = "BREAK"
	ResultUnverified = "UNVERIFIED"
)

// Finding categories.
const (
	CatPopulation = "POPULATION_COMPLETENESS"
	CatAccounting = "ACCOUNTING_EVENT"
	CatTieOut     = "GL_TIEOUT"
	CatRollup     = "EXCEPTION_ROLLUP"
)

// Finding kinds.
const (
	KindMissingOpenItem    = "MISSING_OPEN_ITEM"
	KindUnexpectedOpenItem = "UNEXPECTED_OPEN_ITEM"
	KindDuplicateOpenItem  = "DUPLICATE_OPEN_ITEM"
	KindAmountMismatch     = "OPEN_ITEM_AMOUNT_MISMATCH"
	KindTotalsMismatch     = "COMPLETENESS_TOTALS_MISMATCH"
	KindPostingMissing     = "POSTING_MISSING"
	KindPostingPending     = "POSTING_PENDING_TOO_LONG"
	KindPostingInFlight    = "POSTING_IN_FLIGHT"
	KindPostingFailed      = "POSTING_FAILED"
	KindPostingQuarantined = "POSTING_QUARANTINED"
	KindGLDifference       = "GL_DIFFERENCE"
	KindGLUnverified       = "GL_UNVERIFIED"
	KindOperational        = "OPERATIONAL_EXCEPTION"
)

// Severities. INFO never changes a result; WARNING and BREAK both stop a PASS.
const (
	SevInfo    = "INFO"
	SevWarning = "WARNING"
	SevBreak   = "BREAK"
)

// Evidence kinds (one row each per run).
const (
	EvCompleteness = "COMPLETENESS"
	EvPosting      = "POSTING"
	EvSubledger    = "SUBLEDGER"
	EvGLRead       = "GL_READ"
	EvExceptions   = "EXCEPTIONS"
	EvPolicy       = "POLICY"
)

// Event types (spec: ar.<aggregate>.<verb>).
const (
	EventRunCompleted  = "ar.control.reconciliation_run_completed"
	EventBreakDetected = "ar.control.reconciliation_break_detected"
	EventFindingAcked  = "ar.control.finding_acknowledged"
	aggregateRun       = "ARControlRun"
	aggregateFinding   = "ARControlFinding"
)

// ExpectedDoc is one issued document that must have exactly one open item.
type ExpectedDoc struct {
	DocumentID   string     `json:"document_id"`
	DocumentKind string     `json:"document_kind"` // INVOICE | DEBIT_NOTE
	Number       string     `json:"number"`
	Currency     string     `json:"currency"`
	Amount       kit.Amount `json:"amount"`
	Origin       string     `json:"origin"` // AR04 | AR06
}

// PostingFact is a business fact with an accounting consequence.
type PostingFact struct {
	SourceType    string     `json:"source_type"`
	SourceID      string     `json:"source_id"`
	SourceEventID string     `json:"source_event_id"`
	Currency      string     `json:"currency"`
	Amount        kit.Amount `json:"amount"`
	Origin        string     `json:"origin"`
	// Legacy invoices post directly (no queue): posted iff JournalID is set.
	Legacy    bool   `json:"legacy,omitempty"`
	JournalID string `json:"journal_id,omitempty"`
}

// QueueRow is the observed state of one ar_accounting_posting_requests row.
type QueueRow struct {
	SourceEventID string    `json:"source_event_id"`
	SourceType    string    `json:"source_type"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	CreatedAt     time.Time `json:"created_at"`
	JournalID     string    `json:"journal_id,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// Subledger is the AR open receivable per currency at the cut-off.
type Subledger struct {
	Available bool                  `json:"available"`
	Error     string                `json:"error,omitempty"`
	Totals    map[string]kit.Amount `json:"totals"`
	Count     int64                 `json:"count"`
	Watermark string                `json:"watermark"`
}

// GLRead is what the GL answered for the AR control account(s). Available=false means UNVERIFIED.
type GLRead struct {
	Available    bool                  `json:"available"`
	Error        string                `json:"error,omitempty"`
	MappingKeys  []string              `json:"mapping_keys"`
	AccountCodes []string              `json:"account_codes,omitempty"`
	PeriodID     string                `json:"period_id"`
	Totals       map[string]kit.Amount `json:"totals"`
	Watermark    string                `json:"watermark"`
}

// ExceptionItem is one visible exception of an owning module.
type ExceptionItem struct {
	Source      string `json:"source"` // AR04 | AR06 | AR08 | AR05 | AR10 | LEGACY
	Kind        string `json:"kind"`
	AggregateID string `json:"aggregate_id"`
	Detail      string `json:"detail"`
}

// ToleranceRef is the tolerance policy the CALLER (financial-control-svc) says applies. AR never evaluates it.
type ToleranceRef struct {
	PolicyID string `json:"policy_id"`
	Version  string `json:"version"`
}

// Inputs is everything a run's findings are derived from. It is stored as evidence, so a run can be re-evaluated.
type Inputs struct {
	LegalEntityID     string                   `json:"legal_entity_id"`
	AsOfDate          string                   `json:"as_of_date"`
	CutoffDate        string                   `json:"cutoff_date"`
	ComputedAt        time.Time                `json:"computed_at"`
	PendingTTLSeconds int64                    `json:"pending_ttl_seconds"`
	Expected          []ExpectedDoc            `json:"expected"`
	Completeness      *ar07.CompletenessReport `json:"completeness"`
	Facts             []PostingFact            `json:"facts"`
	Queue             []QueueRow               `json:"queue"`
	Subledger         Subledger                `json:"subledger"`
	GL                GLRead                   `json:"gl"`
	Exceptions        []ExceptionItem          `json:"exceptions"`
	Tolerance         ToleranceRef             `json:"tolerance"`
}

// Finding is one break/observation. Amounts are exact decimals.
type Finding struct {
	Seq         int            `json:"seq"`
	Category    string         `json:"category"`
	Kind        string         `json:"kind"`
	Code        string         `json:"code"`
	Severity    string         `json:"severity"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Currency    string         `json:"currency,omitempty"`
	ARAmount    *kit.Amount    `json:"ar_amount,omitempty"`
	GLAmount    *kit.Amount    `json:"gl_amount,omitempty"`
	Difference  *kit.Amount    `json:"difference,omitempty"`
	Detail      map[string]any `json:"detail"`
}

func amt(a kit.Amount) *kit.Amount { return &a }

// Evaluate derives the findings from the inputs. It is pure and deterministic (sorted), so Evaluate(stored inputs) always
// reproduces the stored findings, and nothing in it consults a tolerance.
func Evaluate(in *Inputs) []Finding {
	var out []Finding
	add := func(f Finding) {
		f.Seq = len(out) + 1
		if f.Detail == nil {
			f.Detail = map[string]any{}
		}
		out = append(out, f)
	}

	// (a) population completeness: document by document.
	exp := map[string]ExpectedDoc{}
	for _, d := range in.Expected {
		exp[d.DocumentID] = d
	}
	if c := in.Completeness; c != nil {
		for _, id := range c.Missing {
			d := exp[id]
			add(Finding{Category: CatPopulation, Kind: KindMissingOpenItem, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
				SubjectType: d.DocumentKind, SubjectID: id, Currency: d.Currency, ARAmount: amt(0), GLAmount: nil,
				Detail: map[string]any{"expected_amount": d.Amount.String(), "number": d.Number, "origin": d.Origin,
					"explanation": "issued document has no receivable open item"}})
		}
		for _, id := range c.DuplicateDocuments {
			d := exp[id]
			add(Finding{Category: CatPopulation, Kind: KindDuplicateOpenItem, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
				SubjectType: d.DocumentKind, SubjectID: id, Currency: d.Currency,
				Detail: map[string]any{"expected_amount": d.Amount.String(), "explanation": "document appears more than once"}})
		}
		for _, id := range c.Unexpected {
			add(Finding{Category: CatPopulation, Kind: KindUnexpectedOpenItem, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
				SubjectType: "OPEN_ITEM_SOURCE", SubjectID: id,
				Detail: map[string]any{"explanation": "open item has no issued source document"}})
		}
		for _, id := range c.AmountMismatch {
			d := exp[id]
			add(Finding{Category: CatPopulation, Kind: KindAmountMismatch, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
				SubjectType: d.DocumentKind, SubjectID: id, Currency: d.Currency, ARAmount: amt(d.Amount),
				Detail: map[string]any{"explanation": "open item original amount or currency differs from the issued document"}})
		}
		if !c.Balanced && len(c.Missing)+len(c.DuplicateDocuments)+len(c.Unexpected)+len(c.AmountMismatch) == 0 {
			add(Finding{Category: CatPopulation, Kind: KindTotalsMismatch, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
				SubjectType: "POPULATION", SubjectID: in.LegalEntityID,
				Detail: map[string]any{"expected_count": c.ExpectedCount, "open_item_rows": c.OpenItemRows, "distinct": c.OpenItemDistinct}})
		}
	}

	// (b) accounting-event completeness.
	queue := map[string]QueueRow{}
	for _, q := range in.Queue {
		queue[q.SourceEventID] = q
	}
	ttl := time.Duration(in.PendingTTLSeconds) * time.Second
	for _, f := range in.Facts {
		base := Finding{Category: CatAccounting, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak, SubjectType: f.SourceType,
			SubjectID: f.SourceID, Currency: f.Currency, ARAmount: amt(f.Amount),
			Detail: map[string]any{"source_event_id": f.SourceEventID, "origin": f.Origin}}
		if f.Legacy {
			if f.JournalID == "" {
				base.Kind = KindPostingMissing
				base.Detail["explanation"] = "legacy invoice has no issuance journal"
				add(base)
			}
			continue
		}
		q, ok := queue[f.SourceEventID]
		switch {
		case !ok:
			base.Kind = KindPostingMissing
			base.Detail["explanation"] = "no accounting posting request exists for the business fact"
			add(base)
		case q.Status == kit.PostingPosted:
		case q.Status == kit.PostingFailed:
			base.Kind = KindPostingFailed
			base.Detail["last_error"], base.Detail["attempts"] = q.LastError, q.Attempts
			add(base)
		case q.Status == kit.PostingQuarantined:
			base.Kind = KindPostingQuarantined
			base.Detail["last_error"] = q.LastError
			add(base)
		default: // PENDING / IN_PROGRESS
			base.Detail["status"], base.Detail["queued_at"] = q.Status, q.CreatedAt.UTC().Format(time.RFC3339)
			if in.ComputedAt.Sub(q.CreatedAt) > ttl {
				base.Kind = KindPostingPending
			} else {
				base.Kind, base.Severity = KindPostingInFlight, SevInfo
			}
			add(base)
		}
	}

	// (c) subledger <-> GL tie-out per currency.
	switch {
	case !in.GL.Available:
		add(Finding{Category: CatTieOut, Kind: KindGLUnverified, Code: kit.CodeARGLReconciliationBreak, Severity: SevWarning,
			SubjectType: "GL_CONTROL_ACCOUNT", SubjectID: in.LegalEntityID,
			Detail: map[string]any{"explanation": "the GL control-account balance could not be read; the tie-out is UNVERIFIED, not passed", "error": in.GL.Error}})
	case !in.Subledger.Available:
		add(Finding{Category: CatTieOut, Kind: KindGLUnverified, Code: kit.CodeARGLReconciliationBreak, Severity: SevWarning,
			SubjectType: "AR_SUBLEDGER", SubjectID: in.LegalEntityID,
			Detail: map[string]any{"explanation": "the AR open-receivable total could not be read; the tie-out is UNVERIFIED", "error": in.Subledger.Error}})
	default:
		cur := map[string]bool{}
		for c := range in.Subledger.Totals {
			cur[c] = true
		}
		for c := range in.GL.Totals {
			cur[c] = true
		}
		cs := make([]string, 0, len(cur))
		for c := range cur {
			cs = append(cs, c)
		}
		sort.Strings(cs)
		for _, c := range cs {
			ar, gl := in.Subledger.Totals[c], in.GL.Totals[c]
			if ar != gl {
				add(Finding{Category: CatTieOut, Kind: KindGLDifference, Code: kit.CodeARGLReconciliationBreak, Severity: SevBreak,
					SubjectType: "GL_CONTROL_ACCOUNT", SubjectID: in.LegalEntityID, Currency: c,
					ARAmount: amt(ar), GLAmount: amt(gl), Difference: amt(ar.Sub(gl)),
					Detail: map[string]any{"mapping_keys": in.GL.MappingKeys, "account_codes": in.GL.AccountCodes, "period_id": in.GL.PeriodID,
						"explanation": "exact comparison; no tolerance is applied by AR and nothing is plugged"}})
			}
		}
	}

	// (d) operational exception roll-up.
	ex := append([]ExceptionItem(nil), in.Exceptions...)
	sort.SliceStable(ex, func(i, j int) bool {
		a, b := ex[i], ex[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.AggregateID < b.AggregateID
	})
	for _, e := range ex {
		add(Finding{Category: CatRollup, Kind: KindOperational, Code: e.Kind, Severity: SevWarning, SubjectType: e.Source, SubjectID: e.AggregateID,
			Detail: map[string]any{"detail": e.Detail}})
	}
	return out
}

// ResultOf is BREAK when any blocking finding other than the unverified marker exists, UNVERIFIED when the tie-out could
// not be established, else PASS. An unreadable GL can therefore never yield PASS.
func ResultOf(fs []Finding) string {
	unverified := false
	for _, f := range fs {
		if f.Severity == SevInfo {
			continue
		}
		if f.Kind == KindGLUnverified {
			unverified = true
			continue
		}
		return ResultBreak
	}
	if unverified {
		return ResultUnverified
	}
	return ResultPass
}

// Summary is the run's headline, derived from the findings and inputs.
type Summary struct {
	Counts          map[string]int        `json:"finding_counts_by_category"`
	BreakCount      int                   `json:"blocking_findings"`
	ExpectedCount   int                   `json:"expected_documents"`
	OpenItemRows    int                   `json:"open_item_rows"`
	FactsChecked    int                   `json:"accounting_facts_checked"`
	PostingByStatus map[string]int        `json:"posting_status_counts"`
	ARTotals        map[string]kit.Amount `json:"ar_totals"`
	GLTotals        map[string]kit.Amount `json:"gl_totals"`
	GLVerified      bool                  `json:"gl_verified"`
}

func SummaryOf(in *Inputs, fs []Finding) Summary {
	s := Summary{Counts: map[string]int{}, PostingByStatus: map[string]int{}, ExpectedCount: len(in.Expected), FactsChecked: len(in.Facts),
		ARTotals: in.Subledger.Totals, GLTotals: in.GL.Totals, GLVerified: in.GL.Available && in.Subledger.Available}
	if in.Completeness != nil {
		s.OpenItemRows = in.Completeness.OpenItemRows
	}
	for _, f := range fs {
		s.Counts[f.Category]++
		if f.Severity != SevInfo {
			s.BreakCount++
		}
	}
	q := map[string]QueueRow{}
	for _, r := range in.Queue {
		q[r.SourceEventID] = r
	}
	for _, f := range in.Facts {
		switch {
		case f.Legacy && f.JournalID != "":
			s.PostingByStatus[kit.PostingPosted]++
		case f.Legacy:
			s.PostingByStatus["MISSING"]++
		default:
			if r, ok := q[f.SourceEventID]; ok {
				s.PostingByStatus[r.Status]++
			} else {
				s.PostingByStatus["MISSING"]++
			}
		}
	}
	if s.ARTotals == nil {
		s.ARTotals = map[string]kit.Amount{}
	}
	if s.GLTotals == nil {
		s.GLTotals = map[string]kit.Amount{}
	}
	return s
}

// hashJSON is the SHA-256 of the canonical (encoding/json, sorted map keys) encoding of v.
func hashJSON(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), b, nil
}

// splitEvidence breaks the inputs into the evidence rows stored for a run, and joinEvidence reverses it.
func splitEvidence(in *Inputs) map[string]any {
	return map[string]any{
		EvCompleteness: map[string]any{"expected": in.Expected, "report": in.Completeness},
		EvPosting:      map[string]any{"facts": in.Facts, "queue": in.Queue, "pending_ttl_seconds": in.PendingTTLSeconds, "computed_at": in.ComputedAt},
		EvSubledger:    in.Subledger,
		EvGLRead:       in.GL,
		EvExceptions:   in.Exceptions,
		EvPolicy:       map[string]any{"tolerance": in.Tolerance, "legal_entity_id": in.LegalEntityID, "as_of_date": in.AsOfDate, "cutoff_date": in.CutoffDate},
	}
}

func joinEvidence(raw map[string][]byte) (*Inputs, error) {
	in := &Inputs{}
	var c struct {
		Expected []ExpectedDoc            `json:"expected"`
		Report   *ar07.CompletenessReport `json:"report"`
	}
	var p struct {
		Facts      []PostingFact `json:"facts"`
		Queue      []QueueRow    `json:"queue"`
		TTL        int64         `json:"pending_ttl_seconds"`
		ComputedAt time.Time     `json:"computed_at"`
	}
	var pol struct {
		Tolerance     ToleranceRef `json:"tolerance"`
		LegalEntityID string       `json:"legal_entity_id"`
		AsOf          string       `json:"as_of_date"`
		Cutoff        string       `json:"cutoff_date"`
	}
	for kind, dst := range map[string]any{EvCompleteness: &c, EvPosting: &p, EvSubledger: &in.Subledger, EvGLRead: &in.GL, EvExceptions: &in.Exceptions, EvPolicy: &pol} {
		b, ok := raw[kind]
		if !ok {
			return nil, fmt.Errorf("evidence %s is missing", kind)
		}
		if err := json.Unmarshal(b, dst); err != nil {
			return nil, fmt.Errorf("evidence %s: %w", kind, err)
		}
	}
	in.Expected, in.Completeness = c.Expected, c.Report
	in.Facts, in.Queue, in.PendingTTLSeconds, in.ComputedAt = p.Facts, p.Queue, p.TTL, p.ComputedAt
	in.Tolerance, in.LegalEntityID, in.AsOfDate, in.CutoffDate = pol.Tolerance, pol.LegalEntityID, pol.AsOf, pol.Cutoff
	return in, nil
}

// CutoffOf is the date both sides are compared at: the GL control-account read is per period (month), so the cut-off is the
// last day of the as-of date's month. It is recorded on the run.
func CutoffOf(asOf time.Time) time.Time {
	return time.Date(asOf.Year(), asOf.Month()+1, 0, 0, 0, 0, 0, time.UTC)
}

// PeriodOf is the YYYY-MM period id of a date.
func PeriodOf(asOf time.Time) string { return asOf.Format("2006-01") }
