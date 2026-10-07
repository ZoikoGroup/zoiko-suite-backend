// Package envelope defines the canonical request and transition contracts for ZoikoSuite.
//
// This file implements the Governed Reason-Code Registry and Exception Taxonomy
// per ZS-STATE-001 (Business Object State Machine, Workflow & Approval Catalogue)
// §16 and Appendix B.
//
// Doctrine (ZS-STATE-001 §16):
// "Every business rejection, cancellation, reversal, reopen, write-off, override,
// return and privileged intervention uses a governed reason code. Free text may
// supplement the code but never replace it."
package envelope

import (
	"fmt"
	"strings"
)

// ReasonFamily represents a governed category of state transition reasons
// defined in ZS-STATE-001 Appendix B.
type ReasonFamily string

const (
	ReasonFamilyCancel    ReasonFamily = "CANCEL"
	ReasonFamilyReturn    ReasonFamily = "RETURN"
	ReasonFamilyReject    ReasonFamily = "REJECT"
	ReasonFamilyReverse   ReasonFamily = "REVERSE"
	ReasonFamilyReopen    ReasonFamily = "REOPEN"
	ReasonFamilyWriteOff  ReasonFamily = "WRITE_OFF"
	ReasonFamilyTerminate ReasonFamily = "TERMINATE"
	ReasonFamilyOverride  ReasonFamily = "OVERRIDE"
	ReasonFamilyExternal  ReasonFamily = "EXTERNAL"
)

// ReasonCode represents a specific governed transition reason within a ReasonFamily.
type ReasonCode string

// Canonical Reason Codes defined in ZS-STATE-001 Appendix B.
const (
	// Family: CANCEL
	ReasonCancelCustomerRequest      ReasonCode = "CUSTOMER_REQUEST"
	ReasonCancelDuplicate            ReasonCode = "DUPLICATE"
	ReasonCancelCommercialWithdrawal ReasonCode = "COMMERCIAL_WITHDRAWAL"
	ReasonCancelPreIssueError        ReasonCode = "PRE_ISSUE_ERROR"

	// Family: RETURN
	ReasonReturnMissingEvidence       ReasonCode = "MISSING_EVIDENCE"
	ReasonReturnDataCorrectionReq     ReasonCode = "DATA_CORRECTION_REQUIRED"
	ReasonReturnPolicyException       ReasonCode = "POLICY_EXCEPTION"
	ReasonReturnReviewNotes           ReasonCode = "REVIEW_NOTES"

	// Family: REJECT
	ReasonRejectAuthorityDenied     ReasonCode = "AUTHORITY_DENIED"
	ReasonRejectControlFailure      ReasonCode = "CONTROL_FAILURE"
	ReasonRejectPolicyNotMet        ReasonCode = "POLICY_NOT_MET"
	ReasonRejectCounterpartyReject  ReasonCode = "COUNTERPARTY_REJECTED"

	// Family: REVERSE
	ReasonReversePostingError   ReasonCode = "POSTING_ERROR"
	ReasonReverseWrongPeriod    ReasonCode = "WRONG_PERIOD"
	ReasonReverseWrongAccount   ReasonCode = "WRONG_ACCOUNT"
	ReasonReverseDuplicatePost  ReasonCode = "DUPLICATE_POSTING"

	// Family: REOPEN
	ReasonReopenPostCloseError       ReasonCode = "POST_CLOSE_ERROR"
	ReasonReopenRegulatoryAdjustment ReasonCode = "REGULATORY_ADJUSTMENT"
	ReasonReopenAuditAdjustment      ReasonCode = "AUDIT_ADJUSTMENT"
	ReasonReopenAuthorizedCorrection ReasonCode = "AUTHORIZED_CORRECTION"

	// Family: WRITE_OFF
	ReasonWriteOffImmaterialBalance ReasonCode = "IMMATERIAL_BALANCE"
	ReasonWriteOffBadDebtApproved   ReasonCode = "BAD_DEBT_APPROVED"
	ReasonWriteOffRounding          ReasonCode = "ROUNDING"
	ReasonWriteOffStatutoryAdjust   ReasonCode = "STATUTORY_ADJUSTMENT"

	// Family: TERMINATE
	ReasonTerminateConvenience     ReasonCode = "CONVENIENCE"
	ReasonTerminateBreach          ReasonCode = "BREACH"
	ReasonTerminateRegulatory      ReasonCode = "REGULATORY"
	ReasonTerminateMutualAgreement ReasonCode = "MUTUAL_AGREEMENT"
	ReasonTerminateEndOfService    ReasonCode = "END_OF_SERVICE"

	// Family: OVERRIDE
	ReasonOverrideControlOwner   ReasonCode = "CONTROL_OWNER_OVERRIDE"
	ReasonOverrideEmergencyOp    ReasonCode = "EMERGENCY_OPERATION"
	ReasonOverrideAuthorizedExc  ReasonCode = "AUTHORIZED_EXCEPTION"

	// Family: EXTERNAL
	ReasonExternalProviderRejected  ReasonCode = "PROVIDER_REJECTED"
	ReasonExternalAuthorityRejected ReasonCode = "AUTHORITY_REJECTED"
	ReasonExternalReturned          ReasonCode = "RETURNED"
	ReasonExternalNetworkFailure    ReasonCode = "NETWORK_FAILURE"
)

