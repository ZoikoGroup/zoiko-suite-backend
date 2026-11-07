package eval

import (
	"context"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

type answerFunc2 struct {
	status contract.Status
	text   string
}

func (a answerFunc2) Answer(context.Context, orchestrator.Input) orchestrator.Result {
	return orchestrator.Result{Status: a.status, IntentID: "I12", Text: a.text, KnowledgeReleaseID: "kb"}
}

func answerFunc(status, text string) orchestrator.Answerer {
	return answerFunc2{contract.Status(status), text}
}
