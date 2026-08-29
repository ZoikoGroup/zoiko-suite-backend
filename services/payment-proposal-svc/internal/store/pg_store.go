// Package store implements payment-proposal-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
	"zoiko.io/payment-proposal-svc/internal/middleware"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isProposalNotComposable reports whether err is the custom SQLSTATE ZK001
// raised by the proposal_items BEFORE INSERT trigger when the parent
// proposal is no longer DRAFT/CALCULATED/REVIEW — see the migration.
func isProposalNotComposable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "ZK001"
}

type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Store is the interface the handler depends on.
type Store interface {
	CreateProposal(ctx context.Context, tenantID string, req domain.CreateProposalRequest, principalID string) (*domain.PaymentProposal, error)
	FindProposal(ctx context.Context, proposalID string) (*domain.PaymentProposal, error)

	AddItem(ctx context.Context, item domain.ProposalItem) (*domain.ProposalItem, error)
	FindItem(ctx context.Context, itemID string) (*domain.ProposalItem, error)
	RemoveItem(ctx context.Context, itemID string) error
	ListItems(ctx context.Context, proposalID string) ([]domain.ProposalItem, error)

	RecalculateProposal(ctx context.Context, proposalID string, gross, withholding, net float64, principalID string) (*domain.PaymentProposal, error)
	SubmitForReview(ctx context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error)
	FreezeProposal(ctx context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error)
	CancelProposal(ctx context.Context, proposalID string, req domain.CancelProposalRequest, principalID string) (*domain.PaymentProposal, error)

	ListEvents(ctx context.Context, proposalID string) ([]domain.ProposalEvent, error)
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

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, proposalID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO proposal_events (event_id, tenant_id, proposal_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, proposalID, eventType, detail, actorPrincipalID,
	)
	return err
}

// ── proposals ────────────────────────────────────────────────────────────────

const proposalColumns = `
	proposal_id, tenant_id, legal_entity_id, paying_bank_account_ref, currency, payment_date, payment_method,
	status, gross_amount, withholding_amount, net_amount, frozen_by_principal_id, frozen_at,
	created_by_principal_id, created_at, updated_at`

