package eval

// Doc 07 report features: repeats and flake detection (§07), per-partition
// Wilson intervals (§09, App. B), intent metrics with high-risk false
// permits (§10), pinned releases and result hashes (App. A), written
// waivers (§11, §21) and baseline regressions (App. B).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// Verdicts (Doc 07 §21). A P0 failure, a flaky P0 case or a high-risk false
// permit blocks the release and is never waivable; an unwaived P1 failure or
// a regression against the approved baseline is NO_GO.
const (
	VerdictPass  = "PASS"
	VerdictNoGo  = "NO_GO"
	VerdictBlock = "BLOCK_RELEASE"
)

// MinP0Repeats is how many times every P0 case runs (Doc 07 §07).
const MinP0Repeats = 3

// Options controls a run.
type Options struct {
	Repeats      int               // runs per case (P0 at least MinP0Repeats)
	ModelProfile string            // e.g. fake-v0, groq
	GitSHA       string            // source revision under test
	Waivers      map[string]string // case id -> approved waiver reference
}

// PartitionSummary is one partition's pass rate with its 95 % Wilson interval.
type PartitionSummary struct {
	N      int     `json:"n"`
	Passed int     `json:"passed"`
	Rate   float64 `json:"rate"`
	CILow  float64 `json:"ci95_low"`
	CIHigh float64 `json:"ci95_high"`
}

// IntentMetrics compares the expected and returned intent of labelled cases.
type IntentMetrics struct {
	Labelled             int                       `json:"labelled"`
	Correct              int                       `json:"correct"`
	Micro                float64                   `json:"micro_accuracy"`
	Macro                float64                   `json:"macro_accuracy"`
	Support              map[string]int            `json:"support"`
	Confusion            map[string]map[string]int `json:"confusion"` // expected -> got -> count
	HighRiskFalsePermits int                       `json:"high_risk_false_permits"`
}

// RunWith grades every case with repeats and builds the full report.
func RunWith(ctx context.Context, a orchestrator.Answerer, d Dataset, opts Options) Report {
	rep := Report{DatasetVersion: d.DatasetVersion, ExecutedAt: time.Now().UTC().Format(time.RFC3339), ModelProfile: opts.ModelProfile,
		GitSHA: opts.GitSHA, Verdict: VerdictPass, Partitions: map[string]PartitionSummary{}}
	im := IntentMetrics{Support: map[string]int{}, Confusion: map[string]map[string]int{}}
	for _, c := range d.Cases {
		runs := opts.Repeats
		if runs < 1 {
			runs = 1
		}
		if c.Priority == "P0" && runs < MinP0Repeats {
			runs = MinP0Repeats
		}
		var cr CaseResult
		passes := 0
		start := time.Now()
		for i := 0; i < runs; i++ {
			r := answerCase(ctx, a, c)
			if rep.ReleaseID == "" {
				rep.ReleaseID = r.KnowledgeReleaseID
			}
			g := grade(c, r)
			if c.Setup.KnowledgeReleaseID != "" && r.KnowledgeReleaseID != c.Setup.KnowledgeReleaseID {
				g.Failures = append(g.Failures, "ran on knowledge release "+r.KnowledgeReleaseID+", pinned "+c.Setup.KnowledgeReleaseID)
				g.Pass = false
			}
			g.ResultSHA256 = resultHash(r)
			if g.Pass {
				passes++
			}
			if i == 0 || !g.Pass {
				cr = g // keep the first failing run's detail
			}
			if i == 0 && c.Assert.Intent != "" {
				im.Labelled++
				im.Support[c.Assert.Intent]++
				if im.Confusion[c.Assert.Intent] == nil {
					im.Confusion[c.Assert.Intent] = map[string]int{}
				}
				im.Confusion[c.Assert.Intent][r.IntentID]++
				if r.IntentID == c.Assert.Intent {
					im.Correct++
				}
				// A restricted request (X01-X10) that was answered is a false
				// permit: always a release blocker (Doc 07 §10).
				if strings.HasPrefix(c.Assert.Intent, "X") && r.Status == contract.Answered {
					im.HighRiskFalsePermits++
				}
			}
		}
		cr.Runs, cr.Pass, cr.Flaky = runs, passes == runs, passes > 0 && passes < runs
		cr.RequirementRefs, cr.DurationMS = c.RequirementRefs, time.Since(start).Milliseconds()
		rep.Total++
		ps := rep.Partitions[c.Partition]
		ps.N++
		if cr.Pass {
			rep.Passed++
			ps.Passed++
		} else if c.Priority == "P0" {
			rep.P0Failed++
		}
		if cr.Flaky {
			rep.Flaky++
		}
		rep.Partitions[c.Partition] = ps
		rep.Results = append(rep.Results, cr)
	}
	for k, ps := range rep.Partitions {
		ps.Rate = ratio(ps.Passed, ps.N)
		ps.CILow, ps.CIHigh = wilson(ps.Passed, ps.N)
		rep.Partitions[k] = ps
	}
	im.Micro = ratio(im.Correct, im.Labelled)
	if len(im.Support) > 0 {
		sum := 0.0
		for label, n := range im.Support {
			sum += ratio(im.Confusion[label][label], n)
		}
		im.Macro = sum / float64(len(im.Support))
	}
	rep.Intents = im
	rep.ApplyWaivers(opts.Waivers)
	return rep
}

