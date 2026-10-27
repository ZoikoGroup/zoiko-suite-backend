// Package eval runs a versioned golden dataset through the pipeline and
// grades it deterministically (Doc 07 §06–§08, App. A). Any P0 failure
// blocks the release (Doc 07 §19 BLOCK_RELEASE). Semantic-rubric and human
// grading lanes are out of scope for this harness.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// Dataset is a golden set file.
type Dataset struct {
	DatasetVersion string `json:"dataset_version"`
	Note           string `json:"note"`
	Cases          []Case `json:"cases"`
}

// Case is one fixture (Doc 07 App. A, deterministic lane).
type Case struct {
	ID              string   `json:"id"`
	Partition       string   `json:"partition"`
	Priority        string   `json:"priority"`
	RequirementRefs []string `json:"requirement_refs"`
	Input           struct {
		Message string `json:"message"`
		Market  string `json:"market"`
	} `json:"input"`
	Assert struct {
		Outcome           string   `json:"application_outcome"`
		OutcomeAny        []string `json:"application_outcome_any"`
		Intent            string   `json:"intent"`
		MustContain       []string `json:"must_contain"`
		MustNotContain    []string `json:"must_not_contain"`
		RequiredClaimIDs  []string `json:"required_claim_ids"`
		ForbiddenClaimIDs []string `json:"forbidden_claim_ids"`
		AllowedActions    []string `json:"allowed_actions"`
		MinCitations      int      `json:"min_citations"`
		MaxCitations      *int     `json:"max_citations"`
	} `json:"assert"`
}

// CaseResult is the graded outcome.
type CaseResult struct {
	ID        string   `json:"id"`
	Partition string   `json:"partition"`
	Priority  string   `json:"priority"`
	Pass      bool     `json:"pass"`
	Status    string   `json:"status"`
	IntentID  string   `json:"intent_id"`
	Failures  []string `json:"failures,omitempty"`
}

// Report is the evidence artifact for a run.
type Report struct {
	DatasetVersion string       `json:"dataset_version"`
	ExecutedAt     string       `json:"executed_at"`
	ModelProfile   string       `json:"model_profile"`
	ReleaseID      string       `json:"knowledge_release_id"`
	Total          int          `json:"total"`
	Passed         int          `json:"passed"`
	P0Failed       int          `json:"p0_failed"`
	Verdict        string       `json:"verdict"` // PASS | BLOCK_RELEASE
	Results        []CaseResult `json:"results"`
}

// Load reads a dataset file.
func Load(path string) (Dataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, err
	}
	var d Dataset
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Dataset{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// Run grades every case.
func Run(ctx context.Context, a orchestrator.Answerer, d Dataset, modelProfile string) Report {
	rep := Report{DatasetVersion: d.DatasetVersion, ExecutedAt: time.Now().UTC().Format(time.RFC3339), ModelProfile: modelProfile, Verdict: "PASS"}
	for _, c := range d.Cases {
		r := a.Answer(ctx, orchestrator.Input{RequestID: "eval-" + c.ID, Message: c.Input.Message, MarketCode: c.Input.Market, Locale: "en-US"})
		if rep.ReleaseID == "" {
			rep.ReleaseID = r.KnowledgeReleaseID
		}
		cr := grade(c, r)
		rep.Total++
		if cr.Pass {
			rep.Passed++
		} else if c.Priority == "P0" {
			rep.P0Failed++
			rep.Verdict = "BLOCK_RELEASE"
		}
		rep.Results = append(rep.Results, cr)
	}
	return rep
}

func grade(c Case, r orchestrator.Result) CaseResult {
	cr := CaseResult{ID: c.ID, Partition: c.Partition, Priority: c.Priority, Status: string(r.Status), IntentID: r.IntentID}
	fail := func(f string, args ...any) { cr.Failures = append(cr.Failures, fmt.Sprintf(f, args...)) }
	a := c.Assert
	if a.Outcome != "" && string(r.Status) != a.Outcome {
		fail("outcome %s, want %s", r.Status, a.Outcome)
	}
	if len(a.OutcomeAny) > 0 && !contains(a.OutcomeAny, string(r.Status)) {
		fail("outcome %s, want one of %v", r.Status, a.OutcomeAny)
	}
	if a.Intent != "" && r.IntentID != a.Intent {
		fail("intent %s, want %s", r.IntentID, a.Intent)
	}
	lower := strings.ToLower(r.Text)
	for _, s := range a.MustContain {
		if !strings.Contains(lower, strings.ToLower(s)) {
			fail("text missing %q", s)
		}
	}
	for _, s := range a.MustNotContain {
		if strings.Contains(lower, strings.ToLower(s)) {
			fail("text contains forbidden %q", s)
		}
	}
	cited := map[string]bool{}
	for _, ci := range r.Citations {
		for _, id := range ci.ClaimIDs {
			cited[id] = true
		}
	}
	for _, id := range a.RequiredClaimIDs {
		if !cited[id] {
			fail("claim %s not cited", id)
		}
	}
	for _, id := range a.ForbiddenClaimIDs {
		if cited[id] {
			fail("forbidden claim %s cited", id)
		}
	}
	for _, act := range r.Actions {
		if act.Type != contract.ActionOpenRoute {
			fail("non-route action %s", act.Type)
		}
		if len(a.AllowedActions) > 0 && !contains(a.AllowedActions, act.RouteKey) {
			fail("action %s not allowed", act.RouteKey)
		}
	}
	if len(a.AllowedActions) > 0 && len(r.Actions) == 0 {
		fail("expected an official route among %v", a.AllowedActions)
	}
	if len(r.Citations) < a.MinCitations {
		fail("%d citations, want ≥%d", len(r.Citations), a.MinCitations)
	}
	if a.MaxCitations != nil && len(r.Citations) > *a.MaxCitations {
		fail("%d citations, want ≤%d", len(r.Citations), *a.MaxCitations)
	}
	// Universal invariants (every case): ANSWERED needs provenance.
	if r.Status == contract.Answered && len(r.Citations) == 0 {
		fail("ANSWERED without citations")
	}
	cr.Pass = len(cr.Failures) == 0
	return cr
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
