package envelope_test

import (
	"testing"

	"zoiko.io/workflow-svc/internal/envelope"
)

func TestReasonFamily_Constants(t *testing.T) {
	families := []envelope.ReasonFamily{
		envelope.ReasonFamilyCancel,
		envelope.ReasonFamilyReturn,
		envelope.ReasonFamilyReject,
		envelope.ReasonFamilyReverse,
		envelope.ReasonFamilyReopen,
		envelope.ReasonFamilyWriteOff,
		envelope.ReasonFamilyTerminate,
		envelope.ReasonFamilyOverride,
		envelope.ReasonFamilyExternal,
	}

	for _, fam := range families {
		if !envelope.IsValidReasonFamily(fam) {
			t.Errorf("%q should be valid", fam)
		}
	}

	if envelope.IsValidReasonFamily("INVALID") {
		t.Errorf("INVALID should not be valid")
	}
}

func TestReasonCode_Constants(t *testing.T) {
	codes := map[envelope.ReasonFamily][]envelope.ReasonCode{
		envelope.ReasonFamilyCancel: {
			envelope.ReasonCancelCustomerRequest,
			envelope.ReasonCancelDuplicate,
			envelope.ReasonCancelCommercialWithdrawal,
			envelope.ReasonCancelPreIssueError,
		},
		envelope.ReasonFamilyReturn: {
			envelope.ReasonReturnMissingEvidence,
			envelope.ReasonReturnDataCorrectionReq,
			envelope.ReasonReturnPolicyException,
			envelope.ReasonReturnReviewNotes,
		},
		envelope.ReasonFamilyReject: {
			envelope.ReasonRejectAuthorityDenied,
			envelope.ReasonRejectControlFailure,
			envelope.ReasonRejectPolicyNotMet,
			envelope.ReasonRejectCounterpartyReject,
		},
		envelope.ReasonFamilyReverse: {
			envelope.ReasonReversePostingError,
			envelope.ReasonReverseWrongPeriod,
			envelope.ReasonReverseWrongAccount,
			envelope.ReasonReverseDuplicatePost,
		},
		envelope.ReasonFamilyReopen: {
			envelope.ReasonReopenPostCloseError,
			envelope.ReasonReopenRegulatoryAdjustment,
			envelope.ReasonReopenAuditAdjustment,
			envelope.ReasonReopenAuthorizedCorrection,
		},
		envelope.ReasonFamilyWriteOff: {
			envelope.ReasonWriteOffImmaterialBalance,
			envelope.ReasonWriteOffBadDebtApproved,
			envelope.ReasonWriteOffRounding,
			envelope.ReasonWriteOffStatutoryAdjust,
		},
		envelope.ReasonFamilyTerminate: {
			envelope.ReasonTerminateConvenience,
			envelope.ReasonTerminateBreach,
			envelope.ReasonTerminateRegulatory,
			envelope.ReasonTerminateMutualAgreement,
			envelope.ReasonTerminateEndOfService,
		},
		envelope.ReasonFamilyOverride: {
			envelope.ReasonOverrideControlOwner,
			envelope.ReasonOverrideEmergencyOp,
			envelope.ReasonOverrideAuthorizedExc,
		},
		envelope.ReasonFamilyExternal: {
			envelope.ReasonExternalProviderRejected,
			envelope.ReasonExternalAuthorityRejected,
			envelope.ReasonExternalReturned,
			envelope.ReasonExternalNetworkFailure,
		},
	}

	for fam, codes := range codes {
		for _, code := range codes {
			if !envelope.IsValidReasonCode(fam, code) {
				t.Errorf("%q should be valid for %q", code, fam)
			}
		}
	}
}

func TestParseReason_CompositeFormat(t *testing.T) {
	tests := []struct {
		input    string
		wantFam  envelope.ReasonFamily
		wantCode envelope.ReasonCode
		wantErr  bool
	}{
		{"CANCEL:CUSTOMER_REQUEST", envelope.ReasonFamilyCancel, envelope.ReasonCancelCustomerRequest, false},
		{"CANCEL/CUSTOMER_REQUEST", envelope.ReasonFamilyCancel, envelope.ReasonCancelCustomerRequest, false},
		{"REJECT:POLICY_NOT_MET", envelope.ReasonFamilyReject, envelope.ReasonRejectPolicyNotMet, false},
		{"OVERRIDE:CONTROL_OWNER_OVERRIDE", envelope.ReasonFamilyOverride, envelope.ReasonOverrideControlOwner, false},
		{"INVALID:CODE", "", "", true},
		{"CANCEL:INVALID_CODE", "", "", true},
	}

	for _, tt := range tests {
		fam, code, err := envelope.ParseReason(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseReason(%q) expected error, got nil", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseReason(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if fam != tt.wantFam || code != tt.wantCode {
			t.Errorf("ParseReason(%q) = (%q, %q), want (%q, %q)", tt.input, fam, code, tt.wantFam, tt.wantCode)
		}
	}
}

