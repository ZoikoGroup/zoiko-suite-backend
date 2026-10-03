//go:build integration

package store_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ---- fixtures ----

type w5Opts struct {
	source     *string // journal_headers.source_event_id
	approvedBy *string // journal_headers.approved_by_principal_id (nil leaves NULL)
	finalize   bool
}

func strp(s string) *string { return &s }

// w5Journal creates a journal (lines must balance) in the given tenant/entity.
// The store's normal CreateJournal + TransitionJournal path is used, so a
// finalized journal really has ledger_entries; approved_by is set with direct
// SQL because the store has no setter outside the approval workflow.
func (f *w4Fixture) w5Journal(t *testing.T, tenant, entity, period, currency string, txn domain.Date, o w5Opts, lines ...domain.JournalLine) string {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenant, LegalEntityID: entity,
		FiscalPeriod: period, Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: txn,
		PostingDate: txn, CurrencyCode: currency,
		CreatedByPrincipalID: "preparer-1", CorrelationID: uuid.New().String(),
		ApprovalStatus: domain.ApprovalStatusPosted, SourceEventID: o.source,
	}
	if _, _, err := f.s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}
	if o.approvedBy != nil {
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE journal_headers SET approved_by_principal_id=$1 WHERE tenant_id=$2 AND journal_id=$3`,
			*o.approvedBy, tenant, h.JournalID); err != nil {
			t.Fatalf("set approved_by: %v", err)
		}
	}
	if o.finalize {
		for _, step := range [][2]domain.JournalStatus{
			{domain.JournalStatusPending, domain.JournalStatusValidated},
			{domain.JournalStatusValidated, domain.JournalStatusFinalized},
		} {
			if err := f.s.TransitionJournal(ctx, tenant, h.JournalID, step[0], step[1], "preparer-1"); err != nil {
				t.Fatalf("transition to %s: %v", step[1], err)
			}
		}
	}
	return h.JournalID
}

func (f *w4Fixture) manual(t *testing.T, period string, txn domain.Date, lines ...domain.JournalLine) string {
	t.Helper()
	return f.w5Journal(t, f.tenant, f.entity, period, "USD", txn, w5Opts{finalize: true}, lines...)
}

func (f *w4Fixture) account(t *testing.T, tenant, code string, control, restricted bool) {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	err := f.s.CreateAccount(ctx, &domain.Account{
		AccountID: uuid.New().String(), TenantID: tenant, AccountCode: code, AccountName: "acct " + code,
		AccountType: domain.AccountTypeLiability, IsControlAccount: control, DirectPostingRestricted: restricted,
		Status: "ACTIVE", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "admin",
	})
	if err != nil {
		t.Fatalf("CreateAccount %s: %v", code, err)
	}
}

func (f *w4Fixture) fp(t *testing.T, pop, period string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	q := domain.FiscalPeriodPopulationQuery{LegalEntityID: f.entity, FiscalPeriod: period, Limit: limit, AfterRecordID: after}
	var p *domain.ControlPopulationPage
	var err error
	switch pop {
	case "journal-balances":
		p, err = f.s.QueryJournalBalances(f.ctx, f.tenant, q)
	case "control-account-postings":
		p, err = f.s.QueryControlAccountPostings(f.ctx, f.tenant, q)
	case "manual-journals":
		p, err = f.s.QueryManualJournals(f.ctx, f.tenant, q)
	default:
		t.Fatalf("unknown population %s", pop)
	}
	if err != nil {
		t.Fatalf("%s: %v", pop, err)
	}
	return p
}

// pageAll walks every page with a small limit, asserting a stable watermark,
// ascending unique record ids and a row_count that equals what was visited.
func (f *w4Fixture) pageAll(t *testing.T, pop, period string, limit int) (map[string]domain.ControlPopulationRecord, *domain.ControlPopulationPage) {
	t.Helper()
	seen := map[string]domain.ControlPopulationRecord{}
	var first *domain.ControlPopulationPage
	var prev string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("paging did not terminate")
		}
		p := f.fp(t, pop, period, limit, after)
		if first == nil {
			first = p
		} else if p.Watermark != first.Watermark {
			t.Fatalf("watermark changed between pages: %q vs %q", first.Watermark, p.Watermark)
		}
		for _, r := range p.Records {
			if _, dup := seen[r.RecordID]; dup {
				t.Fatalf("record %s served twice", r.RecordID)
			}
			if prev != "" && r.RecordID <= prev {
				t.Fatalf("not ascending: %s after %s", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = r
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
	}
	if int64(len(seen)) != first.DeclaredTotals.RowCount {
		t.Fatalf("visited %d rows, declared row_count %d", len(seen), first.DeclaredTotals.RowCount)
	}
	return seen, first
}

func sumAmounts(t *testing.T, recs map[string]domain.ControlPopulationRecord, cur string) string {
	t.Helper()
	sum := new(big.Rat)
	for _, r := range recs {
		if r.Currency != cur {
			continue
		}
		v, ok := new(big.Rat).SetString(r.Amount)
		if !ok {
			t.Fatalf("amount %q not decimal", r.Amount)
		}
		sum.Add(sum, v)
	}
	return sum.FloatString(2)
}

// ---- journal-balances ----

func TestJournalBalances_PerJournalCurrency_ExactSums_ImbalanceVisible(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 3)
	bal := f.manual(t, "2026-09", d, dr("1200", 100.10), cr("4000", 100.10))
	multi := f.manual(t, "2026-09", domain.NewDate(2026, 9, 7), dr("1200", 0.10), dr("1210", 0.20), cr("4000", 0.30))
	skewed := f.manual(t, "2026-09", d, dr("1200", 10), cr("4000", 10))
	// A debit-only stray entry (legacy feed) makes 'skewed' unbalanced in the ledger.
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO ledger_entries (ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
			journal_id, journal_line_id, line_number, account_code, debit_amount, credit_amount, currency_code,
			transaction_date, posting_date, correlation_id, created_at)
		VALUES ($1,$2,$3,'','2026-09',$4,$5,99,'1200',4.00,0,'USD','2026-09-03','2026-09-03','corr-x', now())`,
		uuid.New().String(), f.tenant, f.entity, skewed, uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	// Excluded: other period (incl. near-miss text), other tenant, other entity.
	f.manual(t, "2026-10", d, dr("1200", 500), cr("4000", 500))
	f.manual(t, "2026-09 ", d, dr("1200", 600), cr("4000", 600))
	f.journalAs(t, uuid.New().String(), f.entity, "2026-09", "USD", d, dr("1200", 99), cr("4000", 99))
	f.journalAs(t, f.tenant, uuid.New().String(), "2026-09", "USD", d, dr("1200", 98), cr("4000", 98))

	p := f.fp(t, "journal-balances", "2026-09", 100, "")
	if p.DeclaredTotals.RowCount != 3 || len(p.Records) != 3 {
		t.Fatalf("want 3 journals, got %+v", p.Records)
	}
	m := byID(p)
	b := m[bal+":USD"]
	if b.Reference != bal || b.Amount != "100.10" || b.Currency != "USD" || b.Date != "2026-09-03" ||
		b.Attributes["debit_total"] != "100.10" || b.Attributes["credit_total"] != "100.10" || b.Attributes["entry_count"] != "2" {
		t.Fatalf("balanced journal wrong: %+v", b)
	}
	mu := m[multi+":USD"]
	if mu.Amount != "0.30" || mu.Attributes["debit_total"] != "0.30" || mu.Attributes["credit_total"] != "0.30" ||
		mu.Attributes["entry_count"] != "3" || mu.Date != "2026-09-07" {
		t.Fatalf("0.1+0.2 must be exactly 0.30: %+v", mu)
	}
	sk := m[skewed+":USD"]
	if sk.Attributes["debit_total"] != "14.00" || sk.Attributes["credit_total"] != "10.00" || sk.Amount != "14.00" {
		t.Fatalf("imbalance must be visible in attributes: %+v", sk)
	}
	if p.DeclaredTotals.Totals["USD"] != "114.40" || len(p.DeclaredTotals.Totals) != 1 {
		t.Fatalf("declared totals = %v, want USD 114.40", p.DeclaredTotals.Totals)
	}
	if !strings.HasPrefix(p.Watermark, "gl4:n=3;entry_seq=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}
}

func TestJournalBalances_TwoCurrencyJournal_TwoRecords_SameReference(t *testing.T) {
	f := newW4Fixture(t)
	j := f.manual(t, "2026-09", domain.NewDate(2026, 9, 1), dr("1200", 10), cr("4000", 10))
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO ledger_entries (ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
			journal_id, journal_line_id, line_number, account_code, debit_amount, credit_amount, currency_code,
			transaction_date, posting_date, correlation_id, created_at)
		VALUES ($1,$2,$3,'','2026-09',$4,$5,99,'1200',4.00,0,'EUR','2026-09-02','2026-09-02','corr-x', now())`,
		uuid.New().String(), f.tenant, f.entity, j, uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	p := f.fp(t, "journal-balances", "2026-09", 100, "")
	m := byID(p)
	if len(m) != 2 || m[j+":USD"].Reference != j || m[j+":EUR"].Reference != j ||
		m[j+":EUR"].Amount != "4.00" || m[j+":EUR"].Attributes["credit_total"] != "0.00" {
		t.Fatalf("two-currency journal must be two distinct records: %+v", p.Records)
	}
	if p.DeclaredTotals.Totals["USD"] != "10.00" || p.DeclaredTotals.Totals["EUR"] != "4.00" {
		t.Fatalf("totals per currency: %v", p.DeclaredTotals.Totals)
	}
}

func TestJournalBalances_Paging_Watermark_Isolation(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	for i := 0; i < 5; i++ {
		f.manual(t, "2026-09", d, dr("1200", float64(10+i)+0.1), cr("4000", float64(10+i)+0.1))
	}
	f.journalAs(t, uuid.New().String(), f.entity, "2026-09", "USD", d, dr("1200", 1), cr("4000", 1))
	seen, first := f.pageAll(t, "journal-balances", "2026-09", 2)
	if len(seen) != 5 {
		t.Fatalf("visited %d, want 5", len(seen))
	}
	if got := sumAmounts(t, seen, "USD"); got != "60.50" || first.DeclaredTotals.Totals["USD"] != "60.50" {
		t.Fatalf("sum %s / declared %v, want 60.50", got, first.DeclaredTotals.Totals)
	}
	again := f.fp(t, "journal-balances", "2026-09", 100, "")
	if again.Watermark != first.Watermark {
		t.Fatal("watermark must be stable when nothing changed")
	}
	f.manual(t, "2026-10", d, dr("1200", 1), cr("4000", 1))
	if other := f.fp(t, "journal-balances", "2026-09", 100, ""); other.Watermark != first.Watermark {
		t.Fatal("a posting in another period must not change the watermark")
	}
	f.manual(t, "2026-09", d, dr("1200", 1), cr("4000", 1))
	if changed := f.fp(t, "journal-balances", "2026-09", 100, ""); changed.Watermark == first.Watermark {
		t.Fatal("a new posting in the period must change the watermark")
	}
	foreign, err := f.s.QueryJournalBalances(svcmiddleware.WithTenant(context.Background(), uuid.New().String()),
		uuid.New().String(), domain.FiscalPeriodPopulationQuery{LegalEntityID: f.entity, FiscalPeriod: "2026-09", Limit: 10})
	if err != nil || foreign.DeclaredTotals.RowCount != 0 || len(foreign.Records) != 0 {
		t.Fatalf("a foreign tenant saw rows: %v %+v", err, foreign)
	}
}

// ---- control-account-postings ----

func TestControlAccountPostings_ViolationSet(t *testing.T) {
	f := newW4Fixture(t)
	f.account(t, f.tenant, "2000", true, true)   // control + restricted: in scope
	f.account(t, f.tenant, "2100", true, false)  // control, not restricted: out
	f.account(t, f.tenant, "2200", false, true)  // restricted, not control: out
	f.account(t, f.tenant, "1200", false, false) // ordinary
	d := domain.NewDate(2026, 9, 4)

	// Violation: manual journal crediting the control account, approved by someone.
	bad := f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", d,
		w5Opts{approvedBy: strp("approver-1"), finalize: true}, dr("1200", 50), cr("2000", 50))
	// Violation: debit side, blank (not NULL) source_event_id is treated as manual.
	blank := f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", d, w5Opts{source: strp(""), finalize: true}, dr("2000", 7.25), cr("1200", 7.25))
	// Not violations.
	f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", d, w5Opts{source: strp("INV-1"), finalize: true}, dr("1200", 20), cr("2000", 20)) // subledger-fed
	f.manual(t, "2026-09", d, dr("1200", 30), cr("2100", 30))                                                                              // not restricted
	f.manual(t, "2026-09", d, dr("1200", 31), cr("2200", 31))                                                                              // not control
	f.manual(t, "2026-09", d, dr("1200", 32), cr("4000", 32))                                                                              // unrelated
	f.manual(t, "2026-10", d, dr("1200", 33), cr("2000", 33))                                                                              // other period
	// Other tenant: same account code, made control+restricted there too.
	other := uuid.New().String()
	f.account(t, other, "2000", true, true)
	f.journalAs(t, other, f.entity, "2026-09", "USD", d, dr("1200", 44), cr("2000", 44))
	// Same tenant, other entity.
	f.journalAs(t, f.tenant, uuid.New().String(), "2026-09", "USD", d, dr("1200", 45), cr("2000", 45))

	p := f.fp(t, "control-account-postings", "2026-09", 100, "")
	if p.DeclaredTotals.RowCount != 2 || len(p.Records) != 2 {
		t.Fatalf("want 2 violating entries, got %+v", p.Records)
	}
	var credit, debit domain.ControlPopulationRecord
	for _, r := range p.Records {
		if r.Reference == bad {
			credit = r
		}
		if r.Reference == blank {
			debit = r
		}
	}
	if credit.RecordID == "" || credit.Amount != "-50.00" || credit.Currency != "USD" || credit.Date != "2026-09-04" ||
		credit.Attributes["account_code"] != "2000" || credit.Attributes["journal_id"] != bad ||
		credit.Attributes["journal_type"] != "STANDARD" || credit.Attributes["created_by"] != "preparer-1" ||
		credit.Attributes["approved_by"] != "approver-1" {
		t.Fatalf("credit violation wrong: %+v", credit)
	}
	if _, err := uuid.Parse(credit.RecordID); err != nil {
		t.Fatalf("record_id must be the ledger entry id: %q", credit.RecordID)
	}
	if debit.Amount != "7.25" || debit.Attributes["approved_by"] != "" {
		t.Fatalf("debit violation wrong (signed net, empty approver): %+v", debit)
	}
	if p.DeclaredTotals.Totals["USD"] != "-42.75" {
		t.Fatalf("declared total = %v, want USD -42.75", p.DeclaredTotals.Totals)
	}
	if !strings.HasPrefix(p.Watermark, "gl5:n=2;entry_seq=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}
}

func TestControlAccountPostings_Paging_Watermark_Isolation(t *testing.T) {
	f := newW4Fixture(t)
	f.account(t, f.tenant, "2000", true, true)
	d := domain.NewDate(2026, 9, 1)
	for i := 0; i < 5; i++ {
		f.manual(t, "2026-09", d, dr("1200", float64(i)+1.5), cr("2000", float64(i)+1.5))
	}
	seen, first := f.pageAll(t, "control-account-postings", "2026-09", 2)
	if len(seen) != 5 {
		t.Fatalf("visited %d, want 5", len(seen))
	}
	if got := sumAmounts(t, seen, "USD"); got != "-17.50" || first.DeclaredTotals.Totals["USD"] != "-17.50" {
		t.Fatalf("sum %s / declared %v, want -17.50", got, first.DeclaredTotals.Totals)
	}
	f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", d, w5Opts{source: strp("INV-9"), finalize: true}, dr("1200", 1), cr("2000", 1))
	if same := f.fp(t, "control-account-postings", "2026-09", 100, ""); same.Watermark != first.Watermark {
		t.Fatal("a subledger-fed posting is outside the population and must not change the watermark")
	}
	f.manual(t, "2026-09", d, dr("1200", 1), cr("2000", 1))
	if changed := f.fp(t, "control-account-postings", "2026-09", 100, ""); changed.Watermark == first.Watermark {
		t.Fatal("a new violation must change the watermark")
	}
}

// ---- unposted-events ----

func (f *w4Fixture) execution(t *testing.T, tenant, entity, status string, source *string, created time.Time) string {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	e := &domain.PostingExecution{
		ExecutionID: uuid.New().String(), TenantID: tenant, LegalEntityID: entity, Kind: domain.PostingExecutionKindEvent,
		SourceEventID: source, Status: status, CalculationTrace: "{}", CorrelationID: uuid.New().String(),
		CreatedAt: created, CreatedByPrincipalID: "svc-1",
	}
	if err := f.s.CreatePostingExecution(ctx, e); err != nil {
		t.Fatalf("CreatePostingExecution: %v", err)
	}
	return e.ExecutionID
}

func (f *w4Fixture) unposted(t *testing.T, before time.Time, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryUnpostedEvents(f.ctx, f.tenant, domain.UnpostedEventsQuery{
		LegalEntityID: f.entity, CreatedBefore: before, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryUnpostedEvents: %v", err)
	}
	return p
}

func TestUnpostedEvents_NotCommittedBeforeCutoff_DeterministicAge(t *testing.T) {
	f := newW4Fixture(t)
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(day, hour int) time.Time { return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC) }

	submitted := f.execution(t, f.tenant, f.entity, "SUBMITTED", strp("EVT-1"), at(20, 12))    // 10.5 days -> 10
	validating := f.execution(t, f.tenant, f.entity, "VALIDATING", strp("EVT-2"), at(30, 23))  // 0.04 days -> 0
	ready := f.execution(t, f.tenant, f.entity, "READY", nil, at(1, 0))                        // 30 days, no source event
	blankSrc := f.execution(t, f.tenant, f.entity, "READY", strp(""), at(2, 0))                // 29 days, blank source
	failed := f.execution(t, f.tenant, f.entity, "FAILED", strp("EVT-3"), at(10, 6))           // 20.75 days -> 20
	quarantined := f.execution(t, f.tenant, f.entity, "QUARANTINED", strp("EVT-4"), at(15, 0)) // 16 days
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE posting_executions SET failure_reason=$1 WHERE tenant_id=$2 AND execution_id=$3`,
		strings.Repeat("x", 300), f.tenant, failed); err != nil {
		t.Fatal(err)
	}
	// Excluded.
	f.execution(t, f.tenant, f.entity, "COMMITTED", strp("EVT-5"), at(5, 0))
	f.execution(t, f.tenant, f.entity, "SUBMITTED", strp("EVT-6"), cutoff)                  // exactly at cutoff: strict <
	f.execution(t, f.tenant, f.entity, "SUBMITTED", strp("EVT-7"), cutoff.Add(time.Second)) // after
	f.execution(t, uuid.New().String(), f.entity, "FAILED", strp("EVT-8"), at(5, 0))        // other tenant
	f.execution(t, f.tenant, uuid.New().String(), "FAILED", strp("EVT-9"), at(5, 0))        // other entity

	p := f.unposted(t, cutoff, 100, "")
	if p.DeclaredTotals.RowCount != 6 || len(p.Records) != 6 {
		t.Fatalf("want 6 unposted executions, got %d: %+v", len(p.Records), p.Records)
	}
	m := byID(p)
	wantAge := map[string]string{submitted: "10", validating: "0", ready: "30", blankSrc: "29", failed: "20", quarantined: "16"}
	for id, age := range wantAge {
		r, ok := m[id]
		if !ok {
			t.Fatalf("missing execution %s", id)
		}
		if r.Attributes["age_days"] != age || r.Amount != "0" || r.Currency != "XXX" {
			t.Fatalf("%s: age_days=%q amount=%q currency=%q, want %s/0/XXX", id, r.Attributes["age_days"], r.Amount, r.Currency, age)
		}
	}
	if r := m[submitted]; r.Reference != "EVT-1" || r.Date != "2026-09-20" || r.Attributes["status"] != "SUBMITTED" ||
		r.Attributes["kind"] != "EVENT" || r.Attributes["failure_reason"] != "" {
		t.Fatalf("submitted record wrong: %+v", r)
	}
	if m[ready].Reference != ready || m[blankSrc].Reference != blankSrc {
		t.Fatal("reference must fall back to execution_id when source_event_id is null or blank")
	}
	if fr := m[failed].Attributes["failure_reason"]; fr != strings.Repeat("x", 200) {
		t.Fatalf("failure_reason must be truncated to 200 characters, got %d runes", len([]rune(fr)))
	}
	if p.DeclaredTotals.Totals["XXX"] != "0" || len(p.DeclaredTotals.Totals) != 1 {
		t.Fatalf("declared totals = %v", p.DeclaredTotals.Totals)
	}
	if !strings.HasPrefix(p.Watermark, "gl6:n=6;entry_seq=0;md5=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}

	// Determinism: the same instant expressed in another zone is identical
	// (including the watermark), and age_days follows created_before, not now().
	est := time.FixedZone("EST", -5*3600)
	same := f.unposted(t, cutoff.In(est), 100, "")
	if same.Watermark != p.Watermark {
		t.Fatalf("same instant, different watermark: %q vs %q", same.Watermark, p.Watermark)
	}
	far := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	farPage := f.unposted(t, far, 100, "")
	wantFar := int(far.Sub(at(20, 12)).Hours() / 24)
	if got := byID(farPage)[submitted].Attributes["age_days"]; got != itoa64(int64(wantFar)) {
		t.Fatalf("age_days against a 2099 cut-off = %s, want %d", got, wantFar)
	}
	if farPage.DeclaredTotals.RowCount != 8 {
		t.Fatalf("2099 cut-off should include the after-cutoff rows of this tenant/entity: %d", farPage.DeclaredTotals.RowCount)
	}
	if farPage.Watermark == p.Watermark {
		t.Fatal("a different scope must produce a different watermark")
	}
}

func TestUnpostedEvents_Paging_Watermark(t *testing.T) {
	f := newW4Fixture(t)
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		f.execution(t, f.tenant, f.entity, "FAILED", nil, time.Date(2026, 9, 1+i, 0, 0, 0, 0, time.UTC))
	}
	seen := map[string]bool{}
	var wm, prev string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		p := f.unposted(t, cutoff, 2, after)
		if wm == "" {
			wm = p.Watermark
		} else if p.Watermark != wm {
			t.Fatal("watermark changed between pages")
		}
		if p.DeclaredTotals.RowCount != 5 {
			t.Fatalf("row_count = %d", p.DeclaredTotals.RowCount)
		}
		for _, r := range p.Records {
			if seen[r.RecordID] || (prev != "" && r.RecordID <= prev) {
				t.Fatalf("record %s repeated or out of order", r.RecordID)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
	}
	if len(seen) != 5 {
		t.Fatalf("visited %d, want 5", len(seen))
	}
	// A status change (FAILED -> COMMITTED) removes the row and changes the watermark.
	var id string
	for k := range seen {
		id = k
		break
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	after2 := f.unposted(t, cutoff, 100, "")
	if after2.Watermark == wm || after2.DeclaredTotals.RowCount != 4 {
		t.Fatalf("a committed execution must leave the population: %+v", after2.DeclaredTotals)
	}
	foreign, err := f.s.QueryUnpostedEvents(svcmiddleware.WithTenant(context.Background(), uuid.New().String()),
		uuid.New().String(), domain.UnpostedEventsQuery{LegalEntityID: f.entity, CreatedBefore: cutoff, Limit: 10})
	if err != nil || foreign.DeclaredTotals.RowCount != 0 {
		t.Fatalf("a foreign tenant saw rows: %v %+v", err, foreign)
	}
}

// ---- manual-journals ----

func TestManualJournals_StatusSourceAndReviewGap(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 9)
	mk := func(o w5Opts, lines ...domain.JournalLine) string {
		return f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", d, o, lines...)
	}
	clean := mk(w5Opts{approvedBy: strp("approver-1"), finalize: true}, dr("1200", 70.05), cr("4000", 70.05))
	noApprover := mk(w5Opts{finalize: true}, dr("1200", 10), dr("1210", 5.5), cr("4000", 15.5))
	emptyApprover := mk(w5Opts{approvedBy: strp(""), finalize: true}, dr("1200", 1), cr("4000", 1))
	self := mk(w5Opts{approvedBy: strp("preparer-1"), finalize: true}, dr("1200", 2), cr("4000", 2))
	// Reverse `self`'s twin to get a REVERSED original plus its reversal (both manual).
	orig := mk(w5Opts{approvedBy: strp("approver-2"), finalize: true}, dr("1200", 3), cr("4000", 3))
	rev := f.reverseViaStore(t, orig, "2026-09", "USD", d, dr("1200", 3), cr("4000", 3))
	// Excluded.
	mk(w5Opts{source: strp("INV-1"), finalize: true}, dr("1200", 4), cr("4000", 4)) // subledger-fed
	mk(w5Opts{}, dr("1200", 5), cr("4000", 5))                                      // PENDING, not FINALIZED
	f.w5Journal(t, f.tenant, f.entity, "2026-10", "USD", d, w5Opts{finalize: true}, dr("1200", 6), cr("4000", 6))
	f.w5Journal(t, uuid.New().String(), f.entity, "2026-09", "USD", d, w5Opts{finalize: true}, dr("1200", 7), cr("4000", 7))
	f.w5Journal(t, f.tenant, uuid.New().String(), "2026-09", "USD", d, w5Opts{finalize: true}, dr("1200", 8), cr("4000", 8))

	p := f.fp(t, "manual-journals", "2026-09", 100, "")
	m := byID(p)
	if _, ok := m[rev]; !ok {
		t.Logf("reversal journal %s absent from manual-journals (status/source differs); population has %d rows", rev, len(m))
	}
	for _, id := range []string{clean, noApprover, emptyApprover, self, orig} {
		if _, ok := m[id]; !ok {
			t.Fatalf("journal %s missing from population %+v", id, p.Records)
		}
	}
	c := m[clean]
	if c.Reference != clean || c.Amount != "70.05" || c.Currency != "USD" || c.Date != "2026-09-09" ||
		c.Attributes["journal_type"] != "STANDARD" || c.Attributes["created_by"] != "preparer-1" ||
		c.Attributes["approved_by"] != "approver-1" || c.Attributes["approval_status"] != "POSTED" ||
		c.Attributes["review_gap"] != "" || c.Attributes["status"] != "FINALIZED" {
		t.Fatalf("clean journal wrong: %+v", c)
	}
	if n := m[noApprover]; n.Amount != "15.50" || n.Attributes["review_gap"] != "NO_APPROVER" || n.Attributes["approved_by"] != "" {
		t.Fatalf("NULL approver wrong (amount = total debit): %+v", n)
	}
	if e := m[emptyApprover]; e.Attributes["review_gap"] != "NO_APPROVER" {
		t.Fatalf("empty approver must be NO_APPROVER: %+v", e)
	}
	if s := m[self]; s.Attributes["review_gap"] != "SELF_APPROVED" || s.Attributes["approved_by"] != "preparer-1" {
		t.Fatalf("self approval wrong: %+v", s)
	}
	if o := m[orig]; o.Attributes["review_gap"] != "" {
		t.Fatalf("original wrong: %+v", o)
	}
	// Exactly the five explicit ones plus (possibly) the reversal journal.
	want := int64(5)
	if _, ok := m[rev]; ok {
		want = 6
	}
	if p.DeclaredTotals.RowCount != want || int64(len(m)) != want {
		t.Fatalf("row_count = %d (visible %d), want %d", p.DeclaredTotals.RowCount, len(m), want)
	}
	if !strings.HasPrefix(p.Watermark, "gl7:n=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}
	var origStatus string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM journal_headers WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, orig).Scan(&origStatus); err != nil {
		t.Fatal(err)
	}
	if m[orig].Attributes["status"] != origStatus {
		t.Fatalf("status attribute %q does not match stored status %q", m[orig].Attributes["status"], origStatus)
	}
	t.Logf("reversed original stored status=%s", origStatus)
}

func TestManualJournals_ReversedStatusIncluded(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 9)
	j := f.manual(t, "2026-09", d, dr("1200", 9), cr("4000", 9))
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE journal_headers SET status='REVERSED' WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, j); err != nil {
		t.Skipf("cannot force REVERSED status: %v", err)
	}
	m := byID(f.fp(t, "manual-journals", "2026-09", 100, ""))
	if r, ok := m[j]; !ok || r.Attributes["status"] != "REVERSED" {
		t.Fatalf("a REVERSED manual journal must be reported and marked: %+v", r)
	}
}

