package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// ── workload bindings (ZS-IAM-001 §16; Governance audit N-09) ───────────────
//
// 000017 created workload_bindings and the decision engine read it, but
// nothing could write a row: every WORKLOAD-typed request was denied as
// unbound, and a binding's allowed_actions was never consulted. These are the
// administration writes behind /v1/admin/workload-bindings and the lookup
// that stops the workload layer being opt-in.

const workloadColumns = `workload_id, tenant_id, allowed_audience, allowed_actions, active_flag, created_at`

func scanWorkload(row pgx.Row) (*domain.WorkloadBinding, error) {
	var wb domain.WorkloadBinding
	var tid uuid.UUID
	if err := row.Scan(&wb.WorkloadID, &tid, &wb.AllowedAudience, &wb.AllowedActions, &wb.ActiveFlag, &wb.CreatedAt); err != nil {
		return nil, err
	}
	wb.TenantID = tid.String()
	return &wb, nil
}

// SaveWorkloadBinding creates or replaces the binding of workloadID in its
// tenant and writes event to the outbox in the same transaction, so the change
// is announced and attributable (actor and reason ride on the event).
func (s *PgStore) SaveWorkloadBinding(ctx context.Context, wb domain.WorkloadBinding, event domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	const query = `
		INSERT INTO workload_bindings (workload_id, tenant_id, allowed_audience, allowed_actions, active_flag)
		VALUES ($1, $2::uuid, $3, $4, TRUE)
		ON CONFLICT (workload_id, tenant_id) DO UPDATE
		SET allowed_audience = EXCLUDED.allowed_audience,
		    allowed_actions  = EXCLUDED.allowed_actions,
		    active_flag      = TRUE
		RETURNING ` + workloadColumns + `;`
	var out *domain.WorkloadBinding
	err := s.withRLS(ctx, wb.TenantID, func(tx pgx.Tx) error {
		var err error
		if out, err = scanWorkload(tx.QueryRow(ctx, query, wb.WorkloadID, wb.TenantID, wb.AllowedAudience, wb.AllowedActions)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, insertOutboxSQL, event.EventType, event.Key, event.Value, event.TenantID)
		return err
	})
	if err != nil {
		s.log.Error("pg SaveWorkloadBinding failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// SetWorkloadBindingActive switches a binding on or off, with its event, in
// one transaction. ErrWorkloadBindingNotFound when the tenant has no binding
// for workloadID.
func (s *PgStore) SetWorkloadBindingActive(ctx context.Context, workloadID, tenantID string, active bool, event domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	const query = `
		UPDATE workload_bindings SET active_flag = $3
		 WHERE workload_id = $1 AND tenant_id = $2::uuid
		RETURNING ` + workloadColumns + `;`
	var out *domain.WorkloadBinding
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		if out, err = scanWorkload(tx.QueryRow(ctx, query, workloadID, tenantID, active)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, insertOutboxSQL, event.EventType, event.Key, event.Value, event.TenantID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWorkloadBindingNotFound
	}
	if err != nil {
		s.log.Error("pg SetWorkloadBindingActive failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ListWorkloadBindings is the tenant's bindings, active and not.
func (s *PgStore) ListWorkloadBindings(ctx context.Context, tenantID string) ([]domain.WorkloadBinding, error) {
	const query = `SELECT ` + workloadColumns + ` FROM workload_bindings WHERE tenant_id = $1::uuid ORDER BY workload_id;`
	out := []domain.WorkloadBinding{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			wb, err := scanWorkload(rows)
			if err != nil {
				return err
			}
			out = append(out, *wb)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListWorkloadBindings failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// IsBoundWorkload reports whether principalID is a workload bound in ANY
// tenant, active or not. The decision engine uses it so a workload is
// evaluated as one even when the request does not say so: the workload layer
// used to apply only to callers that declared principal_type WORKLOAD, so a
// workload could step outside its binding by omitting the field. Read under
// the platform scope: the question is about the principal, not a tenant.
func (s *PgStore) IsBoundWorkload(ctx context.Context, principalID string) (bool, error) {
	var bound bool
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workload_bindings WHERE workload_id = $1);`, principalID).Scan(&bound)
	})
	if err != nil {
		s.log.Error("pg IsBoundWorkload failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return bound, nil
}
