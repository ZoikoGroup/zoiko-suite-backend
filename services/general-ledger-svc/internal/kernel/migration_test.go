package kernel

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func migLine(ref, acct, dr, cr string) MigrationLineInput {
	return MigrationLineInput{SourceRef: ref, SourceAccount: "S" + acct, AccountCode: acct, Debit: dr, Credit: cr}
}

func migBatch(lines []MigrationLineInput, src MigrationTotals) (*MigrationBatch, []MigrationLine) {
	ls := make([]MigrationLine, len(lines))
	for i, l := range lines {
		l.Debit, l.Credit = normAmount(orZero(l.Debit)), normAmount(orZero(l.Credit))
		ls[i] = MigrationLine{LineNo: i + 1, MigrationLineInput: l}
	}
	b := &MigrationBatch{BatchID: "m1", LegalEntityID: entity, BookID: "b1", BatchCode: "OB-1", Kind: MigOpening, CutoverDate: "2026-01-01", Currency: "GBP", SourceTotals: src,
		ControlTotals: map[string]ControlTotal{}, LineCount: len(ls), LinesHash: linesHash(ls), Status: MigStaged, CreatedBy: "preparer"}
	dr, cr := migTotals(ls)
	b.TotalDebit, b.TotalCredit = dr, cr
	return b, ls
}

func migTotals(ls []MigrationLine) (string, string) {
	dr, cr := new(big.Rat), new(big.Rat)
	for _, l := range ls {
		d, _ := amount2(l.Debit)
		c, _ := amount2(l.Credit)
		dr.Add(dr, d)
		cr.Add(cr, c)
	}
	return dr.FloatString(2), cr.FloatString(2)
}

func codes(an *MigrationAnalysis) string {
	var out []string
	for _, i := range an.Issues {
		out = append(out, i.Code)
	}
	return strings.Join(out, ",")
}

func migDir() fakeDir {
	d := dir()
	ar := acct("1100")
	ar.ControlType = "AR"
	d.accounts["1100"] = ar
	bank := acct("1000")
	bank.ControlType = "BANK"
	d.accounts["1000"] = bank
	d.accounts["3000"] = acct("3000")
	hdr := acct("1900")
	hdr.Postable = false
	d.accounts["1900"] = hdr
	return d
}