// canonicalRegistry maps each ReasonFamily to its governed ReasonCodes.
var canonicalRegistry = map[ReasonFamily]map[ReasonCode]bool{
	ReasonFamilyCancel: {
		ReasonCancelCustomerRequest:      true,
		ReasonCancelDuplicate:            true,
		ReasonCancelCommercialWithdrawal: true,
		ReasonCancelPreIssueError:        true,
	},
	ReasonFamilyReturn: {
		ReasonReturnMissingEvidence:   true,
		ReasonReturnDataCorrectionReq: true,
		ReasonReturnPolicyException:   true,
		ReasonReturnReviewNotes:       true,
	},
	ReasonFamilyReject: {
		ReasonRejectAuthorityDenied:    true,
		ReasonRejectControlFailure:     true,
		ReasonRejectPolicyNotMet:       true,
		ReasonRejectCounterpartyReject: true,
	},
	ReasonFamilyReverse: {
		ReasonReversePostingError:  true,
		ReasonReverseWrongPeriod:   true,
		ReasonReverseWrongAccount:  true,
		ReasonReverseDuplicatePost: true,
	},
	ReasonFamilyReopen: {
		ReasonReopenPostCloseError:       true,
		ReasonReopenRegulatoryAdjustment: true,
		ReasonReopenAuditAdjustment:      true,
		ReasonReopenAuthorizedCorrection: true,
	},
	ReasonFamilyWriteOff: {
		ReasonWriteOffImmaterialBalance: true,
		ReasonWriteOffBadDebtApproved:   true,
		ReasonWriteOffRounding:          true,
		ReasonWriteOffStatutoryAdjust:   true,
	},
	ReasonFamilyTerminate: {
		ReasonTerminateConvenience:     true,
		ReasonTerminateBreach:          true,
		ReasonTerminateRegulatory:      true,
		ReasonTerminateMutualAgreement: true,
		ReasonTerminateEndOfService:    true,
	},
	ReasonFamilyOverride: {
		ReasonOverrideControlOwner:  true,
		ReasonOverrideEmergencyOp:   true,
		ReasonOverrideAuthorizedExc: true,
	},
	ReasonFamilyExternal: {
		ReasonExternalProviderRejected:  true,
		ReasonExternalAuthorityRejected: true,
		ReasonExternalReturned:          true,
		ReasonExternalNetworkFailure:    true,
	},
}

// IsValidReasonFamily reports whether f is one of the 9 governed reason families.
func IsValidReasonFamily(f ReasonFamily) bool {
	_, ok := canonicalRegistry[f]
	return ok
}

