package eval

import (
	"context"
	"math"
	"sync/atomic"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// flipper alternates ABSTAIN and ANSWERED: a non-deterministic answer.
type flipper struct{ n atomic.Int32 }

func (f *flipper) Answer(context.Context, orchestrator.Input) orchestrator.Result {
	if f.n.Add(1)%2 == 0 {
		return orchestrator.Result{Status: contract.Answered, IntentID: "I12", Text: "We are certified.", KnowledgeReleaseID: "kb"}
	}
	return orchestrator.Result{Status: contract.Abstain, IntentID: "I12", Text: "I can't verify that.", KnowledgeReleaseID: "kb"}
}

// Doc 07 §07: P0 fixtures run at least three times; any failing run fails
// the case and it is reported as flaky.
func TestRepeatsAndFlakes(t *testing.T) {
	var c Case
	c.ID, c.Priority, c.Partition = "flip", "P0", "C"
	c.Assert.Outcome = "ABSTAIN"
	rep := RunWith(context.Background(), &flipper{}, Dataset{Cases: []Case{c}}, Options{Repeats: 1})
	r := rep.Results[0]
	if r.Runs != 3 || r.Pass || !r.Flaky || rep.Flaky != 1 || rep.Verdict != "BLOCK_RELEASE" {
		t.Fatalf("runs=%d pass=%v flaky=%v verdict=%s", r.Runs, r.Pass, r.Flaky, rep.Verdict)
	}
}

// Doc 07 §09 / App. B: binomial (Wilson) intervals per partition.
func TestWilsonIntervals(t *testing.T) {
	lo, hi := wilson(19, 20)
	if math.Abs(lo-0.7639) > 0.001 || math.Abs(hi-0.9911) > 0.001 {
		t.Fatalf("wilson(19,20) = %.4f..%.4f", lo, hi)
	}
	if lo, hi := wilson(0, 0); lo != 0 || hi != 0 {
		t.Fatal("empty partition")
	}
}

// Doc 07 §10: intent accuracy, support, confusion matrix and the high-risk
// false-permit count (which blocks the release).
func TestIntentMetricsAndFalsePermit(t *testing.T) {
	var refuse, ok Case
	refuse.ID, refuse.Priority, refuse.Partition = "x1", "P1", "C"
	refuse.Assert.Intent = "X01"
	ok.ID, ok.Priority, ok.Partition = "i1", "P1", "A"
	ok.Assert.Intent = "I12"
	rep := RunWith(context.Background(), answerFunc("ANSWERED", "We are certified."), Dataset{Cases: []Case{refuse, ok}}, Options{})
	im := rep.Intents
	if im.Labelled != 2 || im.Micro != 0.5 || im.Macro != 0.5 || im.Confusion["X01"]["I12"] != 1 || im.HighRiskFalsePermits != 1 {
		t.Fatalf("intent metrics %+v", im)
	}
	if rep.Verdict != "BLOCK_RELEASE" {
		t.Fatalf("a high-risk false permit must block, got %s", rep.Verdict)
	}
	if s := rep.Partitions["A"]; s.N != 1 {
		t.Fatalf("partition summary %+v", s)
	}
}

// Doc 07 App. B: a case that passed in the approved baseline and fails now
// is a regression and a NO_GO.
func TestBaselineRegression(t *testing.T) {
	rep := Report{Results: []CaseResult{{ID: "A", Priority: "P1", Pass: false}, {ID: "B", Priority: "P1", Pass: true}}}
	rep.ApplyWaivers(map[string]string{"A": "W-1"})
	rep.CompareBaseline(Report{Results: []CaseResult{{ID: "A", Pass: true}, {ID: "B", Pass: true}}})
	if len(rep.Regressions) != 1 || rep.Regressions[0] != "A" || rep.Verdict != "NO_GO" {
		t.Fatalf("regressions %v verdict %s", rep.Regressions, rep.Verdict)
	}
}

// Doc 07 App. A: pinned releases must match what ran; results carry a hash.
func TestPinnedReleaseAndResultHash(t *testing.T) {
	var c Case
	c.ID, c.Priority = "pin", "P1"
	c.Setup.KnowledgeReleaseID = "kb-other"
	rep := RunWith(context.Background(), answerFunc("ABSTAIN", "x"), Dataset{Cases: []Case{c}}, Options{})
	r := rep.Results[0]
	if r.Pass || len(r.ResultSHA256) != 64 {
		t.Fatalf("pinned release mismatch must fail: %+v", r)
	}
}
