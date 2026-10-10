package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// CreateWorkflowRef appends one close_workflow_refs row in its OWN committed
// transaction (withRLS commits before returning), so that a caller who then
// invokes REF-05 knows REF-05's verification callback can already read it.
// The tenant is taken from ctx and written to the row; wr.TenantID is ignored.
func (s *PgStore) CreateWorkflowRef(ctx context.Context, wr *domain.WorkflowRef) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO close_workflow_refs (
				ref_id, tenant_id, legal_entity_id, fiscal_period_id, period_name, period_key,
				command, status, control_snapshot_ref, requested_by, reason
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING created_at
		`, wr.RefID, tenantID, wr.LegalEntityID, wr.FiscalPeriodID, wr.PeriodName, wr.PeriodKey,
			wr.Command, wr.Status, wr.ControlSnapshotRef, wr.RequestedBy, wr.Reason).Scan(&wr.CreatedAt)
	})
	if err == nil {
		wr.TenantID = tenantID
	}
	return mapPgError(err)
}

// GetWorkflowRef reads one ref for the tenant in ctx. A malformed id, an unknown
// id and another tenant's id all return ErrWorkflowRefNotFound.
func (s *PgStore) GetWorkflowRef(ctx context.Context, refID string) (*domain.WorkflowRef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if _, err := uuid.Parse(refID); err != nil {
		return nil, domain.ErrWorkflowRefNotFound
	}
	var wr domain.WorkflowRef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT ref_id, tenant_id, legal_entity_id, fiscal_period_id, period_name, period_key,
			       command, status, control_snapshot_ref, requested_by, reason, created_at
			FROM close_workflow_refs WHERE ref_id = $1 AND tenant_id = $2
		`, refID, tenantID).Scan(&wr.RefID, &wr.TenantID, &wr.LegalEntityID, &wr.FiscalPeriodID, &wr.PeriodName,
			&wr.PeriodKey, &wr.Command, &wr.Status, &wr.ControlSnapshotRef, &wr.RequestedBy, &wr.Reason, &wr.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWorkflowRefNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &wr, nil
}