func TestAStagedExtractIsCheckedForShapeBeforeAnythingIsKept(t *testing.T) {
	req := StageRequest{Lines: []MigrationLineInput{
		migLine("a", "1000", "100.00", ""),
		migLine("a", "3000", "", "100.00"),                                           // duplicate source ref
		migLine("c", "3000", "10.005", ""),                                           // a third decimal
		migLine("d", "3000", "5.00", "5.00"),                                         // both sides
		migLine("e", "3000", "", ""),                                                 // neither side
		migLine("", "3000", "1.00", ""),                                              // no source ref
		{SourceRef: "f", AccountCode: "3000", Debit: "1.00", TransactionAmount: "5"}, // half a foreign basis, no source account
	}}
	got := map[string]bool{}
	for _, i := range shapeIssues(req) {
		got[i.Code] = true
	}
	for _, want := range []string{"DUPLICATE_LINE", "AMOUNT_INVALID", "ONE_SIDE_REQUIRED", "SOURCE_REF_REQUIRED", "TRANSACTION_BASIS_INVALID", "ACCOUNT_REQUIRED"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(shapeIssues(StageRequest{})) == 0 {
		t.Fatal("an empty batch is refused")
	}
}

func TestTheLinesHashDetectsAnyChangeToTheStagedContent(t *testing.T) {
	_, a := migBatch([]MigrationLineInput{migLine("a", "1000", "100.00", ""), migLine("b", "3000", "", "100.00")}, MigrationTotals{})
	_, b := migBatch([]MigrationLineInput{migLine("a", "1000", "100.00", ""), migLine("b", "3000", "", "100.00")}, MigrationTotals{})
	if linesHash(a) != linesHash(b) {
		t.Fatal("the same content hashes the same")
	}
	b[1].AccountCode = "3001"
	if linesHash(a) == linesHash(b) {
		t.Fatal("a changed account changes the hash")
	}
	b[1].AccountCode = "3000"
	b[0], b[1] = b[1], b[0]
	if linesHash(a) == linesHash(b) {
		t.Fatal("order is part of the content")
	}
}

func TestAnalysisFindsEveryReasonABatchCannotPost(t *testing.T) {
	bk := book()
	s := &MigrationService{Dir: migDir()}
	good := []MigrationLineInput{
		{SourceRef: "ar1", SourceAccount: "S1", AccountCode: "1100", Debit: "100.00", CounterpartyID: "CUST-1"},
		migLine("bk1", "1000", "50.00", ""),
		migLine("eq1", "3000", "", "150.00"),
	}
	b, ls := migBatch(good, MigrationTotals{Debit: "150.00", Credit: "150.00"})
	b.ControlTotals = map[string]ControlTotal{"AR": {Total: "100.00", Count: 4, Ref: "ar-aging"}, "BANK": {Total: "50.00", Count: 1, Ref: "bank-rec"}}
	an, err := s.analyze(context.Background(), bk, b, ls)
	if err != nil || len(an.Issues) != 0 {
		t.Fatalf("a clean batch: %v %+v", err, an.Issues)
	}
	if len(an.Accounts) != 3 || an.TotalDebit != "150.00" {
		t.Fatalf("%+v", an)
	}
	for _, c := range an.Controls {
		if !c.Match {
			t.Fatalf("%+v", c)
		}
	}

	bad := []MigrationLineInput{
		migLine("ar1", "1100", "100.00", ""), // AR control line without its counterparty
		migLine("x", "9999", "10.00", ""),    // not in the chart
		migLine("h", "1900", "5.00", ""),     // a header account
		{SourceRef: "d", SourceAccount: "S", AccountCode: "3000", Credit: "20.00", Dimensions: map[string]string{"cost_center": "NOPE"}},
	}
	b2, ls2 := migBatch(bad, MigrationTotals{Debit: "115.00", Credit: "20.00"})
	d := migDir()
	d.badDims = map[string]bool{"cost_center=NOPE": true}
	b2.ControlTotals = map[string]ControlTotal{"AR": {Total: "90.00", Ref: "ar-aging"}}
	an2, _ := (&MigrationService{Dir: d}).analyze(context.Background(), bk, b2, ls2)
	got := codes(an2)
	for _, want := range []string{"COUNTERPARTY_REQUIRED", "ACCOUNT_UNAVAILABLE", "DIMENSION_INVALID", "BATCH_UNBALANCED", "CONTROL_TOTAL_MISMATCH"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}

	// the totals the source declared must be the totals of the lines
	b3, ls3 := migBatch(good, MigrationTotals{Debit: "151.00", Credit: "150.00"})
	if an3, _ := s.analyze(context.Background(), bk, b3, ls3); !strings.Contains(codes(an3), "SOURCE_TOTALS_MISMATCH") {
		t.Fatalf("%s", codes(an3))
	}
	// staged lines that no longer hash to the staged content are refused
	b4, ls4 := migBatch(good, MigrationTotals{Debit: "150.00", Credit: "150.00"})
	ls4[0].Description = "edited"
	if an4, _ := s.analyze(context.Background(), bk, b4, ls4); !strings.Contains(codes(an4), "BATCH_ALTERED") {
		t.Fatalf("%s", codes(an4))
	}
	// control balances the batch holds without a subledger total to reconcile them to
	b5, ls5 := migBatch(good, MigrationTotals{Debit: "150.00", Credit: "150.00"})
	if an5, _ := s.analyze(context.Background(), bk, b5, ls5); !strings.Contains(codes(an5), "CONTROL_TOTAL_MISSING") {
		t.Fatalf("%s", codes(an5))
	}
}

// memMig is the least a MigrationService needs to be exercised without a database.
type memMig struct {
	MigrationRepo
	b      MigrationBatch
	posted int
	led    MigrationLedgerSummary
	rec    *MigrationReconciliation
}

func (m *memMig) GetMigrationBatch(context.Context, string, string) (*MigrationBatch, error) {
	c := m.b
	return &c, nil
}
func (m *memMig) ApproveMigration(_ context.Context, _, _, actor, authority string, ws, we, at time.Time, _ OutEvent) (*MigrationBatch, error) {
	m.b.Status, m.b.ApprovedBy, m.b.AuthorityRef, m.b.WindowStart, m.b.WindowEnd = MigApproved, actor, authority, &ws, &we
	c := m.b
	return &c, nil
}
func (m *memMig) MigrationLedger(context.Context, string, string) (*MigrationLedgerSummary, error) {
	return &m.led, nil
}
func (m *memMig) RecordMigrationReconciliation(_ context.Context, _, _, _ string, rec MigrationReconciliation, hash string, _ time.Time, _ OutEvent) (*MigrationBatch, error) {
	m.rec = &rec
	if rec.Status == "MATCHED" {
		m.b.Status, m.b.EvidenceHash, m.b.Reconciliation = MigReconciled, hash, &rec
	}
	c := m.b
	return &c, nil
}

func TestApprovalNeedsAnotherPersonANamedAuthorityAndAWindow(t *testing.T) {
	now := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	m := &memMig{b: MigrationBatch{BatchID: "m1", Status: MigValidated, CreatedBy: "preparer"}}
	s := &MigrationService{Repo: m, Clock: func() time.Time { return now }}
	ws, we := now, now.Add(4*time.Hour)
	if _, err := s.Approve(context.Background(), "t", Actor{PrincipalID: "preparer"}, "m1", "MIG-AUTH-1", ws, we); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the preparer cannot approve: %v", err)
	}
	for what, c := range map[string]struct {
		authority string
		ws, we    time.Time
	}{
		"no authority":              {" ", ws, we},
		"a window that ends before": {"MIG-AUTH-1", we, ws},
		"a window already over":     {"MIG-AUTH-1", now.Add(-5 * time.Hour), now.Add(-time.Hour)},
		"no window":                 {"MIG-AUTH-1", time.Time{}, time.Time{}},
	} {
		if _, err := s.Approve(context.Background(), "t", Actor{PrincipalID: "approver"}, "m1", c.authority, c.ws, c.we); err == nil {
			t.Fatalf("%s: accepted", what)
		}
	}
	m.b.Status = MigStaged
	if _, err := s.Approve(context.Background(), "t", Actor{PrincipalID: "approver"}, "m1", "MIG-AUTH-1", ws, we); !errors.Is(err, ErrState) {
		t.Fatalf("only a validated batch can be approved: %v", err)
	}
	m.b.Status = MigValidated
	if b, err := s.Approve(context.Background(), "t", Actor{PrincipalID: "approver"}, "m1", "MIG-AUTH-1", ws, we); err != nil || b.Status != MigApproved || b.ApprovedBy != "approver" {
		t.Fatalf("%v %+v", err, b)
	}
}

