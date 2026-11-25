// WFC-05 consolidation — data backfill (Doc 04 §2.1 single-owner doctrine,
// tracker row 78). These two methods exist for exactly one caller,
// cmd/migrate-duplicates: reconstructing a historical row from
// obligation-tracking-svc or filing-tracker-svc verbatim, rather than
// replaying it through today's live-command validation (CreateObligation,
// ValidateExtractedObligation, TransitionExtended), which would refuse
// perfectly real history that predates an evidence rule those commands now
// enforce.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/obligations-svc/internal/domain"
)

// BackfillObligation inserts one historical obligation row exactly as
// supplied. Idempotent on obligation_id: a repeat call with the same
// (caller-supplied, deterministic) id is a no-op, returning the row already
// there rather than erroring or duplicating it.
func (s *PgStore) BackfillObligation(ctx context.Context, p domain.BackfillObligationParams) (*domain.Obligation, bool, error) {
	if p.LegalEntityID == "" || p.JurisdictionID == "" {
		return nil, false, domain.ErrInvalidField
	}
	if p.ObligationSourceType == "" || p.ObligationSourceID == "" || p.ObligationCode == "" ||
		p.ObligationType == "" || p.SeverityLevel == "" || p.ResponsibleFunction == "" || p.SourceReference == "" {
		return nil, false, domain.ErrMissingField
	}
	if !domain.AnyObligationStatus(p.ObligationStatus) {
		return nil, false, domain.ErrInvalidField
	}
	version := p.Version
	if version < 1 {
		version = 1
	}

	// Explicit INSERT column list (tenant_id first — obligationColumns is the
	// SELECT/RETURNING projection and deliberately omits it, same as
	// CreateObligation's own INSERT above).
	const insert = `
		INSERT INTO obligations (
			obligation_id, tenant_id, legal_entity_id, jurisdiction_id, workflow_id,
			obligation_source_type, obligation_source_id, obligation_code, obligation_type, obligation_status,
			due_date, severity_level, responsible_function, source_reference,
			created_at, created_by_principal_id, updated_at, closed_at, version,
			obligation_definition_id, definition_version,
			lifecycle_model, clock_class, extracted_by_ai,
			validated_at, validated_by,
			source_clause_id, trigger_description, calculation_method,
			scheduled_at, scheduled_by,
			due_soon_marked_at, due_marked_at, due_marked_by,
			in_progress_at, in_progress_by,
			fulfilled_at, fulfilled_by, fulfillment_evidence_ref,
			waived_at, waived_by, waiver_authority_reference, waiver_reason_code,
			not_applicable_at, not_applicable_by, not_applicable_reason,
			breached_at, breached_by, breach_note,
			disputed_at, disputed_by, dispute_reason,
			superseded_by_obligation_id,
			effective_from, effective_to
		) VALUES (
			$1,$2,$3,$4,$5,
			$6,$7,$8,$9,$10,
			$11,$12,$13,$14,
			$15,$16,$17,$18,$19,
			$20,$21,
			$22,$23,$24,
			$25,$26,
			$27,$28,$29,
			$30,$31,
			$32,$33,$34,
			$35,$36,
			$37,$38,$39,
			$40,$41,$42,$43,
			$44,$45,$46,
			$47,$48,$49,
			$50,$51,$52,
			$53,
			$54,$55
		)
		ON CONFLICT (obligation_id) DO NOTHING
		RETURNING ` + obligationColumns + `;`

	const lookup = `SELECT ` + obligationColumns + ` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2;`

	var out *domain.Obligation
	created := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, insert,
			p.ObligationID, tenantID, p.LegalEntityID, p.JurisdictionID, p.WorkflowID,
			p.ObligationSourceType, p.ObligationSourceID, p.ObligationCode, p.ObligationType, p.ObligationStatus,
			p.DueDate, p.SeverityLevel, p.ResponsibleFunction, p.SourceReference,
			p.CreatedAt, p.CreatedByPrincipalID, p.UpdatedAt, p.ClosedAt, version,
			p.ObligationDefinitionID, p.DefinitionVersion,
			p.LifecycleModel, p.ClockClass, p.ExtractedByAI,
			p.ValidatedAt, p.ValidatedBy,
			p.SourceClauseID, p.TriggerDescription, p.CalculationMethod,
			p.ScheduledAt, p.ScheduledBy,
			p.DueSoonMarkedAt, p.DueMarkedAt, p.DueMarkedBy,
			p.InProgressAt, p.InProgressBy,
			p.FulfilledAt, p.FulfilledBy, p.FulfillmentEvidenceRef,
			p.WaivedAt, p.WaivedBy, p.WaiverAuthorityReference, p.WaiverReasonCode,
			p.NotApplicableAt, p.NotApplicableBy, p.NotApplicableReason,
			p.BreachedAt, p.BreachedBy, p.BreachNote,
			p.DisputedAt, p.DisputedBy, p.DisputeReason,
			p.SupersededByObligationID,
			p.EffectiveFrom, p.EffectiveTo,
		)
		scanned, scanErr := scanObligation(row)
		switch {
		case scanErr == nil:
			out = scanned
			created = true
		case errors.Is(scanErr, pgx.ErrNoRows):
			existing, lookupErr := scanObligation(tx.QueryRow(ctx, lookup, p.ObligationID, tenantID))
			if lookupErr != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, lookupErr)
			}
			out = existing
		default:
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		if !created {
			return nil
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "obligation.migrated", p.CorrelationID,
			out.LegalEntityID, out.JurisdictionID, p.CreatedByPrincipalID, map[string]any{
				"obligation_id":          out.ObligationID,
				"obligation_status":      out.ObligationStatus,
				"obligation_source_type": out.ObligationSourceType,
				"obligation_source_id":   out.ObligationSourceID,
			})
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// BackfillFilingRequirement inserts one historical filing requirement row
// under an already-backfilled parent obligation. Idempotent on
// filing_requirement_id, same discipline as BackfillObligation.
func (s *PgStore) BackfillFilingRequirement(ctx context.Context, p domain.BackfillFilingRequirementParams) (*domain.FilingRequirement, bool, error) {
	if p.ObligationID == "" || p.FilingType == "" || p.FilingAuthority == "" || p.SubmissionChannel == "" {
		return nil, false, domain.ErrMissingField
	}
	if _, known := domain.FilingStatuses[p.FilingStatus]; !known {
		return nil, false, domain.ErrInvalidField
	}
	rowVersion := p.RowVersion
	if rowVersion < 1 {
		rowVersion = 1
	}

	const insert = `
		INSERT INTO filing_requirements (
			filing_requirement_id, tenant_id, obligation_id, filing_type, filing_authority,
			submission_channel, filing_status, row_version,
			jurisdiction_id, period_key, due_date,
			submission_reference, submitted_at, submitted_by,
			confirmation_reference, confirmed_at, rejection_reason, notes
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (filing_requirement_id) DO NOTHING
		RETURNING ` + filingRequirementColumns + `;`

	const lookup = `SELECT ` + filingRequirementColumns + ` FROM filing_requirements WHERE filing_requirement_id = $1 AND tenant_id = $2;`

	var out *domain.FilingRequirement
	created := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := scanObligation(tx.QueryRow(ctx,
			`SELECT `+obligationColumns+` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2;`,
			p.ObligationID, tenantID)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
				return domain.ErrObligationNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		row := tx.QueryRow(ctx, insert,
			p.FilingRequirementID, tenantID, p.ObligationID, p.FilingType, p.FilingAuthority,
			p.SubmissionChannel, p.FilingStatus, rowVersion,
			p.JurisdictionID, p.PeriodKey, p.DueDate,
			p.SubmissionReference, p.SubmittedAt, p.SubmittedBy,
			p.ConfirmationReference, p.ConfirmedAt, p.RejectionReason, p.Notes,
		)
		scanned, scanErr := scanFilingRequirement(row)
		switch {
		case scanErr == nil:
			out = scanned
			created = true
		case errors.Is(scanErr, pgx.ErrNoRows):
			existing, lookupErr := scanFilingRequirement(tx.QueryRow(ctx, lookup, p.FilingRequirementID, tenantID))
			if lookupErr != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, lookupErr)
			}
			out = existing
		default:
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		if !created {
			return nil
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "obligation.updated", p.CorrelationID,
			"", "", "migrate-duplicates", map[string]any{
				"obligation_id":         p.ObligationID,
				"changed_fields":        []string{"filing_requirements.filing_status"},
				"filing_requirement_id": out.FilingRequirementID,
				"filing_status":         out.FilingStatus,
				"migrated":              true,
			})
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}
