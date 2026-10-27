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
	Tokens             int64
	// LatencyBucket and SecuritySignal feed redacted events.
	SecuritySignal string
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
