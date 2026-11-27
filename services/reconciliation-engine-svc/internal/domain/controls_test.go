package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ZS-SVC-Z-001 DQC-03 (docs/architecture/DQC_GAP_MATRIX.md, step I): source-declared
// hash total, sequence total and roll-forward, and the PARTIAL scope.

func seqRecs(n int) []PopulationRecord {
	out := make([]PopulationRecord, n)
	for i := range out {
		out[i] = PopulationRecord{RecordID: fmt.Sprintf("r%03d", i), Reference: fmt.Sprintf("REF-%d", i), Amount: "10.00",
			Currency: "USD", Date: "2026-09-01", Attributes: map[string]string{"seq": fmt.Sprint(i + 1)}}
	}
	return out
}

func mustFail(t *testing.T, c DeclaredControls, recs []PopulationRecord, want string) {
	t.Helper()
	_, err := VerifyDeclaredControls(c, recs)
	if !errors.Is(err, ErrDeclaredControl) {
		t.Fatalf("err = %v, want ErrDeclaredControl", err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to mention %q", err, want)
	}
}

// The v1 bytes were derived independently with sha256sum over the serialization as
// documented, not from this code. If this fails the format changed: give the new one
// a new name, never edit this vector.
const goldenCpcV1 = "fb706d5e972963ca394a44ca66f6b32b7d36b5ae8752d4364ecf626776b33390"

func goldenRecs() []PopulationRecord {
	return []PopulationRecord{
		{RecordID: "rec-2", Reference: "REF-2", Amount: "-250.50", Currency: "EUR", Date: "2026-09-04", Attributes: map[string]string{"seq": "2", "entity": "E2"}},
		{RecordID: "rec-1", Reference: "REF-1", Amount: "100.00", Currency: "USD", Date: "2026-09-01"},
	}
}

func TestSourceContentHash_CpcV1IsPinned(t *testing.T) {
	got, err := SourceContentHash(goldenRecs())
	if err != nil || got != goldenCpcV1 {
		t.Fatalf("SourceContentHash = %s, %v\nwant the independently derived %s", got, err, goldenCpcV1)
	}
	// "100", "100.0" and "100.00" are one amount.
	r := goldenRecs()
	r[1].Amount = "100"
	if again, _ := SourceContentHash(r); again != goldenCpcV1 {
		t.Fatal("insignificant zeros changed the hash")
	}
}

func TestHashTotal_VerifiedAndEveryTamperRefused(t *testing.T) {
	good := DeclaredControls{Hash: &DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: goldenCpcV1}}
	out, err := VerifyDeclaredControls(good, goldenRecs())
	if err != nil || out[0].Control != ControlHash || out[0].Status != OutcomeVerified {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	// Upper-case hex is the same hash.
	up := DeclaredControls{Hash: &DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: strings.ToUpper(goldenCpcV1)}}
	if _, err := VerifyDeclaredControls(up, goldenRecs()); err != nil {
		t.Fatalf("upper-case hex refused: %v", err)
	}
	tamper := map[string]func([]PopulationRecord) []PopulationRecord{
		"amount changed":    func(r []PopulationRecord) []PopulationRecord { r[0].Amount = "-250.51"; return r },
		"reference changed": func(r []PopulationRecord) []PopulationRecord { r[1].Reference = "REF-X"; return r },
		"record dropped":    func(r []PopulationRecord) []PopulationRecord { return r[:1] },
		"attribute changed": func(r []PopulationRecord) []PopulationRecord { r[0].Attributes["entity"] = "E3"; return r },
		"date changed":      func(r []PopulationRecord) []PopulationRecord { r[1].Date = "2026-09-02"; return r },
		"record added": func(r []PopulationRecord) []PopulationRecord {
			return append(r, PopulationRecord{RecordID: "x", Reference: "x", Amount: "0", Currency: "USD", Date: "2026-09-01"})
		},
	}
	for name, f := range tamper {
		t.Run(name, func(t *testing.T) { mustFail(t, good, f(goldenRecs()), "hash total") })
	}
}

func TestHashTotal_UnknownFormatIsReportedNeverGuessed(t *testing.T) {
	for _, h := range []DeclaredHash{
		{Algorithm: "md5", Serialization: "cpc-v1", Value: goldenCpcV1},
		{Algorithm: "sha256", Serialization: "cpc-v2", Value: goldenCpcV1},
		{Algorithm: "", Serialization: "", Value: goldenCpcV1},
	} {
		h := h
		mustFail(t, DeclaredControls{Hash: &h}, goldenRecs(), "re-derive")
	}
}

