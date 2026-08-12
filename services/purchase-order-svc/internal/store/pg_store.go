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
	"time"

	"github.com/google/uuid"
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
	if pgErr.Code == "22P02" {
		return domain.ErrInvalidIdentifier
	}
	return err
}

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

// CreateOrder inserts a purchase order in ISSUED status, generating its
// po_number from purchase_order_number_seq. Idempotent on
// (tenant_id, correlation_id): if a row already exists for that pair, o is
// overwritten with the EXISTING row's values and created=false is returned —
// callers must not re-publish purchase.order.issued in that case.
func (s *PgStore) CreateOrder(ctx context.Context, o *domain.PurchaseOrder) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, o.TenantID)

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, "SELECT nextval('purchase_order_number_seq')").Scan(&seq); err != nil {
			return err
		}
		poNumber := fmt.Sprintf("PO-%06d", seq)
		now := time.Now().UTC()

		tag, err := tx.Exec(ctx, `
			INSERT INTO purchase_orders (
				purchase_order_id, tenant_id, legal_entity_id, purchase_request_id,
				vendor_profile_id, po_number, po_status, total_amount, currency_code,
				version, issued_by_principal_id, correlation_id, created_at, issued_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, o.PurchaseOrderID, o.TenantID, o.LegalEntityID, o.PurchaseRequestID,
			o.VendorProfileID, poNumber, string(domain.OrderStatusIssued), o.TotalAmount, o.CurrencyCode,
			1, o.IssuedByPrincipalID, o.CorrelationID, now, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			o.PONumber = poNumber
			o.Status = domain.OrderStatusIssued
			o.Version = 1
			o.CreatedAt = now
			o.IssuedAt = now
			return nil
		}

		// Conflict: an order for this (tenant_id, correlation_id) already
		// exists — this is a retry, not a new order. Fetch the existing row
		// so the handler can return it as-is (idempotent response).
		row := tx.QueryRow(ctx, `
			SELECT purchase_order_id, legal_entity_id, purchase_request_id, vendor_profile_id,
			       po_number, po_status, total_amount, currency_code, version,
			       issued_by_principal_id, closed_by_principal_id,
			       created_at, issued_at, closed_at
			FROM purchase_orders WHERE tenant_id = $1 AND correlation_id = $2
		`, o.TenantID, o.CorrelationID)
		var status string
		if err := row.Scan(
			&o.PurchaseOrderID, &o.LegalEntityID, &o.PurchaseRequestID, &o.VendorProfileID,
			&o.PONumber, &status, &o.TotalAmount, &o.CurrencyCode, &o.Version,
			&o.IssuedByPrincipalID, &o.ClosedByPrincipalID,
			&o.CreatedAt, &o.IssuedAt, &o.ClosedAt,
		); err != nil {
			return err
		}
		o.Status = domain.OrderStatus(status)
		created = false
		return nil
	})
	return created, err
}

// GetOrder returns (nil, nil) if not found — including when the caller's
// tenant scope doesn't match the order's tenant (explicit filter, not
// RLS-only — see package doc).
func (s *PgStore) GetOrder(ctx context.Context, orderID string) (*domain.PurchaseOrder, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var o domain.PurchaseOrder
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT purchase_order_id, tenant_id, legal_entity_id, purchase_request_id, vendor_profile_id,
			       po_number, po_status, total_amount, currency_code, version,
			       issued_by_principal_id, closed_by_principal_id, correlation_id,
			       created_at, issued_at, closed_at
			FROM purchase_orders WHERE purchase_order_id = $1 AND tenant_id = $2
		`, orderID, tenantID)
		if err := row.Scan(
			&o.PurchaseOrderID, &o.TenantID, &o.LegalEntityID, &o.PurchaseRequestID, &o.VendorProfileID,
			&o.PONumber, &status, &o.TotalAmount, &o.CurrencyCode, &o.Version,
			&o.IssuedByPrincipalID, &o.ClosedByPrincipalID, &o.CorrelationID,
			&o.CreatedAt, &o.IssuedAt, &o.ClosedAt,
		); err != nil {
			return mapPgError(err)
		}
		o.Status = domain.OrderStatus(status)
		return nil
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
	return &o, nil
}

