package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
)

const assetEventColumns = `
	event_id, tenant_id, legal_entity_id, asset_id, component_id, event_type, status,
	source_document_ref, valuation_evidence_ref, amount, currency, effective_date, fiscal_period,
	proceeds_amount, destination_custodian_id, destination_location_id, destination_legal_entity_id,
	debit_account_code, credit_account_code, journal_id, correction_of_event_id,
	created_at, created_by_principal_id, validated_at,
	approved_at, approved_by_principal_id, applied_at, emitted_at,
	reversed_at, reversed_by_principal_id, reversal_reason,
	superseded_at, superseded_by_principal_id, supersession_reason`

func scanAssetEvent(row pgx.Row) (*domain.AssetEvent, error) {
	var e domain.AssetEvent
	if err := row.Scan(
		&e.EventID, &e.TenantID, &e.LegalEntityID, &e.AssetID, &e.ComponentID, &e.EventType, &e.Status,
		&e.SourceDocumentRef, &e.ValuationEvidenceRef, &e.Amount, &e.Currency, &e.EffectiveDate, &e.FiscalPeriod,
		&e.ProceedsAmount, &e.DestinationCustodianID, &e.DestinationLocationID, &e.DestinationLegalEntityID,
		&e.DebitAccountCode, &e.CreditAccountCode, &e.JournalID, &e.CorrectionOfEventID,
		&e.CreatedAt, &e.CreatedByPrincipalID, &e.ValidatedAt,
		&e.ApprovedAt, &e.ApprovedByPrincipalID, &e.AppliedAt, &e.EmittedAt,
		&e.ReversedAt, &e.ReversedByPrincipalID, &e.ReversalReason,
		&e.SupersededAt, &e.SupersededByPrincipalID, &e.SupersessionReason,
	); err != nil {
		return nil, err
	}
	return &e, nil
}

