package service

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/decision-support-svc/internal/domain"
)

// Field length limits. Every limit matches (or is below) its column width,
// so an over-long value is a 400 here, never a database error.
const (
	maxSubjectTypeLength      = 100
	maxSubjectReferenceLength = 255
	maxActionTypeLength       = 100
	maxCorrelationIDLength    = 255
	maxEvidenceRefs           = 50
	maxEvidenceRefLength      = 512
	maxRationaleLength        = 4000
	maxModelIDLength          = 255
	maxModelVersionLength     = 64
	maxReasonLength           = 2000
	maxReferenceLength        = 255

	defaultReviewWindow = 72 * time.Hour
	maxReviewWindow     = 90 * 24 * time.Hour
)

// validatedCreate is a CreateRecommendationRequest that passed validation,
// with defaults applied.
type validatedCreate struct {
	legalEntityID    string
	subjectType      string
	subjectReference string
	actionType       string
	correlationID    string
	oversightClass   domain.OversightClass
	expiresAt        time.Time
	evidenceRefs     []string
	proposal         *domain.ExternalProposal
}

// validateCreate checks a create request field by field, in the order a
// caller reads them, and returns the first problem found.
func validateCreate(caller domain.CallerContext, req domain.CreateRecommendationRequest, now time.Time) (validatedCreate, error) {
	// Tenant: the body may only agree with X-Tenant-Id.
	if req.TenantID != "" && req.TenantID != caller.TenantID {
		return validatedCreate{}, domain.ErrTenantMismatch
	}

	legalEntityID := strings.TrimSpace(req.LegalEntityID)
	if legalEntityID == "" {
		return validatedCreate{}, domain.NewFieldError("legal_entity_id", "is required")
	}
	if !isUUID(legalEntityID) {
		return validatedCreate{}, domain.NewFieldError("legal_entity_id", "must be a UUID")
	}
	if caller.LegalEntityID != "" && caller.LegalEntityID != legalEntityID {
		return validatedCreate{}, domain.NewFieldError("legal_entity_id", "does not match the X-Legal-Entity-Id header")
	}

	subjectType, err := requiredText("subject_type", req.SubjectType, maxSubjectTypeLength)
	if err != nil {
		return validatedCreate{}, err
	}
	subjectReference, err := requiredText("subject_reference", req.SubjectReference, maxSubjectReferenceLength)
	if err != nil {
		return validatedCreate{}, err
	}
	actionType, err := requiredText("action_type", req.ActionType, maxActionTypeLength)
	if err != nil {
		return validatedCreate{}, err
	}

	correlationID := strings.TrimSpace(req.CorrelationID)
	if correlationID == "" {
		correlationID = caller.CorrelationID
	}
	if len(correlationID) > maxCorrelationIDLength {
		return validatedCreate{}, domain.NewFieldError("correlation_id", "must be at most 255 characters")
	}

	oversightClass, known := domain.ParseOversightClass(req.OversightClass)
	if !known {
		return validatedCreate{}, domain.NewFieldError("oversight_class", "must be one of O1, O2, O3")
	}
	if oversightClass == domain.OversightAIProhibited {
		return validatedCreate{}, domain.ErrOversightProhibited
	}

	expiresAt, err := resolveExpiry(req.ExpiresAt, now)
	if err != nil {
		return validatedCreate{}, err
	}

	evidenceRefs, err := validateEvidenceRefs(req.EvidenceRefs)
	if err != nil {
		return validatedCreate{}, err
	}

	if req.Proposal != nil {
		if err := validateProposal(*req.Proposal); err != nil {
			return validatedCreate{}, err
		}
	}

	return validatedCreate{
		legalEntityID:    legalEntityID,
		subjectType:      subjectType,
		subjectReference: subjectReference,
		actionType:       actionType,
		correlationID:    correlationID,
		oversightClass:   oversightClass,
		expiresAt:        expiresAt,
		evidenceRefs:     evidenceRefs,
		proposal:         req.Proposal,
	}, nil
}

// resolveExpiry applies the default review window and bounds a supplied one.
func resolveExpiry(requested *time.Time, now time.Time) (time.Time, error) {
	if requested == nil {
		return now.Add(defaultReviewWindow), nil
	}
	expiresAt := requested.UTC()
	if !expiresAt.After(now) {
		return time.Time{}, domain.NewFieldError("expires_at", "must be in the future")
	}
	if expiresAt.After(now.Add(maxReviewWindow)) {
		return time.Time{}, domain.NewFieldError("expires_at", "must be at most 90 days ahead")
	}
	return expiresAt, nil
}

