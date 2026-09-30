package privacy

import (
	"errors"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrDirectDeleteBlocked is returned when a direct DELETE is attempted against held or legally obligated data (GOV-16, DG-040, NP-17).
	ErrDirectDeleteBlocked = errors.New("direct DELETE bypasses legal hold and statutory obligations; privacy requests must be resolved through governed policy workflow (blocked per NP-17/DG-040)")
)

// Resolver resolves privacy erasure and restriction requests against legal holds and retention obligations.
type Resolver struct{}

func NewResolver() *Resolver {
	return &Resolver{}
}

// FieldPolicyContext defines the retention/hold status of a candidate field or data attribute.
type FieldPolicyContext struct {
	FieldName            string
	IsUnderLegalHold     bool
	HoldRef              string
	HasStatutoryBasis    bool
	LegalRegulatoryBasis string
}

// Resolve evaluates privacy erasure against active legal holds and statutory retention rules (NP-17, NP-18).
func (r *Resolver) Resolve(
	request *PrivacyDispositionRequest,
	targetContext []FieldPolicyContext,
	resolvedBy string,
	resolvedAt time.Time,
) (*PrivacyResolution, error) {
	if request == nil {
		return nil, errors.New("privacy request cannot be nil")
	}
	if len(targetContext) == 0 {
		return nil, errors.New("target context cannot be empty")
	}

	resolutionID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	// 1. Check for Active Legal Holds (NP-17, DG-040)
	var activeHoldRef string
	for _, ctx := range targetContext {
		if ctx.IsUnderLegalHold {
			activeHoldRef = ctx.HoldRef
			break
		}
	}

	if activeHoldRef != "" {
		// Held data resolves to DEFER; direct deletion is strictly blocked (NP-17)
		reviewDate := resolvedAt.AddDate(0, 3, 0) // 90-day review cycle
		return &PrivacyResolution{
			ResolutionID:         resolutionID,
			TenantID:             request.TenantID,
			RequestID:            request.RequestID,
			Outcome:              OutcomeDefer,
			LegalRegulatoryBasis: "PRESERVATION_LEGAL_HOLD_PREEMPTION",
			HoldBlockRef:         &activeHoldRef,
			ReviewDate:           &reviewDate,
			ResolvedAt:           resolvedAt,
			ResolvedBy:           resolvedBy,
		}, nil
	}

	// 2. Check for Mixed Retained and Non-retained Fields (NP-18, DG-052)
	hasRetained := false
	hasNonRetained := false
	var fieldActions []FieldResolutionDetail

	for _, ctx := range targetContext {
		if ctx.HasStatutoryBasis {
			hasRetained = true
			fieldActions = append(fieldActions, FieldResolutionDetail{
				FieldName:            ctx.FieldName,
				Action:               "RETAIN",
				LegalRegulatoryBasis: ctx.LegalRegulatoryBasis,
			})
		} else {
			hasNonRetained = true
			fieldActions = append(fieldActions, FieldResolutionDetail{
				FieldName: ctx.FieldName,
				Action:    "ERASE",
			})
		}
	}

	if hasRetained && hasNonRetained {
		// Mixed case resolves to PARTIAL (NP-18)
		return &PrivacyResolution{
			ResolutionID:         resolutionID,
			TenantID:             request.TenantID,
			RequestID:            request.RequestID,
			Outcome:              OutcomePartial,
			LegalRegulatoryBasis: "STATUTORY_RETENTION_OVERRIDE_FOR_MATERIAL_ELEMENTS",
			FieldActions:         fieldActions,
			ResolvedAt:           resolvedAt,
			ResolvedBy:           resolvedBy,
		}, nil
	}

	if hasRetained && !hasNonRetained {
		// Purely statutory record
		return &PrivacyResolution{
			ResolutionID:         resolutionID,
			TenantID:             request.TenantID,
			RequestID:            request.RequestID,
			Outcome:              OutcomeDenyWithBasis,
			LegalRegulatoryBasis: "MANDATORY_STATUTORY_RETENTION",
			FieldActions:         fieldActions,
			ResolvedAt:           resolvedAt,
			ResolvedBy:           resolvedBy,
		}, nil
	}

	// Purely non-retained personal data
	return &PrivacyResolution{
		ResolutionID:         resolutionID,
		TenantID:             request.TenantID,
		RequestID:            request.RequestID,
		Outcome:              OutcomeErase,
		LegalRegulatoryBasis: "DATA_SUBJECT_ERASURE_CONSENT_REVOCATION",
		FieldActions:         fieldActions,
		ResolvedAt:           resolvedAt,
		ResolvedBy:           resolvedBy,
	}, nil
}