// IsValidReasonCode reports whether code is a recognized governed reason code
// for the given family.
func IsValidReasonCode(f ReasonFamily, code ReasonCode) bool {
	codes, ok := canonicalRegistry[f]
	if !ok {
		return false
	}
	return codes[code]
}

// LookupReasonFamily finds which ReasonFamily a bare reason code belongs to.
// Returns the family and true if uniquely matched, or empty and false otherwise.
func LookupReasonFamily(code ReasonCode) (ReasonFamily, bool) {
	var found ReasonFamily
	matchCount := 0
	for fam, codes := range canonicalRegistry {
		if codes[code] {
			found = fam
			matchCount++
		}
	}
	if matchCount == 1 {
		return found, true
	}
	return "", false
}

// ParseReason parses and validates a raw reason string.
//
// It accepts either:
//   - A composite string formatted as "FAMILY:CODE" (e.g. "CANCEL:CUSTOMER_REQUEST" or "CANCEL/CUSTOMER_REQUEST")
//   - A prefixed string formatted as "FAMILY_CODE" (e.g. "CANCEL_CUSTOMER_REQUEST")
//   - A bare code string (e.g. "CUSTOMER_REQUEST") if the code uniquely identifies a family.
//
// Returns the resolved ReasonFamily, ReasonCode, and nil on success.
// Returns an error if the format is invalid or the code is not in the governed registry.
func ParseReason(raw string) (ReasonFamily, ReasonCode, error) {
	s := strings.TrimSpace(strings.ToUpper(raw))
	if s == "" {
		return "", "", fmt.Errorf("reason code is required and cannot be empty")
	}

	// Case 1: "FAMILY:CODE" or "FAMILY/CODE"
	if sepIdx := strings.IndexAny(s, ":/"); sepIdx != -1 {
		famStr := s[:sepIdx]
		codeStr := s[sepIdx+1:]
		fam := ReasonFamily(famStr)
		code := ReasonCode(codeStr)
		if !IsValidReasonCode(fam, code) {
			return "", "", fmt.Errorf("invalid reason code %q for family %q", codeStr, famStr)
		}
		return fam, code, nil
	}

	// Case 2: Direct match against family prefix (e.g. "CANCEL_CUSTOMER_REQUEST")
	for fam, codes := range canonicalRegistry {
		prefix := string(fam) + "_"
		if strings.HasPrefix(s, prefix) {
			codeStr := strings.TrimPrefix(s, prefix)
			code := ReasonCode(codeStr)
			if codes[code] {
				return fam, code, nil
			}
		}
	}

	// Case 3: Bare code matching
	code := ReasonCode(s)
	if fam, ok := LookupReasonFamily(code); ok {
		return fam, code, nil
	}

	return "", "", fmt.Errorf("unknown or ungoverned reason code %q", raw)
}

// ExceptionClass represents the blocking or retry taxonomy defined in ZS-STATE-001 §16.
type ExceptionClass string

const (
	ExceptionClassValidation         ExceptionClass = "VALIDATION"
	ExceptionClassBusinessRule       ExceptionClass = "BUSINESS_RULE"
	ExceptionClassAuthorization      ExceptionClass = "AUTHORIZATION"
	ExceptionClassConcurrency        ExceptionClass = "CONCURRENCY"
	ExceptionClassExternalRejection  ExceptionClass = "EXTERNAL_REJECTION"
	ExceptionClassTechnicalTransient ExceptionClass = "TECHNICAL_TRANSIENT"
	ExceptionClassTechnicalPermanent ExceptionClass = "TECHNICAL_PERMANENT"
	ExceptionClassControlException   ExceptionClass = "CONTROL_EXCEPTION"
)

// Retryable reports whether an exception class permits automated idempotent retry.
// Per ZS-STATE-001 §16, only TECHNICAL_TRANSIENT permits bounded automated retry.
func (c ExceptionClass) Retryable() bool {
	return c == ExceptionClassTechnicalTransient
}
