//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ---- event-journal-breaks fixtures ----

// ejJournal creates a FINALIZED journal carrying source in the fixture's
// tenant/entity through the store's normal path, then pins created_at and
// posted_at (direct SQL: the store stamps them with now()).
func (f *w4Fixture) ejJournal(t *testing.T, source string, at time.Time, amt float64) string {
	t.Helper()
	o := w5Opts{finalize: true}
	if source != "" {
		o.source = strp(source)
	}
	id := f.w5Journal(t, f.tenant, f.entity, "2026-09", "USD", domain.NewDate(2026, 9, 4), o, dr("1200", amt), cr("4000", amt))
	f.pinJournalTime(t, id, at)
	return id
}

func (f *w4Fixture) pinJournalTime(t *testing.T, journalID string, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE journal_headers SET created_at=$1, posted_at=$1 WHERE tenant_id=$2 AND journal_id=$3`,
		at, f.tenant, journalID); err != nil {
		t.Fatalf("pin journal time: %v", err)
	}
}

// ejCommit marks an execution COMMITTED pointing at journalID through the
// store's normal path.
func (f *w4Fixture) ejCommit(t *testing.T, executionID, journalID string) {
	t.Helper()
	if err := f.s.MarkPostingExecutionCommitted(f.ctx, f.tenant, executionID, journalID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkPostingExecutionCommitted: %v", err)
	}
}

func (f *w4Fixture) ejExec(t *testing.T, source string, created time.Time) string {
	t.Helper()
	return f.execution(t, f.tenant, f.entity, "SUBMITTED", strp(source), created)
}

func (f *w4Fixture) ejExecSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("sql: %v", err)
	}
}

func (f *w4Fixture) breaks(t *testing.T, before time.Time, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryEventJournalBreaks(f.ctx, f.tenant, domain.EventJournalBreaksQuery{
		LegalEntityID: f.entity, CreatedBefore: before, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryEventJournalBreaks: %v", err)
	}
	return p
}

var ejPast = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func ejFuture() time.Time { return time.Now().UTC().Add(24 * time.Hour) }

func TestEventJournalBreaks_CleanPairYieldsNothing(t *testing.T) {
	f := newW4Fixture(t)
	ex := f.ejExec(t, "EVT-OK", ejPast)
	j := f.ejJournal(t, "EVT-OK", ejPast, 10)
	f.ejCommit(t, ex, j)

	// Manual journal (no source), blank source and a non-EVENT execution are all out of scope.
	f.ejJournal(t, "", ejPast, 5)
	f.ejJournal(t, "   ", ejPast, 5)
	other := f.ejExec(t, "APPROVED-1", ejPast)
	f.ejExecSQL(t, `UPDATE posting_executions SET kind='APPROVED_JOURNAL', status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, other)
	// A REVERSED event journal whose execution still points at it is clean.
	ex2 := f.ejExec(t, "EVT-REV", ejPast)
	j2 := f.ejJournal(t, "EVT-REV", ejPast, 7)
	f.ejCommit(t, ex2, j2)
	f.ejExecSQL(t, `UPDATE journal_headers SET status='REVERSED' WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, j2)
	// In-flight and failed executions are not this control's concern.
	f.execution(t, f.tenant, f.entity, "FAILED", strp("EVT-F"), ejPast)

	p := f.breaks(t, ejFuture(), 100, "")
	if len(p.Records) != 0 || p.DeclaredTotals.RowCount != 0 {
		t.Fatalf("clean data must yield no breaks, got %+v", p.Records)
	}
	if !strings.HasPrefix(p.Watermark, "gl8:n=0;entry_seq=0;md5=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}
}

func TestEventJournalBreaks_EachBreakTypeDetected(t *testing.T) {
	f := newW4Fixture(t)

	// EVENT_WITHOUT_JOURNAL: NULL journal, dangling journal, non-finalized journal.
	eNull := f.ejExec(t, "EVT-NULL", ejPast)
	f.ejExecSQL(t, `UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, eNull)
	eDangling := f.ejExec(t, "EVT-DANGLING", ejPast)
	dangling := uuid.New().String()
	f.ejCommit(t, eDangling, dangling)
	ePending := f.ejExec(t, "EVT-PENDING", ejPast)
	pendingJ := f.w5Journal(t, f.tenant, f.entity, "2026-09", "EUR", domain.NewDate(2026, 9, 4),
		w5Opts{source: strp("EVT-PENDING")}, dr("1200", 12.5), cr("4000", 12.5))
	f.ejCommit(t, ePending, pendingJ)

	// JOURNAL_WITHOUT_EVENT: no execution / execution not COMMITTED / execution points elsewhere.
	orphan := f.ejJournal(t, "EVT-ORPHAN", ejPast, 20.25)
	notCommitted := f.ejJournal(t, "EVT-NOTCOMMITTED", ejPast, 3)
	f.ejExec(t, "EVT-NOTCOMMITTED", ejPast)
	wrongJ := f.ejJournal(t, "EVT-WRONG", ejPast, 4)
	otherJ := f.ejJournal(t, "EVT-WRONG-OTHER", ejPast, 4)
	eWrong := f.ejExec(t, "EVT-WRONG", ejPast)
	f.ejCommit(t, eWrong, otherJ)
	// The reverse link is also missing for otherJ (its own source has no execution).
	reversedOrphan := f.ejJournal(t, "EVT-REVERSED-ORPHAN", ejPast, 6)
	f.ejExecSQL(t, `UPDATE journal_headers SET status='REVERSED' WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, reversedOrphan)

	// DUPLICATE_JOURNAL_FOR_EVENT: a second (and third) live journal for one event; a REVERSED one is ignored.
	eDup := f.ejExec(t, "EVT-DUP", ejPast)
	dupA := f.ejJournal(t, "EVT-DUP", ejPast, 9)
	f.ejCommit(t, eDup, dupA)
	dupB := f.ejJournal(t, "EVT-DUP", ejPast.Add(time.Minute), 9)
	dupC := f.ejJournal(t, "EVT-DUP", ejPast.Add(2*time.Minute), 9)
	dupRev := f.ejJournal(t, "EVT-DUP", ejPast.Add(3*time.Minute), 9)
	f.ejExecSQL(t, `UPDATE journal_headers SET status='REVERSED' WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, dupRev)

	p := f.breaks(t, ejFuture(), 100, "")
	m := byID(p)
	want := map[string]struct{ ref, amount, cur, date, status, journal, exec, source string }{
		"EVENT_WITHOUT_JOURNAL:" + eNull:     {"EVT-NULL", "0", "XXX", "2026-09-10", "COMMITTED", "", eNull, "EVT-NULL"},
		"EVENT_WITHOUT_JOURNAL:" + eDangling: {"EVT-DANGLING", "0", "XXX", "2026-09-10", "COMMITTED", dangling, eDangling, "EVT-DANGLING"},
		"EVENT_WITHOUT_JOURNAL:" + ePending:  {"EVT-PENDING", "12.50", "EUR", "2026-09-10", "PENDING", pendingJ, ePending, "EVT-PENDING"},

		"JOURNAL_WITHOUT_EVENT:" + orphan:         {"EVT-ORPHAN", "20.25", "USD", "2026-09-04", "FINALIZED", orphan, "", "EVT-ORPHAN"},
		"JOURNAL_WITHOUT_EVENT:" + notCommitted:   {"EVT-NOTCOMMITTED", "3.00", "USD", "2026-09-04", "FINALIZED", notCommitted, "", "EVT-NOTCOMMITTED"},
		"JOURNAL_WITHOUT_EVENT:" + wrongJ:         {"EVT-WRONG", "4.00", "USD", "2026-09-04", "FINALIZED", wrongJ, "", "EVT-WRONG"},
		"JOURNAL_WITHOUT_EVENT:" + otherJ:         {"EVT-WRONG-OTHER", "4.00", "USD", "2026-09-04", "FINALIZED", otherJ, "", "EVT-WRONG-OTHER"},
		"JOURNAL_WITHOUT_EVENT:" + reversedOrphan: {"EVT-REVERSED-ORPHAN", "6.00", "USD", "2026-09-04", "REVERSED", reversedOrphan, "", "EVT-REVERSED-ORPHAN"},

		"DUPLICATE_JOURNAL_FOR_EVENT:" + dupB: {"EVT-DUP", "9.00", "USD", "2026-09-04", "FINALIZED", dupB, "", "EVT-DUP"},
		"DUPLICATE_JOURNAL_FOR_EVENT:" + dupC: {"EVT-DUP", "9.00", "USD", "2026-09-04", "FINALIZED", dupC, "", "EVT-DUP"},
		// dupB/dupC are also unlinked from the execution (which points at dupA); dupRev is REVERSED so not a duplicate.
		"JOURNAL_WITHOUT_EVENT:" + dupB:   {"EVT-DUP", "9.00", "USD", "2026-09-04", "FINALIZED", dupB, "", "EVT-DUP"},
		"JOURNAL_WITHOUT_EVENT:" + dupC:   {"EVT-DUP", "9.00", "USD", "2026-09-04", "FINALIZED", dupC, "", "EVT-DUP"},
		"JOURNAL_WITHOUT_EVENT:" + dupRev: {"EVT-DUP", "9.00", "USD", "2026-09-04", "REVERSED", dupRev, "", "EVT-DUP"},
	}
	if len(m) != len(want) || p.DeclaredTotals.RowCount != int64(len(want)) {
		for id := range m {
			if _, ok := want[id]; !ok {
				t.Errorf("unexpected record %s: %+v", id, m[id])
			}
		}
		t.Fatalf("got %d records (row_count %d), want %d", len(m), p.DeclaredTotals.RowCount, len(want))
	}
	for id, w := range want {
		r, ok := m[id]
		if !ok {
			t.Fatalf("missing record %s", id)
		}
		bt := id[:strings.Index(id, ":")]
		if r.Reference != w.ref || r.Amount != w.amount || r.Currency != w.cur || r.Date != w.date ||
			r.Attributes["break_type"] != bt || r.Attributes["status"] != w.status ||
			r.Attributes["journal_id"] != w.journal || r.Attributes["execution_id"] != w.exec ||
			r.Attributes["source_event_id"] != w.source || len(r.Attributes) != 5 {
			t.Fatalf("%s: unexpected record %+v", id, r)
		}
	}
	if p.DeclaredTotals.Totals["XXX"] != "0" || p.DeclaredTotals.Totals["EUR"] != "12.50" {
		t.Fatalf("declared totals = %v", p.DeclaredTotals.Totals)
	}
	if !strings.HasPrefix(p.Watermark, "gl8:n=13;entry_seq=0;md5=") {
		t.Fatalf("watermark = %q", p.Watermark)
	}
}

