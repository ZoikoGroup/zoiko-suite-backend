package evaluator

import (
	"errors"
	"testing"
)

const matrix = `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["NDA"],"bands":[{"band_id":"A","above":null,"up_to":null,"min_distinct_signers":1,"required_offices":[{"office":"DIRECTOR","min_count":1}]}]}`

func TestSignatoryMatrix_SupportedAndValidated(t *testing.T) {
	if !Supports(TypeSignatoryMatrix) || VersionOf(TypeSignatoryMatrix) != VersionSignatoryMatrix {
		t.Fatal("SIGNATORY_MATRIX must be evaluable")
	}
	for _, typ := range []string{"SPEND_CONTROL", "SOD_RULE", "LEGAL_BASIS"} {
		if Supports(typ) {
			t.Fatalf("%s must never be evaluated by policy-svc", typ)
		}
		if _, err := Evaluate(typ, "v", []byte(`{}`), []byte(`{}`)); !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	if err := ValidatePayload(TypeSignatoryMatrix, []byte(matrix)); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePayload(TypeSignatoryMatrix, []byte(`{"threshold_amount":1}`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("invalid matrix accepted: %v", err)
	}
	o, err := Evaluate(TypeSignatoryMatrix, "v", []byte(matrix), []byte(`{"instrument_type":"NDA","amount":"5","currency":"GBP","basis":"GROSS"}`))
	if err != nil || o.Result != "SIGNATURE_REQUIRED" || o.Signatory == nil || o.EvaluatorVersion != VersionSignatoryMatrix {
		t.Fatalf("%+v %v", o, err)
	}
	var in *InputError
	if _, err := Evaluate(TypeSignatoryMatrix, "v", []byte(matrix), []byte(`{"amount":"5"}`)); !errors.As(err, &in) || in.Field != "action_context.instrument_type" {
		t.Fatalf("input error expected, got %v", err)
	}
}