func validateEvidenceRefs(refs []string) ([]string, error) {
	if len(refs) > maxEvidenceRefs {
		return nil, domain.NewFieldError("evidence_refs", "must contain at most 50 references")
	}
	cleaned := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil, domain.NewFieldError("evidence_refs", "must not contain empty references")
		}
		if len(ref) > maxEvidenceRefLength {
			return nil, domain.NewFieldError("evidence_refs", "each reference must be at most 512 characters")
		}
		cleaned = append(cleaned, ref)
	}
	return cleaned, nil
}

// validateProposal checks an external model's recommendation. Model and
// version are mandatory: "Every material AI output retains model/provider
// version" (Data/Search/Analytics/AI spec §2).
func validateProposal(p domain.ExternalProposal) error {
	if !p.RecommendedAction.IsValidForExternalProposal() {
		return domain.NewFieldError("proposal.recommended_action", "must be one of APPROVE, REJECT, ESCALATE")
	}
	if p.ConfidenceScore == nil {
		return domain.NewFieldError("proposal.confidence_score", "is required")
	}
	if *p.ConfidenceScore < 0 || *p.ConfidenceScore > 1 {
		return domain.NewFieldError("proposal.confidence_score", "must be between 0 and 1")
	}
	if _, err := requiredText("proposal.rationale", p.Rationale, maxRationaleLength); err != nil {
		return err
	}
	if _, err := requiredText("proposal.model_id", p.ModelID, maxModelIDLength); err != nil {
		return err
	}
	if _, err := requiredText("proposal.model_version", p.ModelVersion, maxModelVersionLength); err != nil {
		return err
	}
	if len(p.PromptVersion) > maxModelVersionLength {
		return domain.NewFieldError("proposal.prompt_version", "must be at most 64 characters")
	}
	return nil
}

// validateDecision checks the body of accept / reject / expire / supersede.
// Rules that depend on the stored recommendation (O3 approval reference,
// early-expiry reason) are checked later, in Decide.
func validateDecision(cmd domain.DecisionCommand) error {
	req := cmd.Request

	if len(req.Reason) > maxReasonLength {
		return domain.NewFieldError("reason", "must be at most 2000 characters")
	}
	if len(req.DownstreamActionRef) > maxReferenceLength {
		return domain.NewFieldError("downstream_action_ref", "must be at most 255 characters")
	}
	if len(req.ApprovalReference) > maxReferenceLength {
		return domain.NewFieldError("approval_reference", "must be at most 255 characters")
	}

	isAccept := cmd.TargetStatus == domain.StatusAccepted
	if !isAccept && req.DownstreamActionRef != "" {
		return domain.NewFieldError("downstream_action_ref", "is only allowed when accepting")
	}
	if !isAccept && req.ApprovalReference != "" {
		return domain.NewFieldError("approval_reference", "is only allowed when accepting")
	}

	isSupersede := cmd.TargetStatus == domain.StatusSuperseded
	if !isSupersede && req.ReplacementRecommendationID != "" {
		return domain.NewFieldError("replacement_recommendation_id", "is only allowed when superseding")
	}

	switch cmd.TargetStatus {
	case domain.StatusRejected:
		// "REJECTED -> no downstream authority" — and the reviewer must say why.
		if strings.TrimSpace(req.Reason) == "" {
			return domain.NewFieldError("reason", "is required to reject a recommendation")
		}
	case domain.StatusSuperseded:
		replacementID := strings.TrimSpace(req.ReplacementRecommendationID)
		if replacementID == "" {
			return domain.NewFieldError("replacement_recommendation_id", "is required to supersede a recommendation")
		}
		if !isUUID(replacementID) {
			return domain.NewFieldError("replacement_recommendation_id", "must be a UUID")
		}
	}
	return nil
}

func requiredText(field, value string, maxLength int) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", domain.NewFieldError(field, "is required")
	}
	if len(trimmed) > maxLength {
		return "", domain.NewFieldError(field, "is too long")
	}
	return trimmed, nil
}

func isUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil && len(value) == 36
}
