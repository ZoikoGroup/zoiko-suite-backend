package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

const costEntryColumns = `
	entry_id, legal_entity_id, project_id, wbs_id, source_type, source_reference,
	cost_category, quantity, amount, currency, transaction_date, billable, capitalizable, status,
	reclassifies_entry_id, reverses_entry_id, reason,
	created_at, created_by_principal_id, validated_at, approved_at, approved_by_principal_id`

func scanCostEntry(row pgx.Row) (*domain.CostEntry, error) {
	var e domain.CostEntry
	var costCategory *string
	if err := row.Scan(
		&e.EntryID, &e.LegalEntityID, &e.ProjectID, &e.WBSID, &e.SourceType, &e.SourceReference,
		&costCategory, &e.Quantity, &e.Amount, &e.Currency, &e.TransactionDate, &e.Billable, &e.Capitalizable, &e.Status,
		&e.ReclassifiesEntryID, &e.ReversesEntryID, &e.Reason,
		&e.CreatedAt, &e.CreatedByPrincipalID, &e.ValidatedAt, &e.ApprovedAt, &e.ApprovedByPrincipalID,
	); err != nil {
		return nil, err
	}
	if costCategory != nil {
		e.CostCategory = *costCategory
	}
	return &e, nil
}

// CaptureProjectCost is a real idempotent create — INSERT ... ON CONFLICT
// (tenant_id, source_type, source_reference) DO NOTHING. If the source
// line was already captured, *e is overwritten with the EXISTING row
// before returning — the caller (handler) returns e either way, the
// spec's own failure semantics verbatim: "Duplicate source line is
// idempotently rejected/returned."
func (s *PgStore) CaptureProjectCost(ctx context.Context, e *domain.CostEntry) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO project_cost_entries (
				entry_id, tenant_id, legal_entity_id, project_id, wbs_id, source_type, source_reference,
				cost_category, quantity, amount, currency, transaction_date, billable, capitalizable, status,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (tenant_id, source_type, source_reference) DO NOTHING
		`, e.EntryID, tenantID, e.LegalEntityID, e.ProjectID, e.WBSID, e.SourceType, e.SourceReference,
			nullIfEmpty(e.CostCategory), e.Quantity, e.Amount, e.Currency, e.TransactionDate, e.Billable, e.Capitalizable, e.Status,
			e.CreatedAt, e.CreatedByPrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		row := tx.QueryRow(ctx, `SELECT `+costEntryColumns+` FROM project_cost_entries WHERE tenant_id = $1 AND source_type = $2 AND source_reference = $3`, tenantID, e.SourceType, e.SourceReference)
		existing, err := scanCostEntry(row)
		if err != nil {
			return err
		}
		*e = *existing
		return nil
	})
}

func (s *PgStore) GetCostEntry(ctx context.Context, entryID string) (*domain.CostEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var e *domain.CostEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+costEntryColumns+` FROM project_cost_entries WHERE entry_id = $1 AND tenant_id = $2`, entryID, tenantID)
		var err error
		e, err = scanCostEntry(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCostEntryNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ListCostEntries backs GetProjectCosts/GetCostByWBS — filtered by
// project, optionally further by WBS.
func (s *PgStore) ListCostEntries(ctx context.Context, projectID, wbsID string) ([]domain.CostEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.CostEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + costEntryColumns + ` FROM project_cost_entries WHERE tenant_id = $1 AND project_id = $2`
		args := []any{tenantID, projectID}
		if wbsID != "" {
			query += ` AND wbs_id = $3`
			args = append(args, wbsID)
		}
		query += ` ORDER BY created_at`
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanCostEntry(rows)
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

// ValidateProjectCost moves CAPTURED -> ACCEPTED directly — collapsing
// the spec's own named Validated state, see migration 000002's doc
// comment.
func (s *PgStore) ValidateProjectCost(ctx context.Context, entryID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE project_cost_entries SET status = $1, validated_at = $2
			WHERE entry_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.CostEntryStatusAccepted, at, entryID, domain.CostEntryStatusCaptured, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCostEntryTransition
		}
		return nil
	})
}

// MarkBillableEligibility updates classification metadata only — never
// an economic field, so it is not blocked by the reject-mutation
// trigger.
func (s *PgStore) MarkBillableEligibility(ctx context.Context, entryID string, billable, capitalizable bool) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE project_cost_entries SET billable = $1, capitalizable = $2 WHERE entry_id = $3 AND tenant_id = $4
		`, billable, capitalizable, entryID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrCostEntryNotFound
		}
		return nil
	})
}

