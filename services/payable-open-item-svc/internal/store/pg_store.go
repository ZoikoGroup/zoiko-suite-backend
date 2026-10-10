// Package store is the Postgres persistence layer for payable-open-item-svc.
package store

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payable-open-item-svc/internal/domain"
	"zoiko.io/payable-open-item-svc/internal/middleware"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreatePayable(ctx context.Context, tenantID string, req domain.CreatePayableRequest, principalID string) (*domain.PayableOpenItem, error)
	FindPayable(ctx context.Context, payableID string) (*domain.PayableOpenItem, error)
	FindBySource(ctx context.Context, sourceType domain.SourceType, sourceReference string) (*domain.PayableOpenItem, error)
	ListOpenPayables(ctx context.Context, legalEntityID string) ([]domain.PayableOpenItem, error)
	GetSupplierBalance(ctx context.Context, payeeRef string) (float64, error)

	ApplySupplierCredit(ctx context.Context, payableID string, req domain.ApplySupplierCreditRequest, principalID string) (*domain.PayableOpenItem, error)
	PlaceHold(ctx context.Context, payableID string, req domain.PlaceHoldRequest, principalID string) (*domain.PayableOpenItem, error)
	ReleaseHold(ctx context.Context, payableID, principalID string) (*domain.PayableOpenItem, error)
	OpenDispute(ctx context.Context, payableID string, req domain.OpenDisputeRequest, principalID string) (*domain.PayableOpenItem, error)
	ResolveDispute(ctx context.Context, payableID string, req domain.ResolveDisputeRequest, principalID string) (*domain.PayableOpenItem, error)
	ApplyConfirmedPayment(ctx context.Context, payableID string, req domain.ApplyConfirmedPaymentRequest, principalID string) (*domain.PayableOpenItem, bool, error)
	ApplyRecovery(ctx context.Context, payableID string, req domain.ApplyRecoveryRequest, principalID string) (*domain.PayableOpenItem, error)
	ClosePayable(ctx context.Context, payableID, principalID string) (*domain.PayableOpenItem, error)

	ListApplications(ctx context.Context, payableID string) ([]domain.SettlementApplication, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const payableColumns = `
	payable_id, tenant_id, legal_entity_id, source_type, source_reference, payee_ref,
	original_amount, residual_amount, currency, due_date, status,
	is_held, hold_reason, is_disputed, dispute_reason, dispute_opened_at, closed_at,
	created_by_principal_id, created_at, updated_at`

func scanPayable(row pgx.Row) (*domain.PayableOpenItem, error) {
	p := &domain.PayableOpenItem{}
	err := row.Scan(&p.PayableID, &p.TenantID, &p.LegalEntityID, &p.SourceType, &p.SourceReference, &p.PayeeRef,
		&p.OriginalAmount, &p.ResidualAmount, &p.Currency, &p.DueDate, &p.Status,
		&p.IsHeld, &p.HoldReason, &p.IsDisputed, &p.DisputeReason, &p.DisputeOpenedAt, &p.ClosedAt,
		&p.CreatedByPrincipalID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

// recordApplication inserts one settlement application. It reports
// inserted=false when the idempotency reference was already recorded. ON
// CONFLICT DO NOTHING (rather than catching the unique violation) keeps the
// surrounding transaction usable — a failed INSERT would abort it.
func (s *PgStore) recordApplication(ctx context.Context, tx pgx.Tx, tenantID *string, payableID, appType string, amount float64, idempotencyRef, detail, actorPrincipalID string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO settlement_applications (application_id, tenant_id, payable_id, application_type, amount, idempotency_ref, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING`,
		uuid.New().String(), tenantID, payableID, appType, amount, idempotencyRef, detail, actorPrincipalID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PgStore) CreatePayable(ctx context.Context, tenantID string, req domain.CreatePayableRequest, principalID string) (*domain.PayableOpenItem, error) {
	payableID := uuid.New().String()
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			INSERT INTO payable_open_items (`+payableColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8, $9, 'OPEN', FALSE, '', FALSE, '', NULL, NULL, $10, NOW(), NOW())
			RETURNING `+payableColumns,
			payableID, nullableTenant(tenantID), req.LegalEntityID, req.SourceType, req.SourceReference, req.PayeeRef,
			req.OriginalAmount, req.Currency, req.DueDate, principalID,
		))
		return err
	})
	if isUniqueViolation(err) {
		return nil, domain.ErrDuplicateSource
	}
	if err != nil {
		s.log.Error("pg CreatePayable failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FindPayable(ctx context.Context, payableID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `SELECT `+payableColumns+` FROM payable_open_items WHERE payable_id = $1`, payableID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPayableNotFound
	}
	if err != nil {
		s.log.Error("pg FindPayable failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FindBySource(ctx context.Context, sourceType domain.SourceType, sourceReference string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `SELECT `+payableColumns+` FROM payable_open_items WHERE source_type = $1 AND source_reference = $2`, sourceType, sourceReference))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPayableNotFound
	}
	if err != nil {
		s.log.Error("pg FindBySource failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ListOpenPayables(ctx context.Context, legalEntityID string) ([]domain.PayableOpenItem, error) {
	var out []domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payableColumns+` FROM payable_open_items
			WHERE legal_entity_id = $1 AND status IN ('OPEN', 'PARTIALLY_SETTLED') AND closed_at IS NULL
			ORDER BY due_date ASC`, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPayable(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListOpenPayables failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) GetSupplierBalance(ctx context.Context, payeeRef string) (float64, error) {
	var total float64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(residual_amount), 0) FROM payable_open_items
			WHERE payee_ref = $1 AND status IN ('OPEN', 'PARTIALLY_SETTLED') AND closed_at IS NULL`, payeeRef,
		).Scan(&total)
	})
	if err != nil {
		s.log.Error("pg GetSupplierBalance failed", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return total, nil
}

// applyResidualDelta is the single shared path every residual-changing
// command goes through — it recomputes status from the new residual
// server-side (never accepts a caller-supplied absolute residual), and
// refuses to let it go negative unless allowNegative is set (only
// ApplySupplierCredit sets it, per the spec's own "residual cannot go
// negative except explicit supplier-credit state").
// application is one settlement_applications row: its type, signed delta
// against the residual, and idempotency reference.
type application struct {
	appType        string
	delta          float64
	idempotencyRef string
}

func (s *PgStore) applyResidualDelta(ctx context.Context, payableID, appType string, delta float64, idempotencyRef, detail, principalID string, allowNegative bool) (*domain.PayableOpenItem, bool, error) {
	return s.applyApplications(ctx, payableID, []application{{appType, delta, idempotencyRef}}, detail, principalID, allowNegative)
}

// applyApplications records one or more applications against a payable in
// a single transaction and moves the residual by their sum. Idempotency is
// decided by the first application: if its reference was already recorded,
// the whole set is a replay and nothing changes.
func (s *PgStore) applyApplications(ctx context.Context, payableID string, apps []application, detail, principalID string, allowNegative bool) (*domain.PayableOpenItem, bool, error) {
	var p *domain.PayableOpenItem
	var applied bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var residual float64
		var status domain.PayableStatus
		if err := tx.QueryRow(ctx, `SELECT tenant_id, residual_amount, status FROM payable_open_items WHERE payable_id = $1 FOR UPDATE`, payableID).
			Scan(&tenantID, &residual, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPayableNotFound
			}
			return err
		}

		// The idempotency insert must be attempted BEFORE the
		// state-transition guard: a replayed payment that fully settled the
		// payable on its first application would otherwise be rejected as
		// "already settled" on replay instead of returning the idempotent
		// no-op it actually is.
		var total float64
		for i, a := range apps {
			inserted, err := s.recordApplication(ctx, tx, tenantID, payableID, a.appType, a.delta, a.idempotencyRef, detail, principalID)
			if err != nil {
				return err
			}
			if !inserted {
				if i > 0 {
					return fmt.Errorf("application %s %q partially recorded before", a.appType, a.idempotencyRef)
				}
				applied = false
				p, err = scanPayable(tx.QueryRow(ctx, `SELECT `+payableColumns+` FROM payable_open_items WHERE payable_id = $1`, payableID))
				return err
			}
			total += a.delta
		}

		if !domain.CanApplySettlement(status) {
			return domain.ErrInvalidTransition
		}

		// NUMERIC(18,2) on disk: compare and store in cents so float noise
		// can neither block a full settlement nor leave a phantom residual.
		newResidual := math.Round((residual+total)*100) / 100
		if newResidual < 0 && !allowNegative {
			return domain.ErrResidualWouldGoNegative
		}
		newStatus := domain.StatusPartiallySettled
		if newResidual <= 0 {
			newStatus = domain.StatusSettled
		}

		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET residual_amount = $1, status = $2, updated_at = NOW()
			WHERE payable_id = $3
			RETURNING `+payableColumns,
			newResidual, newStatus, payableID,
		))
		applied = true
		return err
	})
	if errors.Is(err, domain.ErrPayableNotFound) || isInvalidUUID(err) {
		return nil, false, domain.ErrPayableNotFound
	}
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrResidualWouldGoNegative) {
		return nil, false, err
	}
	if err != nil {
		s.log.Error("pg applyResidualDelta failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, applied, nil
}

func (s *PgStore) ApplySupplierCredit(ctx context.Context, payableID string, req domain.ApplySupplierCreditRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, _, err := s.applyResidualDelta(ctx, payableID, "SUPPLIER_CREDIT", -req.Amount, req.CreditRef, req.Reason, principalID, true)
	return p, err
}

// ApplyConfirmedPayment settles the net payment and, when present, the
// withheld portion (paid to the tax authority rather than the supplier) in
// one transaction, both keyed by the same Banking payment reference.
func (s *PgStore) ApplyConfirmedPayment(ctx context.Context, payableID string, req domain.ApplyConfirmedPaymentRequest, principalID string) (*domain.PayableOpenItem, bool, error) {
	apps := []application{{"PAYMENT", -req.Amount, req.ProviderPaymentRef}}
	if req.WithholdingAmount > 0 {
		apps = append(apps, application{"WITHHOLDING", -req.WithholdingAmount, req.ProviderPaymentRef})
	}
	return s.applyApplications(ctx, payableID, apps, "bnk07:"+req.Bnk07PaymentID, principalID, false)
}

func (s *PgStore) ApplyRecovery(ctx context.Context, payableID string, req domain.ApplyRecoveryRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, _, err := s.applyResidualDelta(ctx, payableID, "RECOVERY", -req.Amount, req.RecoveryRef, req.Reason, principalID, false)
	return p, err
}

func (s *PgStore) PlaceHold(ctx context.Context, payableID string, req domain.PlaceHoldRequest, principalID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET is_held = TRUE, hold_reason = $1, updated_at = NOW()
			WHERE payable_id = $2 AND closed_at IS NULL
			RETURNING `+payableColumns,
			req.Reason, payableID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPayableNotFound
	}
	if err != nil {
		s.log.Error("pg PlaceHold failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ReleaseHold(ctx context.Context, payableID, principalID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET is_held = FALSE, hold_reason = '', updated_at = NOW()
			WHERE payable_id = $1 AND closed_at IS NULL
			RETURNING `+payableColumns,
			payableID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPayableNotFound
	}
	if err != nil {
		s.log.Error("pg ReleaseHold failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) OpenDispute(ctx context.Context, payableID string, req domain.OpenDisputeRequest, principalID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET is_disputed = TRUE, dispute_reason = $1, dispute_opened_at = NOW(), updated_at = NOW()
			WHERE payable_id = $2 AND closed_at IS NULL
			RETURNING `+payableColumns,
			req.Reason, payableID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPayableNotFound
	}
	if err != nil {
		s.log.Error("pg OpenDispute failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ResolveDispute(ctx context.Context, payableID string, req domain.ResolveDisputeRequest, principalID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET is_disputed = FALSE, dispute_reason = $1, dispute_opened_at = NULL, updated_at = NOW()
			WHERE payable_id = $2 AND closed_at IS NULL AND is_disputed = TRUE
			RETURNING `+payableColumns,
			"resolved: "+req.Resolution, payableID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ResolveDispute failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ClosePayable(ctx context.Context, payableID, principalID string) (*domain.PayableOpenItem, error) {
	var p *domain.PayableOpenItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var status domain.PayableStatus
		var isHeld, isDisputed bool
		if err := tx.QueryRow(ctx, `SELECT status, is_held, is_disputed FROM payable_open_items WHERE payable_id = $1 AND closed_at IS NULL`, payableID).
			Scan(&status, &isHeld, &isDisputed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPayableNotFound
			}
			return err
		}
		if !domain.CanClose(status, isHeld, isDisputed) {
			if isHeld || isDisputed {
				return domain.ErrPayableHeldOrDisputed
			}
			return domain.ErrPayableNotFullySettled
		}
		var err error
		p, err = scanPayable(tx.QueryRow(ctx, `
			UPDATE payable_open_items SET closed_at = NOW(), updated_at = NOW()
			WHERE payable_id = $1
			RETURNING `+payableColumns,
			payableID,
		))
		return err
	})
	if errors.Is(err, domain.ErrPayableNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrPayableNotFound
	}
	if errors.Is(err, domain.ErrPayableHeldOrDisputed) || errors.Is(err, domain.ErrPayableNotFullySettled) {
		return nil, err
	}
	if err != nil {
		s.log.Error("pg ClosePayable failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ListApplications(ctx context.Context, payableID string) ([]domain.SettlementApplication, error) {
	var out []domain.SettlementApplication
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT application_id, tenant_id, payable_id, application_type, amount, idempotency_ref, detail, actor_principal_id, created_at
			FROM settlement_applications WHERE payable_id = $1 ORDER BY created_at ASC`, payableID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.SettlementApplication
			if err := rows.Scan(&a.ApplicationID, &a.TenantID, &a.PayableID, &a.ApplicationType, &a.Amount,
				&a.IdempotencyRef, &a.Detail, &a.ActorPrincipalID, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListApplications failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