// ListOrders returns purchase orders matching the given filter (tenant_id is
// required; the others are optional).
func (s *PgStore) ListOrders(ctx context.Context, filter domain.ListOrdersFilter) ([]domain.PurchaseOrder, error) {
	var out []domain.PurchaseOrder
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT purchase_order_id, tenant_id, legal_entity_id, purchase_request_id, vendor_profile_id,
			       po_number, po_status, total_amount, currency_code, version,
			       issued_by_principal_id, closed_by_principal_id, correlation_id,
			       created_at, issued_at, closed_at
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
			var o domain.PurchaseOrder
			var status string
			if err := rows.Scan(
				&o.PurchaseOrderID, &o.TenantID, &o.LegalEntityID, &o.PurchaseRequestID, &o.VendorProfileID,
				&o.PONumber, &status, &o.TotalAmount, &o.CurrencyCode, &o.Version,
				&o.IssuedByPrincipalID, &o.ClosedByPrincipalID, &o.CorrelationID,
				&o.CreatedAt, &o.IssuedAt, &o.ClosedAt,
			); err != nil {
				return err
			}
			o.Status = domain.OrderStatus(status)
			out = append(out, o)
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

// AmendOrder atomically reads the current version/total_amount and updates
// them in a single statement (CTE + FOR UPDATE lock), so the read and the
// write can never race with a concurrent amend/close. Returns
// domain.ErrInvalidTransition if the order doesn't exist, isn't ISSUED, or
// belongs to a different tenant. Records an append-only
// PurchaseOrderAmendment row in the same transaction.
func (s *PgStore) AmendOrder(ctx context.Context, tenantID, orderID string, newTotalAmount float64, reason, actorPrincipalID string) (*domain.PurchaseOrder, error) {
	var updated domain.PurchaseOrder
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH old AS (
				SELECT version, total_amount FROM purchase_orders
				WHERE purchase_order_id = $1 AND tenant_id = $2 AND po_status = 'ISSUED'
				FOR UPDATE
			)
			UPDATE purchase_orders po
			SET total_amount = $3, version = old.version + 1
			FROM old
			WHERE po.purchase_order_id = $1 AND po.tenant_id = $2 AND po.po_status = 'ISSUED'
			RETURNING old.version, old.total_amount, po.version, po.total_amount,
			          po.purchase_order_id, po.tenant_id, po.legal_entity_id,
			          po.purchase_request_id, po.vendor_profile_id, po.po_number,
			          po.po_status, po.currency_code, po.issued_by_principal_id,
			          po.closed_by_principal_id, po.correlation_id,
			          po.created_at, po.issued_at, po.closed_at
		`, orderID, tenantID, newTotalAmount)

		var fromVersion, toVersion int
		var previousAmount, newAmount float64
		if err := row.Scan(
			&fromVersion, &previousAmount, &toVersion, &newAmount,
			&updated.PurchaseOrderID, &updated.TenantID, &updated.LegalEntityID,
			&updated.PurchaseRequestID, &updated.VendorProfileID, &updated.PONumber,
			&status, &updated.CurrencyCode, &updated.IssuedByPrincipalID,
			&updated.ClosedByPrincipalID, &updated.CorrelationID,
			&updated.CreatedAt, &updated.IssuedAt, &updated.ClosedAt,
		); err != nil {
			return mapPgError(err)
		}
		updated.Status = domain.OrderStatus(status)
		updated.Version = toVersion
		updated.TotalAmount = newAmount

		_, err := tx.Exec(ctx, `
			INSERT INTO purchase_order_amendments (
				amendment_id, purchase_order_id, tenant_id, from_version, to_version,
				previous_total_amount, new_total_amount, reason, amended_by_principal_id, amended_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, uuid.NewString(), orderID, tenantID, fromVersion, toVersion,
			previousAmount, newAmount, reason, actorPrincipalID, time.Now().UTC())
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidTransition
	}
	// A malformed id names no order, so this is a not-found rather than an
	// illegal transition: answering invalid_transition would assert the order
	// exists and is in the wrong state, which is a different and untrue claim.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, domain.ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// CloseOrder moves an order from ISSUED to CLOSED, terminal, using
// WHERE po_status = 'ISSUED' AND tenant_id = $tenantID so the transition,
// the state-machine check, and the tenant scope are one atomic statement.
// Returns domain.ErrInvalidTransition if zero rows were affected.
func (s *PgStore) CloseOrder(ctx context.Context, tenantID, orderID, actorPrincipalID string) (*domain.PurchaseOrder, error) {
	var o domain.PurchaseOrder
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE purchase_orders
			SET po_status = 'CLOSED', closed_by_principal_id = $1, closed_at = $2
			WHERE purchase_order_id = $3 AND po_status = 'ISSUED' AND tenant_id = $4
			RETURNING purchase_order_id, tenant_id, legal_entity_id, purchase_request_id, vendor_profile_id,
			          po_number, po_status, total_amount, currency_code, version,
			          issued_by_principal_id, closed_by_principal_id, correlation_id,
			          created_at, issued_at, closed_at
		`, actorPrincipalID, time.Now().UTC(), orderID, tenantID)
		return mapPgError(row.Scan(
			&o.PurchaseOrderID, &o.TenantID, &o.LegalEntityID, &o.PurchaseRequestID, &o.VendorProfileID,
			&o.PONumber, &status, &o.TotalAmount, &o.CurrencyCode, &o.Version,
			&o.IssuedByPrincipalID, &o.ClosedByPrincipalID, &o.CorrelationID,
			&o.CreatedAt, &o.IssuedAt, &o.ClosedAt,
		))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidTransition
	}
	// As in AmendOrder: a malformed id names no order, so not-found rather than
	// a claim about its state.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, domain.ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	o.Status = domain.OrderStatus(status)
	return &o, nil
}