// CreateLinkedCostEntry is the shared mechanism behind ReclassifyProjectCost
// and ReverseProjectCost — see migration 000002's doc comment. Neither
// ever touches the original entry; each creates a brand-new one linked
// back via reclassifies_entry_id/reverses_entry_id, and refuses
// self-approval against the ORIGINAL entry's own creator.
func (s *PgStore) CreateLinkedCostEntry(ctx context.Context, originalEntryID, principalID, reason string, isReversal bool, newEntryID string, amountOverride *float64, costCategory *string, billable, capitalizable *bool, at time.Time) (*domain.CostEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var result *domain.CostEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+costEntryColumns+` FROM project_cost_entries WHERE entry_id = $1 AND tenant_id = $2`, originalEntryID, tenantID)
		original, err := scanCostEntry(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCostEntryNotFound
		}
		if err != nil {
			return err
		}
		if original.CreatedByPrincipalID == principalID {
			if isReversal {
				return domain.ErrSelfApprovalNotPermittedReversal
			}
			return domain.ErrSelfApprovalNotPermittedReclassify
		}
		if isReversal && original.Status == domain.CostEntryStatusReversed {
			return domain.ErrCostEntryAlreadyReversed
		}

		linked := &domain.CostEntry{
			EntryID: newEntryID, LegalEntityID: original.LegalEntityID, ProjectID: original.ProjectID, WBSID: original.WBSID,
			SourceType: original.SourceType, SourceReference: newEntryID, CostCategory: original.CostCategory,
			Quantity: original.Quantity, Currency: original.Currency, TransactionDate: at,
			Billable: original.Billable, Capitalizable: original.Capitalizable,
			Status: domain.CostEntryStatusAccepted, Reason: &reason,
			CreatedAt: at, CreatedByPrincipalID: principalID, ApprovedAt: &at, ApprovedByPrincipalID: &principalID,
		}
		if isReversal {
			linked.Amount = -original.Amount
			linked.ReversesEntryID = &originalEntryID
		} else {
			linked.Amount = original.Amount
			if amountOverride != nil {
				linked.Amount = *amountOverride
			}
			if costCategory != nil {
				linked.CostCategory = *costCategory
			}
			if billable != nil {
				linked.Billable = *billable
			}
			if capitalizable != nil {
				linked.Capitalizable = *capitalizable
			}
			linked.ReclassifiesEntryID = &originalEntryID
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO project_cost_entries (
				entry_id, tenant_id, legal_entity_id, project_id, wbs_id, source_type, source_reference,
				cost_category, quantity, amount, currency, transaction_date, billable, capitalizable, status,
				reclassifies_entry_id, reverses_entry_id, reason,
				created_at, created_by_principal_id, approved_at, approved_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		`, linked.EntryID, tenantID, linked.LegalEntityID, linked.ProjectID, linked.WBSID, linked.SourceType, linked.SourceReference,
			nullIfEmpty(linked.CostCategory), linked.Quantity, linked.Amount, linked.Currency, linked.TransactionDate, linked.Billable, linked.Capitalizable, linked.Status,
			linked.ReclassifiesEntryID, linked.ReversesEntryID, linked.Reason,
			linked.CreatedAt, linked.CreatedByPrincipalID, linked.ApprovedAt, linked.ApprovedByPrincipalID)
		if err != nil {
			return err
		}

		if isReversal {
			if _, err := tx.Exec(ctx, `UPDATE project_cost_entries SET status = $1 WHERE entry_id = $2 AND tenant_id = $3`, domain.CostEntryStatusReversed, originalEntryID, tenantID); err != nil {
				return err
			}
		}
		result = linked
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CertifyCostPopulation is a real, evidenced attestation over a
// project's own current cost population — the spec's own named
// "completion certificate," applied here.
func (s *PgStore) CertifyCostPopulation(ctx context.Context, projectID, principalID string, at time.Time) (*domain.CostCertification, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var cert *domain.CostCertification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var entryCount int
		var totalAmount float64
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM project_cost_entries
			WHERE tenant_id = $1 AND project_id = $2 AND status != $3
		`, tenantID, projectID, domain.CostEntryStatusReversed).Scan(&entryCount, &totalAmount); err != nil {
			return err
		}
		cert = &domain.CostCertification{
			CertificationID: uuidNewString(), ProjectID: projectID, EntryCount: entryCount, TotalAmount: totalAmount,
			CertifiedAt: at, CertifiedByPrincipalID: principalID,
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO project_cost_certifications (certification_id, tenant_id, project_id, entry_count, total_amount, certified_at, certified_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, cert.CertificationID, tenantID, projectID, cert.EntryCount, cert.TotalAmount, cert.CertifiedAt, cert.CertifiedByPrincipalID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cert, nil
}
