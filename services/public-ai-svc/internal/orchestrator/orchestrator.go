// Package orchestrator runs the fail-closed turn pipeline (Doc 05 §11,
// Doc 06 §08, Doc 04 §08). The HTTP layer only sees Answerer.
package orchestrator

import (
	"context"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Input is a validated, normalized turn. Message never leaves this process
// except to the approved model gateway after redaction.
type Input struct {
	RequestID   string
	SessionID   string
	TurnID      string
	Message     string
	Locale      string
	MarketCode  string
	PageContext string
	// Pending is the carry-over from a previous clarifying question.
	Pending *Pending
}

// Pending is what a clarifying question needs remembered: intent and slot
// values only, never the visitor's words (Doc 02 §10).
type Pending struct {
	Intent         string `json:"intent"`
	Secondary      string `json:"secondary,omitempty"`
	PlanName       string `json:"plan,omitempty"`
	BillingCadence string `json:"billing,omitempty"`
	Integration    string `json:"integration,omitempty"`
	Clarifications int    `json:"clarifications"`
}

// Result is the orchestrator outcome. The HTTP layer adds turn identifiers.
type Result struct {
	Status             contract.Status
	IntentID           string
	Text               string
	Citations          []contract.Citation
	Actions            []contract.Action
	KnowledgeReleaseID string
	Flags              *contract.Flags
	// AnswerClass ("" = contract.DefaultClass), StatusLabel, MarketCode and
	// Clarification are the qualifier fields of the envelope.
	AnswerClass   string
	StatusLabel   string
	MarketCode    string
	Clarification *contract.Clarification
	// SecondaryIntentID is the second intent of a two-part question; Reason
	// (contract.Reason*) says why the turn ended this way.
	SecondaryIntentID string
	Reason            string
	Suggestions       []contract.Chip
	// Draft is set when the answer comes from DRAFT_PENDING_REVIEW articles
	// (served only in local/dev); the UI labels it "Draft content".
	Draft bool
	// Diag is per-turn diagnostics (no message text): logged in dev/test.
	Diag   Diag
	Tokens int64
	// LatencyBucket and SecuritySignal feed redacted events.
	SecuritySignal string
	// Pending, when set, must be stored for the next turn; nil clears it.
	Pending *Pending
}

// Diag records how a turn was decided: slots, claim verdict, retrieval
// counts before and after the access filters, top score and evidence ids.
type Diag struct {
	Slots               map[string]string
	ClaimVerdict        string
	RetrievalMode       string
	CandidatesBeforeACL int
	CandidatesAfterACL  int
	TopScore            float64
	EvidenceIDs         []string
}

// Answerer produces a turn result. Implementations must never return an
// error for business outcomes; dependency failures become UNAVAILABLE.
type Answerer interface {
	Answer(ctx context.Context, in Input) Result
}

// AbstainAll is the STEP 4 placeholder: every turn abstains.
type AbstainAll struct{ ReleaseID string }

func (a AbstainAll) Answer(context.Context, Input) Result {
	return Result{
		Status:             contract.Abstain,
		IntentID:           "UNKNOWN",
		Text:               "I can't verify an answer to that yet.",
		Citations:          []contract.Citation{},
		Actions:            []contract.Action{},
		KnowledgeReleaseID: a.ReleaseID,
	}
}
