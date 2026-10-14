package arctl

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

var tNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

func usd(s string) map[string]kit.Amount { return map[string]kit.Amount{"USD": kit.MustAmount(s)} }

func cleanInputs() *Inputs {
	return &Inputs{
		LegalEntityID: "e1", AsOfDate: "2026-09-30", CutoffDate: "2026-09-30", ComputedAt: tNow, PendingTTLSeconds: 900,
		Expected: []ExpectedDoc{{DocumentID: "A", DocumentKind: "INVOICE", Currency: "USD", Amount: kit.MustAmount("100"), Origin: "AR04"}},
		Completeness: &ar07.CompletenessReport{ExpectedCount: 1, OpenItemRows: 1, OpenItemDistinct: 1, Balanced: true,
			ExpectedSum: usd("100"), OpenItemSum: usd("100"), Missing: []string{}, Unexpected: []string{}, AmountMismatch: []string{}, DuplicateDocuments: []string{}},
		Facts:     []PostingFact{{SourceType: "INVOICE", SourceID: "A", SourceEventID: "A", Currency: "USD", Amount: kit.MustAmount("100")}},
		Queue:     []QueueRow{{SourceEventID: "A", SourceType: "INVOICE", Status: kit.PostingPosted, CreatedAt: tNow.Add(-time.Hour)}},
		Subledger: Subledger{Available: true, Totals: usd("100"), Count: 1, Watermark: "wm:1"},
		GL:        GLRead{Available: true, MappingKeys: []string{MappingARReceivable}, Totals: usd("100"), Watermark: "glwm"},
	}
}