func TestParseReason_PrefixedFormat(t *testing.T) {
	tests := []struct {
		input    string
		wantFam  envelope.ReasonFamily
		wantCode envelope.ReasonCode
	}{
		{"CANCEL_CUSTOMER_REQUEST", envelope.ReasonFamilyCancel, envelope.ReasonCancelCustomerRequest},
		{"REJECT_POLICY_NOT_MET", envelope.ReasonFamilyReject, envelope.ReasonRejectPolicyNotMet},
		{"OVERRIDE_CONTROL_OWNER_OVERRIDE", envelope.ReasonFamilyOverride, envelope.ReasonOverrideControlOwner},
	}

	for _, tt := range tests {
		fam, code, err := envelope.ParseReason(tt.input)
		if err != nil {
			t.Errorf("ParseReason(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if fam != tt.wantFam || code != tt.wantCode {
			t.Errorf("ParseReason(%q) = (%q, %q), want (%q, %q)", tt.input, fam, code, tt.wantFam, tt.wantCode)
		}
	}
}

func TestParseReason_BareCode(t *testing.T) {
	tests := []struct {
		input    string
		wantFam  envelope.ReasonFamily
		wantCode envelope.ReasonCode
	}{
		{"CUSTOMER_REQUEST", envelope.ReasonFamilyCancel, envelope.ReasonCancelCustomerRequest},
		{"POLICY_NOT_MET", envelope.ReasonFamilyReject, envelope.ReasonRejectPolicyNotMet},
	}

	for _, tt := range tests {
		fam, code, err := envelope.ParseReason(tt.input)
		if err != nil {
			t.Errorf("ParseReason(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if fam != tt.wantFam || code != tt.wantCode {
			t.Errorf("ParseReason(%q) = (%q, %q), want (%q, %q)", tt.input, fam, code, tt.wantFam, tt.wantCode)
		}
	}
}

func TestParseReason_EmptyString(t *testing.T) {
	_, _, err := envelope.ParseReason("")
	if err == nil {
		t.Errorf("ParseReason empty string should error")
	}
}

func TestParseReason_UnknownCode(t *testing.T) {
	_, _, err := envelope.ParseReason("UNKNOWN_CODE")
	if err == nil {
		t.Errorf("ParseReason unknown code should error")
	}
}

func TestLookupReasonFamily(t *testing.T) {
	fam, ok := envelope.LookupReasonFamily(envelope.ReasonCancelCustomerRequest)
	if !ok || fam != envelope.ReasonFamilyCancel {
		t.Errorf("LookupReasonFamily(CUSTOMER_REQUEST) = (%q, %v), want (CANCEL, true)", fam, ok)
	}
}

func TestLookupReasonFamily_Ambiguous(t *testing.T) {
	// This would only fail if we had duplicate codes across families
	// Current registry has unique codes per family
	fam, ok := envelope.LookupReasonFamily(envelope.ReasonRejectPolicyNotMet)
	if !ok || fam != envelope.ReasonFamilyReject {
		t.Errorf("LookupReasonFamily failed")
	}
}

func TestExceptionClass_Retryable(t *testing.T) {
	if !envelope.ExceptionClassTechnicalTransient.Retryable() {
		t.Errorf("TECHNICAL_TRANSIENT should be retryable")
	}

	nonRetryable := []envelope.ExceptionClass{
		envelope.ExceptionClassValidation,
		envelope.ExceptionClassBusinessRule,
		envelope.ExceptionClassAuthorization,
		envelope.ExceptionClassConcurrency,
		envelope.ExceptionClassExternalRejection,
		envelope.ExceptionClassTechnicalPermanent,
		envelope.ExceptionClassControlException,
	}

	for _, ec := range nonRetryable {
		if ec.Retryable() {
			t.Errorf("%q should not be retryable", ec)
		}
	}
}