func TestSequence_ContinuousSetVerifies(t *testing.T) {
	out, err := VerifyDeclaredControls(DeclaredControls{Sequence: &DeclaredSequence{Attribute: "seq", First: 1, Last: 5}}, seqRecs(5))
	if err != nil || out[1].Control != ControlSequence || out[1].Status != OutcomeVerified {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
}

func TestSequence_GapDuplicateAndMalformedAreRefused(t *testing.T) {
	seq := func(first, last int64) DeclaredControls {
		return DeclaredControls{Sequence: &DeclaredSequence{Attribute: "seq", First: first, Last: last}}
	}
	gap := seqRecs(5)
	gap = append(gap[:2], gap[3:]...) // 3 is missing, 4 records remain
	mustFail(t, seq(1, 5), gap, "")
	mustFail(t, seq(1, 5), gap, "not continuous")

	// Same count as the span, but a duplicate hides the gap: 1,2,2,4,5.
	dup := seqRecs(5)
	dup[2].Attributes["seq"] = "2"
	mustFail(t, seq(1, 5), dup, "appears on records")
	mustFail(t, seq(1, 5), dup, "3 is missing")

	missingAttr := seqRecs(5)
	delete(missingAttr[0].Attributes, "seq")
	mustFail(t, seq(1, 5), missingAttr, "has no")

	nonInt := seqRecs(5)
	nonInt[0].Attributes["seq"] = "1.5"
	mustFail(t, seq(1, 5), nonInt, "non-integer")

	outside := seqRecs(5)
	outside[4].Attributes["seq"] = "9"
	mustFail(t, seq(1, 5), outside, "outside")

	// Declared wider than the records: a gap by definition; and it must not allocate by the span.
	mustFail(t, seq(1, 9_000_000_000_000), seqRecs(5), "6 is missing")
	mustFail(t, seq(5, 1), seqRecs(5), "runs from")
	mustFail(t, DeclaredControls{Sequence: &DeclaredSequence{Attribute: " ", First: 1, Last: 1}}, seqRecs(1), "attribute")
}

func rf(open, close map[string]string) DeclaredControls {
	return DeclaredControls{RollForward: &DeclaredRollForward{Opening: open, Closing: close, PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30"}}
}

func TestRollForward_TiesAndEveryBreakIsRefused(t *testing.T) {
	recs := seqRecs(3) // movement +30.00 USD
	out, err := VerifyDeclaredControls(rf(map[string]string{"USD": "100.00"}, map[string]string{"USD": "130"}), recs)
	if err != nil || out[2].Status != OutcomeVerified {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	mustFail(t, rf(map[string]string{"USD": "100.00"}, map[string]string{"USD": "130.01"}), recs, "source declared closing")
	// A currency with records but no declared balances is never read as zero.
	mustFail(t, rf(map[string]string{"EUR": "1"}, map[string]string{"EUR": "1"}), recs, "no opening and closing balance for USD")
	mustFail(t, rf(map[string]string{"USD": "100"}, map[string]string{}), recs, "no opening and closing")
	// A declared currency with no records must still roll: opening = closing.
	eur := rf(map[string]string{"USD": "100", "EUR": "5"}, map[string]string{"USD": "130", "EUR": "6"})
	mustFail(t, eur, recs, "EUR")
	okEur := rf(map[string]string{"USD": "100", "EUR": "5"}, map[string]string{"USD": "130", "EUR": "5.00"})
	if _, err := VerifyDeclaredControls(okEur, recs); err != nil {
		t.Fatalf("a currency with no movement and unchanged balance must tie: %v", err)
	}
	mustFail(t, rf(map[string]string{"USD": "abc"}, map[string]string{"USD": "1"}), recs, "")

	half := DeclaredControls{RollForward: &DeclaredRollForward{Opening: map[string]string{"USD": "100"}, Closing: map[string]string{"USD": "130"}, PeriodStart: "2026-09-01"}}
	mustFail(t, half, recs, "go together")
	back := DeclaredControls{RollForward: &DeclaredRollForward{Opening: map[string]string{"USD": "100"}, Closing: map[string]string{"USD": "130"},
		PeriodStart: "2026-09-30", PeriodEnd: "2026-09-01"}}
	mustFail(t, back, recs, "date range")
}

func TestOpeningContinuity(t *testing.T) {
	if err := OpeningContinuity(map[string]string{"USD": "130.00"}, map[string]string{"USD": "130"}); err != nil {
		t.Fatalf("equal balances: %v", err)
	}
	for name, c := range map[string][2]map[string]string{
		"different":              {{"USD": "130.01"}, {"USD": "130"}},
		"currency missing prior": {{"USD": "1", "EUR": "2"}, {"USD": "1"}},
		"currency missing now":   {{"USD": "1"}, {"USD": "1", "EUR": "2"}},
	} {
		if err := OpeningContinuity(c[0], c[1]); !errors.Is(err, ErrDeclaredControl) {
			t.Fatalf("%s: err = %v, want ErrDeclaredControl", name, err)
		}
	}
}

func TestCompleteness(t *testing.T) {
	out, err := VerifyDeclaredControls(DeclaredControls{}, seqRecs(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out {
		if o.Status != OutcomeNotDeclared {
			t.Fatalf("an undeclared control must read NOT_DECLARED, never VERIFIED: %+v", o)
		}
	}
	part := DeclaredControls{Completeness: &DeclaredCompleteness{Scope: "PARTIAL", Description: "EMEA accounts only"}}
	out, err = VerifyDeclaredControls(part, seqRecs(1))
	if err != nil || out[3].Status != OutcomePartial || !part.Partial() {
		t.Fatalf("partial = %+v, %v", out, err)
	}
	mustFail(t, DeclaredControls{Completeness: &DeclaredCompleteness{Scope: "PARTIAL"}}, seqRecs(1), "which subset")
	mustFail(t, DeclaredControls{Completeness: &DeclaredCompleteness{Scope: "MOSTLY"}}, seqRecs(1), "unknown completeness")
	full := DeclaredControls{Completeness: &DeclaredCompleteness{Scope: "FULL"}}
	if out, _ := VerifyDeclaredControls(full, seqRecs(1)); out[3].Status != OutcomeDeclaredFull || full.Partial() {
		t.Fatalf("full = %+v", out)
	}
}
