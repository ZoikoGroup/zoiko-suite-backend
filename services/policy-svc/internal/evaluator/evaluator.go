// Package evaluator is policy-svc's deterministic rule evaluation: a pure
// function of a version's payload and an action context (V-001 PDC-I-09,
// PDC-I-10, PDC-I-14). The live evaluation, the certification test runner and
// the differential simulation all call it, so a test result or a simulated
// result is the result production would give (PDC-I-18).
package evaluator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"zoiko.io/policy-svc/internal/signatory"
)

// Version names an evaluator and its contract version; it is stored with
// every evaluation and test run (PDC-I-03, PDC-I-17).
const (
	TypeApprovalThreshold    = "APPROVAL_THRESHOLD"
	VersionApprovalThreshold = "policy-svc/APPROVAL_THRESHOLD@1"
	TypeSignatoryMatrix      = signatory.PolicyType
	VersionSignatoryMatrix   = signatory.EvaluatorVersion
)

// Results of APPROVAL_THRESHOLD.
const (
	ResultWithinThreshold  = "WITHIN_THRESHOLD"
	ResultApprovalRequired = "APPROVAL_REQUIRED"
)

var (
	// ErrNotImplemented: the policy type has no evaluator. It can be neither
	// evaluated nor certified (V-001: blocked until evaluator capability is
	// supported).
	ErrNotImplemented = errors.New("policy type has no evaluator")
	// ErrInvalidPayload: the stored version cannot be evaluated.
	ErrInvalidPayload = errors.New("policy payload cannot be evaluated")
)

// InputError is a problem with the action context, naming the field.
type InputError struct {
	Field  string
	Reason string // missing | invalid | currency_mismatch
	Detail string
}

func (e *InputError) Error() string { return e.Field + ": " + e.Reason + " " + e.Detail }

// Outcome is one evaluation's result and the rules that decided it.
type Outcome struct {
	Result           string
	SelectedRuleIDs  []string
	EvaluatorVersion string
	// Signatory is the full resolution of a SIGNATORY_MATRIX evaluation
	// (required actions, reasons, band); nil for other types.
	Signatory *signatory.Result
}

// Supports reports whether policyType has an evaluator. Spend controls,
// segregation of duties and legal basis never do: their owning services
// decide them (ZS-GOV-POL-001 PD-01, PD-03).
func Supports(policyType string) bool {
	return policyType == TypeApprovalThreshold || policyType == TypeSignatoryMatrix
}

// VersionOf returns the evaluator version of a supported type, "" otherwise.
func VersionOf(policyType string) string {
	switch policyType {
	case TypeApprovalThreshold:
		return VersionApprovalThreshold
	case TypeSignatoryMatrix:
		return VersionSignatoryMatrix
	}
	return ""
}

// ValidatePayload reports why a stored payload cannot be evaluated, nil when
// it can (or when the type has no evaluator to check it against).
func ValidatePayload(policyType string, payload json.RawMessage) error {
	switch policyType {
	case TypeApprovalThreshold:
		if amt, ok, err := ExactAmount(payload, "threshold_amount"); err != nil || !ok || amt.Sign() < 0 {
			return fmt.Errorf("%w: rule_payload.threshold_amount is missing, not an exact number, or negative", ErrInvalidPayload)
		}
	case TypeSignatoryMatrix:
		if _, err := signatory.ParseMatrix(payload); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidPayload, err)
		}
	}
	return nil
}

// Evaluate decides actionContext under a version's payload. versionID only
// labels the selected rules.
func Evaluate(policyType, versionID string, payload, actionContext json.RawMessage) (Outcome, error) {
	switch policyType {
	case TypeApprovalThreshold:
		return approvalThreshold(versionID, payload, actionContext)
	case TypeSignatoryMatrix:
		return signatoryMatrix(versionID, payload, actionContext)
	default:
		return Outcome{}, ErrNotImplemented
	}
}

// signatoryMatrix resolves the signer requirement. Its result is the Doc 02
// outcome itself: SIGNATURE_REQUIRED, PERMIT (explicit exemption only) or
// NOT_CONFIGURED.
func signatoryMatrix(versionID string, payload, actionContext json.RawMessage) (Outcome, error) {
	m, err := signatory.ParseMatrix(payload)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	in, err := signatory.ParseInput(actionContext)
	if err != nil {
		var ie *signatory.InputError
		if errors.As(err, &ie) {
			return Outcome{}, &InputError{Field: ie.Field, Reason: ie.Reason, Detail: ie.Detail}
		}
		return Outcome{}, &InputError{Field: "action_context", Reason: "invalid", Detail: err.Error()}
	}
	res := signatory.Evaluate(versionID, m, in)
	return Outcome{Result: res.Outcome, SelectedRuleIDs: res.RuleIDs, EvaluatorVersion: VersionSignatoryMatrix, Signatory: &res}, nil
}

// approvalThreshold: amount > threshold_amount is APPROVAL_REQUIRED, otherwise
// WITHIN_THRESHOLD, compared exactly. A version that names its currency is
// only compared with an amount in that currency (PDC-I-11).
func approvalThreshold(versionID string, payload, actionContext json.RawMessage) (Outcome, error) {
	threshold, present, err := ExactAmount(payload, "threshold_amount")
	if err != nil || !present {
		return Outcome{}, fmt.Errorf("%w: threshold_amount", ErrInvalidPayload)
	}
	if len(actionContext) == 0 {
		return Outcome{}, &InputError{Field: "action_context.amount", Reason: "missing"}
	}
	amount, present, err := ExactAmount(actionContext, "amount")
	if err != nil || !present {
		return Outcome{}, &InputError{Field: "action_context.amount", Reason: "missing"}
	}
	// A negative amount is within any threshold, so accepting one would let
	// a caller route any action past approval by flipping its sign.
	if amount.Sign() < 0 {
		return Outcome{}, &InputError{Field: "action_context.amount", Reason: "invalid", Detail: "must not be negative"}
	}
	rules := []string{versionID + "#threshold_amount"}
	if want := StringField(payload, "currency"); want != "" {
		got := StringField(actionContext, "currency")
		switch {
		case got == "":
			return Outcome{}, &InputError{Field: "action_context.currency", Reason: "missing"}
		case got != want:
			return Outcome{}, &InputError{Field: "action_context.currency", Reason: "currency_mismatch", Detail: want}
		}
		rules = append(rules, versionID+"#currency")
	}
	result := ResultWithinThreshold
	if amount.Cmp(threshold) > 0 {
		result = ResultApprovalRequired
	}
	return Outcome{Result: result, SelectedRuleIDs: rules, EvaluatorVersion: VersionApprovalThreshold}, nil
}

// ExactAmount extracts field from a JSON object as an exact decimal, never a
// float64 (PDC-I-11). (nil, false, nil) when absent or null.
func ExactAmount(raw json.RawMessage, field string) (*big.Rat, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, false, err
	}
	v, present := obj[field]
	if !present || v == nil {
		return nil, false, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return nil, false, fmt.Errorf("%s is not a JSON number", field)
	}
	amount, ok := new(big.Rat).SetString(num.String())
	if !ok {
		return nil, false, fmt.Errorf("%s is not a valid decimal number: %q", field, num.String())
	}
	return amount, true, nil
}

// StringField reads a top-level string field of a JSON object, "" if absent
// or not a string.
func StringField(raw json.RawMessage, field string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(obj[field], &s) != nil {
		return ""
	}
	return s
}
