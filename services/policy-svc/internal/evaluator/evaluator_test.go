package evaluator_test

import (
	"encoding/json"
	"errors"
	"testing"

	"zoiko.io/policy-svc/internal/evaluator"
)

func TestApprovalThreshold_DeterministicAndExact(t *testing.T) {
	payload := json.RawMessage(`{"threshold_amount": 5000}`)
	for ctx, want := range map[string]string{
		`{"amount": 5000}`:                     evaluator.ResultWithinThreshold,
		`{"amount": 5000.0000000000000000001}`: evaluator.ResultApprovalRequired,
		`{"amount": 0}`:                        evaluator.ResultWithinThreshold,
	} {
		for i := 0; i < 2; i++ { // PDC-I-09: the same input, the same output
			o, err := evaluator.Evaluate(evaluator.TypeApprovalThreshold, "v", payload, json.RawMessage(ctx))
			if err != nil || o.Result != want || o.EvaluatorVersion != evaluator.VersionApprovalThreshold {
				t.Fatalf("%s: %+v %v", ctx, o, err)
			}
		}
	}
}

func TestApprovalThreshold_Refusals(t *testing.T) {
	gbp := json.RawMessage(`{"threshold_amount": 1, "currency": "GBP"}`)
	var in *evaluator.InputError
	for ctx, reason := range map[string]string{
		`{}`:                                "missing",
		`{"amount": "1"}`:                   "missing",
		`{"amount": -1, "currency": "GBP"}`: "invalid",
		`{"amount": 1}`:                     "missing",
		`{"amount": 1, "currency": "EUR"}`:  "currency_mismatch",
	} {
		_, err := evaluator.Evaluate(evaluator.TypeApprovalThreshold, "v", gbp, json.RawMessage(ctx))
		if !errors.As(err, &in) || in.Reason != reason {
			t.Errorf("%s: want %s, got %v", ctx, reason, err)
		}
	}
	if _, err := evaluator.Evaluate(evaluator.TypeApprovalThreshold, "v", json.RawMessage(`{}`), json.RawMessage(`{"amount":1}`)); !errors.Is(err, evaluator.ErrInvalidPayload) {
		t.Errorf("a payload without threshold_amount: %v", err)
	}
	if _, err := evaluator.Evaluate("SPEND_CONTROL", "v", nil, nil); !errors.Is(err, evaluator.ErrNotImplemented) || evaluator.Supports("SPEND_CONTROL") {
		t.Errorf("SPEND_CONTROL has no evaluator: %v", err)
	}
}
