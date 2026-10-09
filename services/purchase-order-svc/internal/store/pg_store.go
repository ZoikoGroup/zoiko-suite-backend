// Package store provides the PostgreSQL implementation of purchase-order-svc's
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
// built with the explicit filter from day one.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
)

// mapPgError translates driver-level failures that are really caller mistakes
// into domain errors, so they stop being reported as outages.
//
// 22P02 (invalid_text_representation) is the one that matters here:
// purchase_order_id, tenant_id and legal_entity_id are all uuid columns, so a
// mistyped id fails inside the driver before any row is examined. Left unmapped
// it surfaced as 503 store_unavailable — indistinguishable from the database
// being unreachable.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "22P02":
		return domain.ErrInvalidIdentifier
	case "P0001":
		// A trigger raised: the database refused a change the state machine,
		// approval rules or immutability rules do not allow. The store checks
		// these first for typed errors; this is the backstop.
		return fmt.Errorf("%w: %s", domain.ErrInvalidTransition, pgErr.Message)
	}
	return err
}

type PgStore struct {
	pool             *pgxpool.Pool
	overTolerancePct float64
	log              *zap.Logger
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

// tenantFromCtxOrFallback used to live here, resolving the RLS scope from the
// context but FALLING BACK to whatever the caller had supplied in the request
// body. A request carrying no X-Tenant-Id therefore chose its own scope: the
// body's tenant_id was handed to set_config('app.tenant_id') AND written into
// the row, so the policy that should have refused the insert was satisfied by
// the value under attack. The handler now resolves the tenant once from the
// verified header; there is deliberately no fallback left to reach for.

// GetOrder returns the order header, or (nil, nil) if not found — including
// when the caller's tenant scope doesn't match the order's tenant (explicit
// filter, not RLS-only — see package doc). Use GetOrderDetail for the lines.
func (s *PgStore) GetOrder(ctx context.Context, orderID string) (*domain.PurchaseOrder, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var o *domain.PurchaseOrder
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		o, err = scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM purchase_orders WHERE purchase_order_id = $1 AND tenant_id = $2`, orderID, tenantID))
		return mapPgError(err)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// A malformed purchase_order_id or tenant scope cannot name an existing row,
	// so it is absent — not an outage. Reported identically to a well-formed id
	// that happens not to exist, and to another tenant's order.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// ListOrders returns purchase orders matching the given filter (tenant_id is
// required; the others are optional).
func (s *PgStore) ListOrders(ctx context.Context, filter domain.ListOrdersFilter) ([]domain.PurchaseOrder, error) {
	var out []domain.PurchaseOrder
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+orderColumns+`
			FROM purchase_orders
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR po_status = $3)
			ORDER BY created_at DESC
		`, filter.TenantID, filter.LegalEntityID, filter.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOrder(rows)
			if err != nil {
				return err
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	return out, err
}

// ListAmendments returns the append-only amendment ledger for one order,
// oldest first so the version chain reads forwards (v1->v2, v2->v3, …).
//
// Until this existed the ledger was write-only: every amend recorded the
// before/after totals and the operator's reason, and nothing could read them
// back — the order's `version` counter was the only visible trace that an
// amendment had happened at all. Tenant scope comes from the request context
// and is applied as an explicit filter as well as via RLS, matching GetOrder.
func (s *PgStore) ListAmendments(ctx context.Context, orderID string) ([]domain.PurchaseOrderAmendment, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var out []domain.PurchaseOrderAmendment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT amendment_id, purchase_order_id, from_version, to_version,
			       previous_total_amount, new_total_amount, reason,
			       amended_by_principal_id, amended_at
			FROM purchase_order_amendments
			WHERE purchase_order_id = $1 AND tenant_id = $2
			ORDER BY from_version ASC, amended_at ASC
		`, orderID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.PurchaseOrderAmendment
			if err := rows.Scan(
				&a.AmendmentID, &a.PurchaseOrderID, &a.FromVersion, &a.ToVersion,
				&a.PreviousTotalAmount, &a.NewTotalAmount, &a.Reason,
				&a.AmendedByPrincipalID, &a.AmendedAt,
			); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}