func TestManualJournals_Paging_Watermark_Isolation(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	for i := 0; i < 5; i++ {
		f.manual(t, "2026-09", d, dr("1200", float64(i)+1.25), cr("4000", float64(i)+1.25))
	}
	f.journalAs(t, uuid.New().String(), f.entity, "2026-09", "USD", d, dr("1200", 1), cr("4000", 1))
	seen, first := f.pageAll(t, "manual-journals", "2026-09", 2)
	if len(seen) != 5 {
		t.Fatalf("visited %d, want 5", len(seen))
	}
	if got := sumAmounts(t, seen, "USD"); got != "16.25" || first.DeclaredTotals.Totals["USD"] != "16.25" {
		t.Fatalf("sum %s / declared %v, want 16.25", got, first.DeclaredTotals.Totals)
	}
	f.manual(t, "2026-10", d, dr("1200", 1), cr("4000", 1))
	if same := f.fp(t, "manual-journals", "2026-09", 100, ""); same.Watermark != first.Watermark {
		t.Fatal("a journal in another period must not change the watermark")
	}
	f.manual(t, "2026-09", d, dr("1200", 1), cr("4000", 1))
	if changed := f.fp(t, "manual-journals", "2026-09", 100, ""); changed.Watermark == first.Watermark {
		t.Fatal("a new manual journal must change the watermark")
	}
	// An approval change alters the record, hence the watermark.
	before := f.fp(t, "manual-journals", "2026-09", 100, "")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE journal_headers SET approved_by_principal_id='someone' WHERE tenant_id=$1 AND journal_id=$2`,
		f.tenant, before.Records[0].RecordID); err != nil {
		t.Fatal(err)
	}
	if changed := f.fp(t, "manual-journals", "2026-09", 100, ""); changed.Watermark == before.Watermark {
		t.Fatal("a changed attribute must change the watermark")
	}
}
