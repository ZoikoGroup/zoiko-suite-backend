// Package store provides the PostgreSQL implementation of purchase-request-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policy is real and correctly written.
// But every method ALSO filters explicitly by tenant_id in its own SQL,
// rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy. This was found via genuine CI failures in
// general-ledger-svc and tenant-entity-registry-svc, so this service is
// built with the explicit filter from day one rather than discovering the
// same gap a third time.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// CreateRequest inserts a purchase request header in PENDING status.
func (s *PgStore) CreateRequest(ctx context.Context, req *domain.PurchaseRequest) error {
	tenantID := tenantFromCtxOrFallback(ctx, req.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, `
			INSERT INTO purchase_requests (
				request_id, tenant_id, legal_entity_id, requested_by_principal_id,
				description, amount, currency_code, status, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, req.RequestID, req.TenantID, req.LegalEntityID, req.RequestedByPrincipalID,
			req.Description, req.Amount, req.CurrencyCode, string(req.Status), req.CorrelationID, now)
		if err != nil {
			return err
		}
		req.CreatedAt = now
		return nil
	})
}

// GetRequest returns (nil, nil) if not found — including when the caller's
// tenant scope doesn't match the request's tenant (explicit filter, not
// RLS-only — see package doc).
func (s *PgStore) GetRequest(ctx context.Context, requestID string) (*domain.PurchaseRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var r domain.PurchaseRequest
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT request_id, tenant_id, legal_entity_id, requested_by_principal_id,
			       description, amount, currency_code, status,
			       approved_by_principal_id, rejected_by_principal_id, rejection_reason,
			       correlation_id, created_at, approved_at, rejected_at
			FROM purchase_requests WHERE request_id = $1 AND tenant_id = $2
		`, requestID, tenantID)
		if err := row.Scan(
			&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.RequestedByPrincipalID,
			&r.Description, &r.Amount, &r.CurrencyCode, &status,
			&r.ApprovedByPrincipalID, &r.RejectedByPrincipalID, &r.RejectionReason,
			&r.CorrelationID, &r.CreatedAt, &r.ApprovedAt, &r.RejectedAt,
		); err != nil {
			return err
		}
		r.Status = domain.RequestStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRequests returns purchase requests matching the given filter
// (tenant_id is required; the others are optional).
func (s *PgStore) ListRequests(ctx context.Context, filter domain.ListRequestsFilter) ([]domain.PurchaseRequest, error) {
	var out []domain.PurchaseRequest
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT request_id, tenant_id, legal_entity_id, requested_by_principal_id,
			       description, amount, currency_code, status,
			       approved_by_principal_id, rejected_by_principal_id, rejection_reason,
			       correlation_id, created_at, approved_at, rejected_at
			FROM purchase_requests
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR status = $3)
			ORDER BY created_at DESC
		`, filter.TenantID, filter.LegalEntityID, filter.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.PurchaseRequest
			var status string
			if err := rows.Scan(
				&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.RequestedByPrincipalID,
				&r.Description, &r.Amount, &r.CurrencyCode, &status,
				&r.ApprovedByPrincipalID, &r.RejectedByPrincipalID, &r.RejectionReason,
				&r.CorrelationID, &r.CreatedAt, &r.ApprovedAt, &r.RejectedAt,
			); err != nil {
				return err
			}
			r.Status = domain.RequestStatus(status)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// TransitionRequest moves a request from PENDING to either APPROVED or
// REJECTED, using WHERE status = 'PENDING' AND tenant_id = $tenantID so the
// transition, the state-machine check, and the tenant scope are one atomic
// statement — no separate read, no race window. Returns
// domain.ErrInvalidTransition if zero rows were affected (the request
// doesn't exist, wasn't PENDING, or belongs to a different tenant).
func (s *PgStore) TransitionRequest(ctx context.Context, tenantID, requestID string, toStatus domain.RequestStatus, actorPrincipalID string, rejectionReason *string) error {
	var query string
	var args []any
	switch toStatus {
	case domain.RequestStatusApproved:
		query = `
			UPDATE purchase_requests
			SET status = $1, approved_by_principal_id = $2, approved_at = $3
			WHERE request_id = $4 AND status = 'PENDING' AND tenant_id = $5
		`
		args = []any{string(toStatus), actorPrincipalID, time.Now().UTC(), requestID, tenantID}
	case domain.RequestStatusRejected:
		query = `
			UPDATE purchase_requests
			SET status = $1, rejected_by_principal_id = $2, rejected_at = $3, rejection_reason = $4
			WHERE request_id = $5 AND status = 'PENDING' AND tenant_id = $6
		`
		args = []any{string(toStatus), actorPrincipalID, time.Now().UTC(), rejectionReason, requestID, tenantID}
	default:
		return domain.ErrInvalidTransition
	}

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}