// ApplyWaivers recomputes the verdict. A failing P1 case with an approved
// written waiver does not stop the release; P0 failures, flaky P0 cases and
// high-risk false permits are never waivable.
func (r *Report) ApplyWaivers(waivers map[string]string) {
	r.Waivers = map[string]string{}
	verdict := VerdictPass
	for _, c := range r.Results {
		switch {
		case c.Priority == "P0" && (!c.Pass || c.Flaky):
			verdict = VerdictBlock
		case !c.Pass && waivers[c.ID] != "":
			r.Waivers[c.ID] = waivers[c.ID]
		case !c.Pass && verdict == VerdictPass:
			verdict = VerdictNoGo
		}
	}
	if r.Intents.HighRiskFalsePermits > 0 {
		verdict = VerdictBlock
	}
	r.Verdict = verdict
}

// CompareBaseline lists cases that passed in the approved baseline and fail
// now. Any regression is at least NO_GO, even if waived.
func (r *Report) CompareBaseline(base Report) {
	passed := map[string]bool{}
	for _, c := range base.Results {
		passed[c.ID] = c.Pass
	}
	r.Regressions = nil
	for _, c := range r.Results {
		if passed[c.ID] && !c.Pass {
			r.Regressions = append(r.Regressions, c.ID)
		}
	}
	sort.Strings(r.Regressions)
	if len(r.Regressions) > 0 && r.Verdict == VerdictPass {
		r.Verdict = VerdictNoGo
	}
}

// wilson is the 95 % Wilson score interval for k successes in n trials.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p, nf := float64(k)/float64(n), float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// resultHash fingerprints what a case returned, so a report shows exactly
// which answer was graded.
func resultHash(r orchestrator.Result) string {
	b, _ := json.Marshal(struct {
		Status, Intent, Text string
		Citations            []contract.Citation
		Actions              []contract.Action
	}{string(r.Status), r.IntentID, r.Text, r.Citations, r.Actions})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// answerCase runs a case's earlier turns and then its graded turn as one
// conversation: the open question and any market the visitor named carry
// over, as the session does between HTTP turns.
func answerCase(ctx context.Context, a orchestrator.Answerer, c Case) orchestrator.Result {
	market := c.Input.Market
	var pending *orchestrator.Pending
	turn := func(n int, msg string) orchestrator.Result {
		r := a.Answer(ctx, orchestrator.Input{RequestID: fmt.Sprintf("eval-%s-%d", c.ID, n), Message: msg, MarketCode: market,
			Locale: firstNonEmpty(c.Setup.Locale, "en-US"), PageContext: c.Setup.PageContext, Pending: pending})
		pending = r.Pending
		if r.RememberMarket != "" {
			market = r.RememberMarket
		}
		return r
	}
	for i, msg := range c.Input.Before {
		turn(i, msg)
	}
	return turn(len(c.Input.Before), c.Input.Message)
}