// CreateAssetEvent inserts a new event in DRAFT. The caller (handler layer)
// has already resolved the target asset's legal_entity_id and run every
// type-specific validation (evidence, component ownership, cross-entity
// transfer block) — this method persists what it is given.
func (s *PgStore) CreateAssetEvent(ctx context.Context, e *domain.AssetEvent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO asset_events (
				event_id, tenant_id, legal_entity_id, asset_id, component_id, event_type, status,
				source_document_ref, valuation_evidence_ref, amount, currency, effective_date, fiscal_period,
				proceeds_amount, destination_custodian_id, destination_location_id, destination_legal_entity_id,
				debit_account_code, credit_account_code, correction_of_event_id,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		`, e.EventID, tenantID, e.LegalEntityID, e.AssetID, e.ComponentID, e.EventType, e.Status,
			e.SourceDocumentRef, e.ValuationEvidenceRef, e.Amount, e.Currency, e.EffectiveDate, e.FiscalPeriod,
			e.ProceedsAmount, e.DestinationCustodianID, e.DestinationLocationID, e.DestinationLegalEntityID,
			e.DebitAccountCode, e.CreditAccountCode, e.CorrectionOfEventID,
			e.CreatedAt, e.CreatedByPrincipalID)
		return mapAssetEventPgError(err)
	})
}

func (s *PgStore) GetAssetEvent(ctx context.Context, eventID string) (*domain.AssetEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var e *domain.AssetEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+assetEventColumns+` FROM asset_events WHERE tenant_id = $1 AND event_id = $2`, tenantID, eventID)
		var err error
		e, err = scanAssetEvent(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAssetEventNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ListAssetEvents returns every event recorded against one asset, most
// recent first — backs GetAssetEventChain/ExplainAssetState-style queries.
func (s *PgStore) ListAssetEvents(ctx context.Context, assetID string) ([]domain.AssetEvent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.AssetEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+assetEventColumns+` FROM asset_events WHERE tenant_id = $1 AND asset_id = $2 ORDER BY created_at DESC`, tenantID, assetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanAssetEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) transitionAssetEvent(ctx context.Context, eventID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, eventID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE asset_events SET status = $1%s
			WHERE event_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAssetEventTransition
		}
		return nil
	})
}

// ValidateAssetEvent moves DRAFT -> VALIDATED. Evidence-requirement
// checks (negative path #1) run in the handler before this is called —
// this method only performs the guarded transition.
func (s *PgStore) ValidateAssetEvent(ctx context.Context, eventID string, at time.Time) error {
	return s.transitionAssetEvent(ctx, eventID, domain.AssetEventStatusDraft, domain.AssetEventStatusValidated,
		", validated_at = $2", at)
}

func (s *PgStore) ApproveAssetEvent(ctx context.Context, eventID, principalID string, at time.Time) error {
	return s.transitionAssetEvent(ctx, eventID, domain.AssetEventStatusValidated, domain.AssetEventStatusApproved,
		", approved_at = $2, approved_by_principal_id = $3", at, principalID)
}

// ApplyAssetEvent is the one command that reaches both APPLIED and (when
// the event carries a $ amount) ACCOUNTING_EVENT_EMITTED — see migration
// 000003's doc comment. For a DISPOSAL event it also performs the real
// book-state delta: fixed_assets.status ACTIVE/SUSPENDED -> DISPOSED, in
// the same transaction as the event's own status transition, so the two
// can never disagree. journalID/emittedAt are nil when the event carries
// no $ amount — the row lands in APPLIED and stays there.
func (s *PgStore) ApplyAssetEvent(ctx context.Context, eventID string, at time.Time, journalID *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var assetID, eventType string
		err := tx.QueryRow(ctx, `
			SELECT asset_id, event_type FROM asset_events WHERE event_id = $1 AND status = $2 AND tenant_id = $3
		`, eventID, domain.AssetEventStatusApproved, tenantID).Scan(&assetID, &eventType)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidAssetEventTransition
		}
		if err != nil {
			return err
		}

		if eventType == domain.AssetEventTypeDisposal {
			tag, err := tx.Exec(ctx, `
				UPDATE fixed_assets SET status = $1
				WHERE asset_id = $2 AND tenant_id = $3 AND status IN ($4, $5)
			`, domain.AssetStatusDisposed, assetID, tenantID, domain.AssetStatusActive, domain.AssetStatusSuspended)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrAssetNotEligibleForDisposal
			}
		}

		finalStatus := domain.AssetEventStatusApplied
		var emittedAt *time.Time
		if journalID != nil {
			finalStatus = domain.AssetEventStatusAccountingEventEmitted
			emittedAt = &at
		}
		tag, err := tx.Exec(ctx, `
			UPDATE asset_events SET status = $1, applied_at = $2, emitted_at = $3, journal_id = $4
			WHERE event_id = $5 AND status = $6 AND tenant_id = $7
		`, finalStatus, at, emittedAt, journalID, eventID, domain.AssetEventStatusApproved, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAssetEventTransition
		}
		return nil
	})
}

// ReverseAssetEvent moves ACCOUNTING_EVENT_EMITTED -> REVERSED. If the
// original event was a DISPOSAL, the asset's own DISPOSED status is
// reverted back to ACTIVE in the same transaction — see migration
// 000003's doc comment on why Reverse/Supersede are the only correction
// paths for an applied event.
func (s *PgStore) ReverseAssetEvent(ctx context.Context, eventID, principalID, reason string, at time.Time) error {
	return s.correctAssetEvent(ctx, eventID, principalID, reason, at, domain.AssetEventStatusReversed,
		", reversed_at = $2, reversed_by_principal_id = $3, reversal_reason = $4")
}

// SupersedeAssetEvent moves ACCOUNTING_EVENT_EMITTED -> SUPERSEDED,
// performing the identical reversal-of-effects as ReverseAssetEvent —
// the two are distinct named commands (per spec) with distinct terminal
// statuses, but mechanically identical in this v1: Supersede additionally
// signals that a corrected replacement event (linking back via
// correction_of_event_id) is expected to follow.
func (s *PgStore) SupersedeAssetEvent(ctx context.Context, eventID, principalID, reason string, at time.Time) error {
	return s.correctAssetEvent(ctx, eventID, principalID, reason, at, domain.AssetEventStatusSuperseded,
		", superseded_at = $2, superseded_by_principal_id = $3, supersession_reason = $4")
}

func (s *PgStore) correctAssetEvent(ctx context.Context, eventID, principalID, reason string, at time.Time, toStatus, extraSet string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var assetID, eventType string
		err := tx.QueryRow(ctx, `
			SELECT asset_id, event_type FROM asset_events WHERE event_id = $1 AND status = $2 AND tenant_id = $3
		`, eventID, domain.AssetEventStatusAccountingEventEmitted, tenantID).Scan(&assetID, &eventType)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidAssetEventTransition
		}
		if err != nil {
			return err
		}

		if eventType == domain.AssetEventTypeDisposal {
			if _, err := tx.Exec(ctx, `
				UPDATE fixed_assets SET status = $1 WHERE asset_id = $2 AND tenant_id = $3 AND status = $4
			`, domain.AssetStatusActive, assetID, tenantID, domain.AssetStatusDisposed); err != nil {
				return err
			}
		}

		tag, err := tx.Exec(ctx, fmt.Sprintf(`
			UPDATE asset_events SET status = $1%s
			WHERE event_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, 5, 6, 7),
			toStatus, at, principalID, reason, eventID, domain.AssetEventStatusAccountingEventEmitted, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAssetEventTransition
		}
		return nil
	})
}

func mapAssetEventPgError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrAssetEventNotFound
	}
	return err
}