func scanProposal(row pgx.Row) (*domain.PaymentProposal, error) {
	p := &domain.PaymentProposal{}
	err := row.Scan(&p.ProposalID, &p.TenantID, &p.LegalEntityID, &p.PayingBankAccountRef, &p.Currency, &p.PaymentDate,
		&p.PaymentMethod, &p.Status, &p.GrossAmount, &p.WithholdingAmount, &p.NetAmount, &p.FrozenByPrincipalID,
		&p.FrozenAt, &p.CreatedByPrincipalID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) CreateProposal(ctx context.Context, tenantID string, req domain.CreateProposalRequest, principalID string) (*domain.PaymentProposal, error) {
	id := uuid.New().String()
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `
			INSERT INTO payment_proposals (`+proposalColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', 0, 0, 0, NULL, NULL, $8, NOW(), NOW())
			RETURNING `+proposalColumns,
			id, strPtrOrNil(tenantID), req.LegalEntityID, req.PayingBankAccountRef, req.Currency, req.PaymentDate,
			req.PaymentMethod, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, id, domain.EventProposalCreated, "", principalID)
	})
	if err != nil {
		s.log.Error("pg CreateProposal failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FindProposal(ctx context.Context, proposalID string) (*domain.PaymentProposal, error) {
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `SELECT `+proposalColumns+` FROM payment_proposals WHERE proposal_id = $1`, proposalID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrProposalNotFound
	}
	if err != nil {
		s.log.Error("pg FindProposal failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// ── items ────────────────────────────────────────────────────────────────────

const itemColumns = `
	item_id, tenant_id, proposal_id, payable_source, payable_id, payee_ref, gross_amount, withholding_amount,
	net_amount, currency, due_date, payee_snapshot_at, tax_determination_id, exception_ref, is_active, created_at`

func scanItem(row pgx.Row) (*domain.ProposalItem, error) {
	i := &domain.ProposalItem{}
	err := row.Scan(&i.ItemID, &i.TenantID, &i.ProposalID, &i.PayableSource, &i.PayableID, &i.PayeeRef, &i.GrossAmount,
		&i.WithholdingAmount, &i.NetAmount, &i.Currency, &i.DueDate, &i.PayeeSnapshotAt, &nullString{&i.TaxDeterminationID},
		&nullString{&i.ExceptionRef}, &i.IsActive, &i.CreatedAt)
	if err != nil {
		return nil, err
	}
	return i, nil
}

// AddItem relies on the migration's BEFORE INSERT trigger to reject an item
// added to a proposal that is no longer composable, and on the partial
// unique index to reject a payable already active on another proposal
// (negative-path scenario #2) — both genuine database invariants.
func (s *PgStore) AddItem(ctx context.Context, item domain.ProposalItem) (*domain.ProposalItem, error) {
	id := uuid.New().String()
	var out *domain.ProposalItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM payment_proposals WHERE proposal_id = $1`, item.ProposalID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrProposalNotFound
			}
			return err
		}
		var err error
		out, err = scanItem(tx.QueryRow(ctx, `
			INSERT INTO proposal_items (`+itemColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, TRUE, NOW())
			RETURNING `+itemColumns,
			id, tenantID, item.ProposalID, item.PayableSource, item.PayableID, item.PayeeRef, item.GrossAmount,
			item.WithholdingAmount, item.NetAmount, item.Currency, item.DueDate, item.PayeeSnapshotAt,
			strPtrOrNil(item.TaxDeterminationID), strPtrOrNil(item.ExceptionRef),
		))
		return err
	})
	if errors.Is(err, domain.ErrProposalNotFound) {
		return nil, domain.ErrProposalNotFound
	}
	if isUniqueViolation(err) {
		return nil, domain.ErrPayableAlreadyInProposal
	}
	if isProposalNotComposable(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg AddItem failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) FindItem(ctx context.Context, itemID string) (*domain.ProposalItem, error) {
	var i *domain.ProposalItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		i, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemColumns+` FROM proposal_items WHERE item_id = $1`, itemID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrItemNotFound
	}
	if err != nil {
		s.log.Error("pg FindItem failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return i, nil
}

// RemoveItem flips is_active to FALSE — see the migration for why this,
// not DELETE, is the removal mechanism, and what it frees up.
func (s *PgStore) RemoveItem(ctx context.Context, itemID string) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE proposal_items SET is_active = FALSE WHERE item_id = $1 AND is_active = TRUE`, itemID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrItemNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrItemNotFound) || isInvalidUUID(err) {
		return domain.ErrItemNotFound
	}
	if err != nil {
		s.log.Error("pg RemoveItem failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) ListItems(ctx context.Context, proposalID string) ([]domain.ProposalItem, error) {
	var out []domain.ProposalItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+itemColumns+` FROM proposal_items WHERE proposal_id = $1 AND is_active = TRUE ORDER BY created_at ASC`, proposalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *i)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListItems failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (s *PgStore) RecalculateProposal(ctx context.Context, proposalID string, gross, withholding, net float64, principalID string) (*domain.PaymentProposal, error) {
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `
			UPDATE payment_proposals SET status = 'CALCULATED', gross_amount = $2, withholding_amount = $3, net_amount = $4, updated_at = NOW()
			WHERE proposal_id = $1 AND status IN ('DRAFT', 'CALCULATED', 'REVIEW')
			RETURNING `+proposalColumns,
			proposalID, gross, withholding, net,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, proposalID, domain.EventProposalCalculated, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RecalculateProposal failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) SubmitForReview(ctx context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error) {
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `
			UPDATE payment_proposals SET status = 'REVIEW', updated_at = NOW()
			WHERE proposal_id = $1 AND status = 'CALCULATED'
			RETURNING `+proposalColumns,
			proposalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, proposalID, domain.EventProposalChanged, "submitted for review", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SubmitForReview failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FreezeProposal(ctx context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error) {
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `
			UPDATE payment_proposals SET status = 'FROZEN', frozen_by_principal_id = $2, frozen_at = NOW(), updated_at = NOW()
			WHERE proposal_id = $1 AND status = 'REVIEW'
			RETURNING `+proposalColumns,
			proposalID, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, proposalID, domain.EventProposalFrozen, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg FreezeProposal failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// CancelProposal cascades is_active = FALSE onto every active item in the
// same transaction as cancelling the proposal itself — this is what frees
// those payables for reselection into a different proposal.
func (s *PgStore) CancelProposal(ctx context.Context, proposalID string, req domain.CancelProposalRequest, principalID string) (*domain.PaymentProposal, error) {
	var p *domain.PaymentProposal
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProposal(tx.QueryRow(ctx, `
			UPDATE payment_proposals SET status = 'CANCELLED', cancel_reason = $2, updated_at = NOW()
			WHERE proposal_id = $1 AND status IN ('DRAFT', 'CALCULATED', 'REVIEW', 'FROZEN')
			RETURNING `+proposalColumns,
			proposalID, req.Reason,
		))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE proposal_items SET is_active = FALSE WHERE proposal_id = $1 AND is_active = TRUE`, proposalID); err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, proposalID, domain.EventProposalCancelled, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CancelProposal failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// ── events ───────────────────────────────────────────────────────────────────

func (s *PgStore) ListEvents(ctx context.Context, proposalID string) ([]domain.ProposalEvent, error) {
	var out []domain.ProposalEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, proposal_id, event_type, detail, actor_principal_id, created_at
			FROM proposal_events WHERE proposal_id = $1 ORDER BY created_at ASC`, proposalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ProposalEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.ProposalID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