func kinds(fs []Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

func TestEvaluate_CleanRunPasses(t *testing.T) {
	fs := Evaluate(cleanInputs())
	require.Empty(t, fs)
	require.Equal(t, ResultPass, ResultOf(fs))
}

// #21: the headline totals tie (AR 200 = GL 200) but B is missing and C is unexpected: the run MUST fail.
func TestNeg21_EqualTotalsMissingAndUnexpectedStillBreak(t *testing.T) {
	in := cleanInputs()
	in.Expected = append(in.Expected, ExpectedDoc{DocumentID: "B", DocumentKind: "INVOICE", Currency: "USD", Amount: kit.MustAmount("100"), Origin: "AR04"})
	in.Facts = append(in.Facts, PostingFact{SourceType: "INVOICE", SourceID: "B", SourceEventID: "B", Currency: "USD", Amount: kit.MustAmount("100")})
	in.Queue = append(in.Queue, QueueRow{SourceEventID: "B", Status: kit.PostingPosted, CreatedAt: tNow.Add(-time.Hour)})
	in.Completeness = &ar07.CompletenessReport{ExpectedCount: 2, OpenItemRows: 2, OpenItemDistinct: 2, ExpectedSum: usd("200"), OpenItemSum: usd("200"),
		Missing: []string{"B"}, Unexpected: []string{"C"}, AmountMismatch: []string{}, DuplicateDocuments: []string{}, Balanced: false}
	in.Subledger.Totals, in.GL.Totals = usd("200"), usd("200") // headline AR == GL
	fs := Evaluate(in)
	require.ElementsMatch(t, []string{KindMissingOpenItem, KindUnexpectedOpenItem}, kinds(fs))
	require.Equal(t, ResultBreak, ResultOf(fs))
	for _, f := range fs {
		require.Equal(t, kit.CodeARGLReconciliationBreak, f.Code)
	}
}

func TestNeg21_DuplicateAndAmountMismatchWithEqualTotals(t *testing.T) {
	in := cleanInputs()
	in.Completeness.DuplicateDocuments = []string{"A"}
	in.Completeness.AmountMismatch = []string{"A"}
	in.Completeness.Balanced = false
	fs := Evaluate(in)
	require.ElementsMatch(t, []string{KindDuplicateOpenItem, KindAmountMismatch}, kinds(fs))
	require.Equal(t, ResultBreak, ResultOf(fs))
}

func TestEvaluate_UnbalancedWithoutPerDocumentListStillFails(t *testing.T) {
	in := cleanInputs()
	in.Completeness.Balanced = false // e.g. row count differs only
	require.Equal(t, []string{KindTotalsMismatch}, kinds(Evaluate(in)))
}

// #35: accounting events missing / pending too long / failed / quarantined are findings; posted is not.
func TestNeg35_PostingFindings(t *testing.T) {
	in := cleanInputs()
	mk := func(id, typ string) PostingFact {
		return PostingFact{SourceType: typ, SourceID: id, SourceEventID: id, Currency: "USD", Amount: kit.MustAmount("5")}
	}
	in.Facts = []PostingFact{mk("ok", "INVOICE"), mk("none", "CREDIT_NOTE"), mk("old", "DEBIT_NOTE"), mk("young", "CASH_APPLICATION"),
		mk("bad", "REFUND"), mk("quar", "WRITE_OFF"),
		{SourceType: "CASH_UNAPPLICATION", SourceID: "u", SourceEventID: "u:unapply", Currency: "USD", Amount: kit.MustAmount("1")},
		{SourceType: "LEGACY_INVOICE", SourceID: "lg", SourceEventID: "lg", Currency: "USD", Legacy: true},
		{SourceType: "LEGACY_INVOICE", SourceID: "lg2", SourceEventID: "lg2", Currency: "USD", Legacy: true, JournalID: "J1"}}
	in.Queue = []QueueRow{
		{SourceEventID: "ok", Status: kit.PostingPosted, CreatedAt: tNow.Add(-time.Hour)},
		{SourceEventID: "old", Status: kit.PostingPending, CreatedAt: tNow.Add(-time.Hour)},
		{SourceEventID: "young", Status: kit.PostingInProgress, CreatedAt: tNow.Add(-time.Minute)},
		{SourceEventID: "bad", Status: kit.PostingFailed, LastError: "boom", Attempts: 10, CreatedAt: tNow.Add(-time.Hour)},
		{SourceEventID: "quar", Status: kit.PostingQuarantined, CreatedAt: tNow.Add(-time.Hour)},
	}
	fs := Evaluate(in)
	got := map[string]string{}
	for _, f := range fs {
		got[f.SubjectID] = f.Kind + "/" + f.Severity
	}
	require.Equal(t, map[string]string{
		"none":  KindPostingMissing + "/BREAK",
		"old":   KindPostingPending + "/BREAK",
		"young": KindPostingInFlight + "/INFO",
		"bad":   KindPostingFailed + "/BREAK",
		"quar":  KindPostingQuarantined + "/BREAK",
		"u":     KindPostingMissing + "/BREAK",
		"lg":    KindPostingMissing + "/BREAK",
	}, got)
	require.Equal(t, ResultBreak, ResultOf(fs))
	// only an in-flight posting: not a break
	in2 := cleanInputs()
	in2.Queue[0] = QueueRow{SourceEventID: "A", Status: kit.PostingPending, CreatedAt: tNow.Add(-time.Minute)}
	require.Equal(t, ResultPass, ResultOf(Evaluate(in2)))
}

func TestEvaluate_GLDifferenceExactAndPerCurrency(t *testing.T) {
	in := cleanInputs()
	in.Subledger.Totals = map[string]kit.Amount{"USD": kit.MustAmount("100.0001"), "EUR": kit.MustAmount("5")}
	in.GL.Totals = map[string]kit.Amount{"USD": kit.MustAmount("100"), "GBP": kit.MustAmount("7")}
	fs := Evaluate(in)
	require.Len(t, fs, 3)
	by := map[string]Finding{}
	for _, f := range fs {
		require.Equal(t, KindGLDifference, f.Kind)
		require.Equal(t, kit.CodeARGLReconciliationBreak, f.Code)
		by[f.Currency] = f
	}
	require.Equal(t, "0.0001", by["USD"].Difference.String()) // one minor-unit tick is still a break: nothing is rounded away
	require.Equal(t, "100.0001", by["USD"].ARAmount.String())
	require.Equal(t, "100.0000", by["USD"].GLAmount.String())
	require.Equal(t, "5.0000", by["EUR"].Difference.String())
	require.Equal(t, "-7.0000", by["GBP"].Difference.String())
	require.Equal(t, ResultBreak, ResultOf(fs))
}

// A GL that cannot be read is UNVERIFIED, never a pass.
func TestEvaluate_GLUnavailableIsUnverifiedNotPass(t *testing.T) {
	in := cleanInputs()
	in.GL = GLRead{Available: false, Error: "general ledger unreachable", Totals: map[string]kit.Amount{}}
	fs := Evaluate(in)
	require.Equal(t, []string{KindGLUnverified}, kinds(fs))
	require.Equal(t, ResultUnverified, ResultOf(fs))
	in.Subledger.Available = true
	// a hard break outranks UNVERIFIED
	in.Completeness.Missing, in.Completeness.Balanced = []string{"A"}, false
	require.Equal(t, ResultBreak, ResultOf(Evaluate(in)))
	// subledger unreadable is also unverified
	in2 := cleanInputs()
	in2.Subledger = Subledger{Available: false, Error: "x", Totals: map[string]kit.Amount{}}
	require.Equal(t, ResultUnverified, ResultOf(Evaluate(in2)))
}

// #36: the tolerance reference is recorded but never changes what is found.
func TestNeg36_ToleranceNeverChangesFindings(t *testing.T) {
	a, b := cleanInputs(), cleanInputs()
	a.GL.Totals, b.GL.Totals = usd("99.99"), usd("99.99")
	a.Tolerance = ToleranceRef{PolicyID: "TOL-1", Version: "1"}
	b.Tolerance = ToleranceRef{PolicyID: "TOL-1", Version: "2-wide-open"}
	fa, fb := Evaluate(a), Evaluate(b)
	ja, _ := json.Marshal(fa)
	jb, _ := json.Marshal(fb)
	require.JSONEq(t, string(ja), string(jb))
	require.Equal(t, ResultBreak, ResultOf(fa))
	ha, _, _ := hashJSON(splitEvidence(a))
	hb, _, _ := hashJSON(splitEvidence(b))
	require.NotEqual(t, ha, hb, "the policy reference is part of the recorded inputs")
}

func TestEvaluate_ExceptionRollup(t *testing.T) {
	in := cleanInputs()
	in.Exceptions = []ExceptionItem{{Source: "AR10", Kind: "REFUND_STATE_UNKNOWN", AggregateID: "r1", Detail: "d"}, {Source: "AR04", Kind: "ACCOUNTING_MISSING", AggregateID: "i1"}}
	fs := Evaluate(in)
	require.Equal(t, []string{KindOperational, KindOperational}, kinds(fs))
	require.Equal(t, "AR04", fs[0].SubjectType) // sorted by source
	require.Equal(t, SevWarning, fs[0].Severity)
	require.Equal(t, ResultBreak, ResultOf(fs))
}

func TestEvidenceRoundTripAndReproducible(t *testing.T) {
	in := cleanInputs()
	in.GL.Totals = usd("99.5")
	in.Exceptions = []ExceptionItem{{Source: "AR08", Kind: "NO_POSTING", AggregateID: "x"}}
	h1, _, err := hashJSON(splitEvidence(in))
	require.NoError(t, err)
	raw := map[string][]byte{}
	for k, v := range splitEvidence(in) {
		_, b, err := hashJSON(v)
		require.NoError(t, err)
		raw[k] = b
	}
	back, err := joinEvidence(raw)
	require.NoError(t, err)
	h2, _, _ := hashJSON(splitEvidence(back))
	require.Equal(t, h1, h2)
	a, _ := json.Marshal(Evaluate(in))
	b, _ := json.Marshal(Evaluate(back))
	require.JSONEq(t, string(a), string(b))
}

func TestCutoffAndPeriod(t *testing.T) {
	d := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-02-28", CutoffOf(d).Format("2006-01-02"))
	require.Equal(t, "2026-02", PeriodOf(d))
}