func TestPostingOnlyHappensInsideTheApprovedWindow(t *testing.T) {
	now := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	ws, we := now.Add(time.Hour), now.Add(5*time.Hour)
	m := &memMig{b: MigrationBatch{BatchID: "m1", BookID: "b1", Status: MigApproved, WindowStart: &ws, WindowEnd: &we}}
	m.b.CutoverDate = "2026-01-01"
	s := &MigrationService{Repo: m, Books: bookRepo{}, Dir: migDir(), Clock: func() time.Time { return now }}
	_, err := s.Post(context.Background(), "t", Actor{PrincipalID: "poster"}, "m1")
	if e, ok := AsException(err); !ok || e.Code != "MIGRATION_WINDOW_CLOSED" {
		t.Fatalf("before the window: %v", err)
	}
	s.Clock = func() time.Time { return we }
	if _, err := s.Post(context.Background(), "t", Actor{PrincipalID: "poster"}, "m1"); err == nil || !strings.Contains(err.Error(), "WINDOW") {
		t.Fatalf("the window is half-open: %v", err)
	}
}

type bookRepo struct{ Repo }

func (bookRepo) GetBook(context.Context, string, string) (*Book, error) { return book(), nil }

func TestReconciliationMatchesFreezesTheEvidenceAndABreakDoesNot(t *testing.T) {
	ws, we := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	base := MigrationBatch{BatchID: "m1", Status: MigPosted, JournalID: "j1", LinesHash: strings.Repeat("a", 64), SourceExtractHash: strings.Repeat("b", 64), MappingRef: "map-v3",
		SourceTotals: MigrationTotals{Debit: "150.00", Credit: "150.00"}, ApprovedBy: "approver", AuthorityRef: "MIG-AUTH-1", WindowStart: &ws, WindowEnd: &we,
		ControlTotals: map[string]ControlTotal{"AR": {Total: "100.00", Ref: "aging"}, "AP": {Total: "30.00", Ref: "ap-aging"}}}
	m := &memMig{b: base, led: MigrationLedgerSummary{Debit: "150.00", Credit: "150.00", JournalNumber: "JE-2026-000001", JournalHash: "h1",
		ByControl: map[string]string{"AR": "100.00", "AP": "-30.00"}}}
	s := &MigrationService{Repo: m, Clock: func() time.Time { return ws }}
	got, err := s.Reconcile(context.Background(), "t", Actor{PrincipalID: "rec"}, "m1")
	if err != nil || got.Status != MigReconciled || len(got.EvidenceHash) != 64 || m.rec.Status != "MATCHED" {
		t.Fatalf("%v %+v", err, got)
	}
	// the evidence hash binds the approval and the mapping: change either and it no longer verifies
	changed := *got
	changed.MappingRef = "map-v4"
	if evidenceHash(&changed, *got.Reconciliation) == got.EvidenceHash {
		t.Fatal("the mapping is part of the evidence")
	}
	changed = *got
	changed.ApprovedBy = "someone-else"
	if evidenceHash(&changed, *got.Reconciliation) == got.EvidenceHash {
		t.Fatal("the approver is part of the evidence")
	}

	// the AR open items in the subledger are 100 and the ledger holds 90: a break, named, and the batch is not frozen
	m2 := &memMig{b: base, led: MigrationLedgerSummary{Debit: "150.00", Credit: "150.00", ByControl: map[string]string{"AR": "90.00", "AP": "-30.00"}}}
	s2 := &MigrationService{Repo: m2, Clock: func() time.Time { return ws }}
	got2, err := s2.Reconcile(context.Background(), "t", Actor{PrincipalID: "rec"}, "m1")
	if err != nil || got2.Status != MigPosted || m2.rec.Status != "BREAK" || len(m2.rec.Differences) != 1 || !strings.HasPrefix(m2.rec.Differences[0].What, "AR control total") {
		t.Fatalf("%v %+v %+v", err, got2, m2.rec)
	}
	if got2.EvidenceHash != "" {
		t.Fatal("a break freezes nothing")
	}

	// a batch that has not posted cannot be reconciled
	m3 := &memMig{b: MigrationBatch{BatchID: "m1", Status: MigApproved}}
	if _, err := (&MigrationService{Repo: m3}).Reconcile(context.Background(), "t", Actor{}, "m1"); !errors.Is(err, ErrState) {
		t.Fatalf("%v", err)
	}
}

func (m *memMig) MigrationLines(context.Context, string, string, int, int) ([]MigrationLine, error) {
	return nil, nil
}
