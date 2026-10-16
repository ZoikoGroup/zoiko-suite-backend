package domain

// Stable error and reason codes defined in ZS-SVC-W-001 §32
const (
	ErrCodePurposeNotRegistered          = "PRV-001"
	ErrCodeProcessingActivityInactive    = "PRV-002"
	ErrCodePrivacyRoleUnresolved         = "PRV-003"
	ErrCodeJurisdictionUnresolved        = "PRV-004"
	ErrCodePolicyUnavailable             = "PRV-005"
	ErrCodeConsentRequiredMissing        = "PRV-006"
	ErrCodeConsentWithdrawn              = "PRV-007"
	ErrCodeNoticeVersionInvalid          = "PRV-008"
	ErrCodePurposeIncompatible           = "PRV-009"
	ErrCodeDataCategoryRestricted        = "PRV-010"
	ErrCodeMinimizationRequired          = "PRV-011"
	ErrCodeIdentityAssuranceInsufficient = "PRV-012"
	ErrCodeThirdPartyReviewRequired      = "PRV-013"
	ErrCodeRetentionOrHoldBlock          = "PRV-014"
	ErrCodeTransferNotAuthorized         = "PRV-015"
	ErrCodeAssessmentRequired            = "PRV-016"
	ErrCodeProcessorInstructionMissing   = "PRV-017"
	ErrCodeSubprocessorNotApproved       = "PRV-018"
	ErrCodePrivacyContextIndeterminate   = "PRV-019"
	ErrCodeImmutableEvidenceConflict     = "PRV-020"

	// Local extensions for transport/security conditions not in §32
	ErrCodeNotFound             = "NOT_FOUND"
	ErrCodeValidationFailed     = "VALIDATION_FAILED"
	ErrCodeAuthenticationFailed = "AUTHENTICATION_FAILED"
	ErrCodeAuthorizationDenied  = "AUTHORIZATION_DENIED"
	ErrCodeTenantScopeRequired  = "TENANT_SCOPE_REQUIRED"
	ErrCodeTenantScopeMismatch  = "TENANT_SCOPE_MISMATCH"
	ErrCodeIdempotencyRequired  = "IDEMPOTENCY_KEY_REQUIRED"
	ErrCodeCorrelationRequired  = "CORRELATION_ID_REQUIRED"
	ErrCodeStoreUnavailable     = "STORE_UNAVAILABLE"
	ErrCodeUpstreamUnavailable  = "UPSTREAM_UNAVAILABLE"
	ErrCodeInternal             = "INTERNAL_ERROR"
)
