// Package eval runs a versioned golden dataset through the pipeline and
// grades it deterministically (Doc 07 §06–§08, App. A). Any P0 failure
// blocks the release (Doc 07 §19 BLOCK_RELEASE). Semantic-rubric and human
// grading lanes are out of scope for this harness.
package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// Dataset is a golden set file.
type Dataset struct {
	DatasetVersion string `json:"dataset_version"`
	Note           string `json:"note"`
	// LaunchIntents are the intents in launch scope (Doc 07 §10): each one
	// must have at least one labelled case, or intent accuracy across launch
	// intents cannot be claimed.
	LaunchIntents []string `json:"launch_intents,omitempty"`
	Cases         []Case   `json:"cases"`
	// SHA256 is the digest of the file as loaded (Doc 07 §19 evidence).
	SHA256 string `json:"-"`
}

// Case is one fixture (Doc 07 App. A, deterministic lane).
type Case struct {
	ID              string   `json:"id"`
	Partition       string   `json:"partition"`
	Priority        string   `json:"priority"`
	RequirementRefs []string `json:"requirement_refs"`
	// Setup pins what the case runs against (Doc 07 App. A).
	Setup struct {
		Locale             string `json:"locale"`
		PageContext        string `json:"page_context"`
		KnowledgeReleaseID string `json:"knowledge_release_id"`
	} `json:"setup"`
	Input struct {
		// Before are earlier turns of the same conversation, sent first
		// (Doc 07 App. A multi-turn cases); only the last turn is graded.
		Before  []string `json:"before,omitempty"`
		Message string   `json:"message"`
		Market  string   `json:"market"`
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
	// Runs, Flaky: P0 cases run at least three times; any failing run fails
	// the case and mixed results mark it flaky (Doc 07 §07).
	Runs            int      `json:"runs"`
	Flaky           bool     `json:"flaky,omitempty"`
	ResultSHA256    string   `json:"result_sha256"`
	RequirementRefs []string `json:"requirement_refs,omitempty"`
	DurationMS      int64    `json:"duration_ms"`
}

// Report is the evidence artifact for a run.
type Report struct {
	DatasetVersion string `json:"dataset_version"`
	ExecutedAt     string `json:"executed_at"`
	ModelProfile   string `json:"model_profile"`
	ReleaseID      string `json:"knowledge_release_id"`
	Total          int    `json:"total"`
	Passed         int    `json:"passed"`
	P0Failed       int    `json:"p0_failed"`
	Flaky          int    `json:"flaky"`
	Verdict        string `json:"verdict"` // PASS | NO_GO | BLOCK_RELEASE
	GitSHA         string `json:"git_sha,omitempty"`
	// Provenance (Doc 07 §06/§19, REL-QA-02): what exactly was evaluated.
	PromptVersion   string `json:"prompt_version"`
	PolicyVersion   string `json:"policy_version,omitempty"`
	PolicyDigest    string `json:"policy_digest"`
	ClaimsReleaseID string `json:"claims_release_id,omitempty"`
	ModelID         string `json:"model_id"`
	DatasetSHA256   string `json:"dataset_sha256,omitempty"`
	// Partitions, Intents, Waivers and Regressions: Doc 07 §09-§11, App. B.
	Partitions  map[string]PartitionSummary `json:"partitions"`
	Intents     IntentMetrics               `json:"intents"`
	Waivers     map[string]string           `json:"waivers,omitempty"`
	Regressions []string                    `json:"regressions,omitempty"`
	Results     []CaseResult                `json:"results"`
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
	sum := sha256.Sum256(b)
	d.SHA256 = hex.EncodeToString(sum[:])
	return d, nil
}

// Run grades every case once (P0 cases three times) with the given model
// profile; RunWith takes the full options.
func Run(ctx context.Context, a orchestrator.Answerer, d Dataset, modelProfile string) Report {
	return RunWith(ctx, a, d, Options{ModelProfile: modelProfile})
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