func TestEventJournalBreaks_StrictCutoff(t *testing.T) {
	f := newW4Fixture(t)
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	eAt := f.ejExec(t, "EVT-AT", cutoff)
	f.ejExecSQL(t, `UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, eAt)
	eBefore := f.ejExec(t, "EVT-BEFORE", cutoff.Add(-time.Second))
	f.ejExecSQL(t, `UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, eBefore)

	jAt := f.ejJournal(t, "EVT-J-AT", cutoff, 1)
	jBefore := f.ejJournal(t, "EVT-J-BEFORE", cutoff.Add(-time.Second), 1)
	// Created before but posted at the cut-off: still in flight, excluded.
	jPostedAt := f.ejJournal(t, "EVT-J-POSTED", cutoff.Add(-time.Hour), 1)
	f.ejExecSQL(t, `UPDATE journal_headers SET posted_at=$1 WHERE tenant_id=$2 AND journal_id=$3`, cutoff, f.tenant, jPostedAt)
	// Duplicate whose extra journal is created at the cut-off: excluded.
	f.ejJournal(t, "EVT-D", cutoff.Add(-2*time.Hour), 1)
	dAt := f.ejJournal(t, "EVT-D", cutoff, 1)

	m := byID(f.breaks(t, cutoff, 100, ""))
	for id := range m {
		if strings.Contains(id, eAt) || strings.Contains(id, jAt) || strings.Contains(id, jPostedAt) || strings.Contains(id, dAt) {
			t.Fatalf("record at/after the cut-off must be excluded: %s", id)
		}
	}
	if _, ok := m["EVENT_WITHOUT_JOURNAL:"+eBefore]; !ok {
		t.Fatal("execution one second before the cut-off must be included")
	}
	if _, ok := m["JOURNAL_WITHOUT_EVENT:"+jBefore]; !ok {
		t.Fatal("journal one second before the cut-off must be included")
	}

	later := byID(f.breaks(t, cutoff.Add(time.Second), 100, ""))
	for _, id := range []string{"EVENT_WITHOUT_JOURNAL:" + eAt, "JOURNAL_WITHOUT_EVENT:" + jAt, "JOURNAL_WITHOUT_EVENT:" + jPostedAt, "DUPLICATE_JOURNAL_FOR_EVENT:" + dAt} {
		if _, ok := later[id]; !ok {
			t.Fatalf("%s must appear once the cut-off moves past it", id)
		}
	}
	// The same instant expressed in another zone yields the identical population.
	est := time.FixedZone("EST", -5*3600)
	if a, b := f.breaks(t, cutoff, 100, ""), f.breaks(t, cutoff.In(est), 100, ""); a.Watermark != b.Watermark {
		t.Fatalf("same instant, different watermark: %q vs %q", a.Watermark, b.Watermark)
	}
}

