package envelope

import (
	"testing"
)

func TestAllReasonFamiliesAndCodesValid(t *testing.T) {
	expectedCounts := map[ReasonFamily]int{
		ReasonFamilyCancel:    4,
		ReasonFamilyReturn:    4,
		ReasonFamilyReject:    4,
		ReasonFamilyReverse:   4,
		ReasonFamilyReopen:    4,
		ReasonFamilyWriteOff:  4,
		ReasonFamilyTerminate: 5,
		ReasonFamilyOverride:  3,
		ReasonFamilyExternal:  4,
	}

	totalCodes := 0
	for fam, expectedCount := range expectedCounts {
		if !IsValidReasonFamily(fam) {
			t.Errorf("expected family %q to be valid", fam)
		}
		codes, ok := canonicalRegistry[fam]
		if !ok {
			t.Fatalf("missing registry entry for family %q", fam)
		}
		if len(codes) != expectedCount {
			t.Errorf("family %q has %d codes, want %d", fam, len(codes), expectedCount)
		}
		for code := range codes {
			if !IsValidReasonCode(fam, code) {
				t.Errorf("expected code %q to be valid in family %q", code, fam)
			}
			totalCodes++
		}
	}

	if totalCodes != 36 {
		t.Errorf("total registered codes = %d, want 36", totalCodes)
	}
}

func TestInvalidFamilyAndCodes(t *testing.T) {
	if IsValidReasonFamily(ReasonFamily("UNKNOWN_FAMILY")) {
		t.Error("expected UNKNOWN_FAMILY to be invalid")
	}

	if IsValidReasonCode(ReasonFamilyCancel, ReasonCode("NON_EXISTENT_CODE")) {
		t.Error("expected NON_EXISTENT_CODE to be invalid for CANCEL")
	}

	// Code from REVERSE family should not be valid under CANCEL
	if IsValidReasonCode(ReasonFamilyCancel, ReasonReversePostingError) {
		t.Error("expected POSTING_ERROR to be invalid for CANCEL family")
	}
}

func TestParseReason(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantFam    ReasonFamily
		wantCode   ReasonCode
		wantErrMsg string
	}{
		// Valid composite format "FAMILY:CODE"
		{
			name:     "composite cancel",
			raw:      "CANCEL:CUSTOMER_REQUEST",
			wantFam:  ReasonFamilyCancel,
			wantCode: ReasonCancelCustomerRequest,
		},
		{
			name:     "composite slash separator and lowercase",
			raw:      "reject/policy_not_met",
			wantFam:  ReasonFamilyReject,
			wantCode: ReasonRejectPolicyNotMet,
		},
		// Valid prefixed format "FAMILY_CODE"
		{
			name:     "prefixed reverse",
			raw:      "REVERSE_POSTING_ERROR",
			wantFam:  ReasonFamilyReverse,
			wantCode: ReasonReversePostingError,
		},
		{
			name:     "prefixed reopen",
			raw:      "REOPEN_AUTHORIZED_CORRECTION",
			wantFam:  ReasonFamilyReopen,
			wantCode: ReasonReopenAuthorizedCorrection,
		},
		// Valid bare format
		{
			name:     "bare code authority denied",
			raw:      "AUTHORITY_DENIED",
			wantFam:  ReasonFamilyReject,
			wantCode: ReasonRejectAuthorityDenied,
		},
		{
			name:     "bare code emergency op",
			raw:      "EMERGENCY_OPERATION",
			wantFam:  ReasonFamilyOverride,
			wantCode: ReasonOverrideEmergencyOp,
		},
		{
			name:     "bare code with whitespace",
			raw:      "  NETWORK_FAILURE  ",
			wantFam:  ReasonFamilyExternal,
			wantCode: ReasonExternalNetworkFailure,
		},
		// Invalid cases: free-text, unknown, or empty
		{
			name:       "empty reason",
			raw:        "",
			wantErrMsg: "reason code is required and cannot be empty",
		},
		{
			name:       "whitespace only",
			raw:        "   ",
			wantErrMsg: "reason code is required and cannot be empty",
		},
		{
			name:       "free text reason",
			raw:        "changed my mind because invoice was wrong",
			wantErrMsg: "unknown or ungoverned reason code",
		},
		{
			name:       "invalid composite code",
			raw:        "CANCEL:NOT_A_REAL_CODE",
			wantErrMsg: "invalid reason code",
		},
		{
			name:       "mismatched composite family and code",
			raw:        "CANCEL:POSTING_ERROR",
			wantErrMsg: "invalid reason code",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fam, code, err := ParseReason(tc.raw)
			if tc.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fam != tc.wantFam {
				t.Errorf("family = %q, want %q", fam, tc.wantFam)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestLookupReasonFamily(t *testing.T) {
	fam, ok := LookupReasonFamily(ReasonReturnMissingEvidence)
	if !ok || fam != ReasonFamilyReturn {
		t.Errorf("lookup for MISSING_EVIDENCE = (%q, %v), want (RETURN, true)", fam, ok)
	}

	_, ok = LookupReasonFamily(ReasonCode("DOES_NOT_EXIST"))
	if ok {
		t.Error("lookup for non-existent code should return false")
	}
}

func TestExceptionClassRetryable(t *testing.T) {
	if !ExceptionClassTechnicalTransient.Retryable() {
		t.Error("TECHNICAL_TRANSIENT must be retryable per §16")
	}

	nonRetryable := []ExceptionClass{
		ExceptionClassValidation,
		ExceptionClassBusinessRule,
		ExceptionClassAuthorization,
		ExceptionClassConcurrency,
		ExceptionClassExternalRejection,
		ExceptionClassTechnicalPermanent,
		ExceptionClassControlException,
	}

	for _, ec := range nonRetryable {
		if ec.Retryable() {
			t.Errorf("exception class %q must NOT be retryable per §16", ec)
		}
	}
}