func TestEventJournalBreaks_TenantAndEntityIsolation(t *testing.T) {
	f := newW4Fixture(t)
	foreignTenant := uuid.New().String()
	foreignEntity := uuid.New().String()

	// Same source_event_id in another tenant must neither create nor satisfy a link.
	j := f.ejJournal(t, "EVT-SHARED", ejPast, 5)
	fex := f.execution(t, foreignTenant, f.entity, "SUBMITTED", strp("EVT-SHARED"), ejPast)
	fctx := svcmiddleware.WithTenant(context.Background(), foreignTenant)
	if err := f.s.MarkPostingExecutionCommitted(fctx, foreignTenant, fex, j, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Foreign-tenant violations, and same-tenant other-entity violations.
	f.execution(t, foreignTenant, f.entity, "SUBMITTED", strp("EVT-F1"), ejPast)
	f.w5Journal(t, foreignTenant, f.entity, "2026-09", "USD", domain.NewDate(2026, 9, 4),
		w5Opts{source: strp("EVT-F2"), finalize: true}, dr("1200", 1), cr("4000", 1))
	f.w5Journal(t, f.tenant, foreignEntity, "2026-09", "USD", domain.NewDate(2026, 9, 4),
		w5Opts{source: strp("EVT-E1"), finalize: true}, dr("1200", 1), cr("4000", 1))
	f.ejExecSQL(t, `UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1`, foreignTenant)

	p := f.breaks(t, ejFuture(), 100, "")
	if _, ok := byID(p)["JOURNAL_WITHOUT_EVENT:"+j]; !ok || p.DeclaredTotals.RowCount != 1 {
		t.Fatalf("only the own-tenant unlinked journal should be reported, got %+v", p.Records)
	}

	foreign, err := f.s.QueryEventJournalBreaks(svcmiddleware.WithTenant(context.Background(), uuid.New().String()),
		uuid.New().String(), domain.EventJournalBreaksQuery{LegalEntityID: f.entity, CreatedBefore: ejFuture(), Limit: 10})
	if err != nil || foreign.DeclaredTotals.RowCount != 0 {
		t.Fatalf("a foreign tenant saw rows: %v %+v", err, foreign)
	}
}

func TestEventJournalBreaks_Paging_Watermark(t *testing.T) {
	f := newW4Fixture(t)
	for i := 0; i < 3; i++ {
		f.ejJournal(t, "EVT-P"+string(rune('A'+i)), ejPast, float64(i+1))
		e := f.ejExec(t, "EVT-X"+string(rune('A'+i)), ejPast)
		f.ejExecSQL(t, `UPDATE posting_executions SET status='COMMITTED' WHERE tenant_id=$1 AND execution_id=$2`, f.tenant, e)
	}
	cutoff := ejFuture()
	seen := map[string]bool{}
	var wm, prev string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		p := f.breaks(t, cutoff, 2, after)
		if wm == "" {
			wm = p.Watermark
		} else if p.Watermark != wm {
			t.Fatal("watermark changed between pages")
		}
		if p.DeclaredTotals.RowCount != 6 {
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
	if len(seen) != 6 {
		t.Fatalf("visited %d, want 6", len(seen))
	}
	// Repeatable: nothing changed, same watermark.
	if again := f.breaks(t, cutoff, 100, ""); again.Watermark != wm {
		t.Fatalf("unchanged data, different watermark: %q vs %q", again.Watermark, wm)
	}
	// Fixing one break changes the watermark.
	f.ejJournal(t, "EVT-PNEW", ejPast, 1)
	if changed := f.breaks(t, cutoff, 100, ""); changed.Watermark == wm || changed.DeclaredTotals.RowCount != 7 {
		t.Fatalf("a new break must change the watermark: %+v", changed.DeclaredTotals)
	}
}
