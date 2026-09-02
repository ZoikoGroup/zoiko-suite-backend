// Package store implements supplier-financial-profile-svc's persistence.
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

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02) — see the privacy-domain
// services' identical helper (added after live-stack testing caught a
// malformed ID surfacing as a false "store unavailable" instead of "not
// found") for the full rationale. Applied from the start here rather
// than discovered the same way twice.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isExclusionViolation reports whether err is Postgres's exclusion-
// constraint violation (SQLSTATE 23P01) — what fires when a new
// PaymentTermsPeriod's effective range overlaps an existing one for the
// same profile (see the migration's EXCLUDE constraint).
func isExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer)
// string field — see the privacy-domain services' identical helper.
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
	CreateProfile(ctx context.Context, tenantID string, req domain.CreateProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error)
	FindProfile(ctx context.Context, profileID string) (*domain.SupplierFinancialProfile, error)
	ListProfiles(ctx context.Context) ([]domain.SupplierFinancialProfile, error)
	ActivateProfile(ctx context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error)
	AmendProfile(ctx context.Context, profileID string, req domain.AmendProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error)
	PlaceHold(ctx context.Context, profileID string, req domain.PlaceHoldRequest, principalID string) (*domain.SupplierFinancialProfile, error)
	ReleaseHold(ctx context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error)
	RetireProfile(ctx context.Context, profileID string, req domain.RetireProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error)

	ChangePaymentTerms(ctx context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string) (*domain.PaymentTermsPeriod, error)
	ListPaymentTerms(ctx context.Context, profileID string) ([]domain.PaymentTermsPeriod, error)

	ProposeHighRiskChange(ctx context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, error)
	FindChangeRequest(ctx context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error)
	DecideHighRiskChange(ctx context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error)

	ListChangeEvents(ctx context.Context, profileID string) ([]domain.ProfileChangeEvent, error)
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

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, profileID, eventType, prior, new, reason, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO profile_change_events (event_id, tenant_id, profile_id, event_type, prior_value, new_value, reason, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New().String(), tenantID, profileID, eventType, strPtrOrNil(prior), strPtrOrNil(new), strPtrOrNil(reason), actorPrincipalID,
	)
	return err
}

// ── profiles ─────────────────────────────────────────────────────────────────

const profileColumns = `
	profile_id, tenant_id, legal_entity_id, supplier_ref, status, payee_reference, category,
	invoice_channel, payment_method_preference, tax_withholding_ref, hold_reason,
	created_at, created_by_principal_id, updated_at`

func scanProfile(row pgx.Row) (*domain.SupplierFinancialProfile, error) {
	p := &domain.SupplierFinancialProfile{}
	err := row.Scan(&p.ProfileID, &p.TenantID, &p.LegalEntityID, &p.SupplierRef, &p.Status, &nullString{&p.PayeeReference},
		&nullString{&p.Category}, &nullString{&p.InvoiceChannel}, &nullString{&p.PaymentMethodPreference},
		&nullString{&p.TaxWithholdingRef}, &nullString{&p.HoldReason}, &p.CreatedAt, &p.CreatedByPrincipalID, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) CreateProfile(ctx context.Context, tenantID string, req domain.CreateProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	id := uuid.New().String()
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			INSERT INTO supplier_financial_profiles (`+profileColumns+`)
			VALUES ($1, $2, $3, $4, 'DRAFT', NULL, $5, $6, NULL, NULL, NULL, NOW(), $7, NOW())
			RETURNING `+profileColumns,
			id, strPtrOrNil(tenantID), req.LegalEntityID, req.SupplierRef, strPtrOrNil(req.Category), strPtrOrNil(req.InvoiceChannel), principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, id, domain.EventProfileCreated, "", req.SupplierRef, "", principalID)
	})
	if err != nil {
		s.log.Error("pg CreateProfile failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FindProfile(ctx context.Context, profileID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles WHERE profile_id = $1`, profileID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		s.log.Error("pg FindProfile failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ListProfiles(ctx context.Context) ([]domain.SupplierFinancialProfile, error) {
	var out []domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListProfiles failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ActivateProfile(ctx context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE supplier_financial_profiles SET status = 'ACTIVE', updated_at = NOW()
			WHERE profile_id = $1 AND status = 'DRAFT'
			RETURNING `+profileColumns,
			profileID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, profileID, domain.EventProfileActivated, "DRAFT", "ACTIVE", "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ActivateProfile failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) AmendProfile(ctx context.Context, profileID string, req domain.AmendProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE supplier_financial_profiles SET
				category = COALESCE($2, category),
				invoice_channel = COALESCE($3, invoice_channel),
				updated_at = NOW()
			WHERE profile_id = $1
			RETURNING `+profileColumns,
			profileID, req.Category, req.InvoiceChannel,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, profileID, domain.EventProfileAmended, "", "", req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		s.log.Error("pg AmendProfile failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) PlaceHold(ctx context.Context, profileID string, req domain.PlaceHoldRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE supplier_financial_profiles SET status = 'ON_HOLD', hold_reason = $2, updated_at = NOW()
			WHERE profile_id = $1 AND status = 'ACTIVE'
			RETURNING `+profileColumns,
			profileID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, profileID, domain.EventHoldPlaced, "ACTIVE", "ON_HOLD", req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg PlaceHold failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ReleaseHold(ctx context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE supplier_financial_profiles SET status = 'ACTIVE', hold_reason = NULL, updated_at = NOW()
			WHERE profile_id = $1 AND status = 'ON_HOLD'
			RETURNING `+profileColumns,
			profileID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, profileID, domain.EventHoldReleased, "ON_HOLD", "ACTIVE", "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ReleaseHold failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) RetireProfile(ctx context.Context, profileID string, req domain.RetireProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE supplier_financial_profiles SET status = 'RETIRED', updated_at = NOW()
			WHERE profile_id = $1 AND status != 'RETIRED'
			RETURNING `+profileColumns,
			profileID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, profileID, domain.EventProfileRetired, "", "RETIRED", req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RetireProfile failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// ── payment terms ────────────────────────────────────────────────────────────

// ChangePaymentTerms relies on the migration's EXCLUDE constraint to
// reject an overlapping effective period atomically — see
// isExclusionViolation. This is a genuine database-enforced invariant,
// not an application-level date check a race could slip past.
func (s *PgStore) ChangePaymentTerms(ctx context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string) (*domain.PaymentTermsPeriod, error) {
	id := uuid.New().String()
	var t domain.PaymentTermsPeriod
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM supplier_financial_profiles WHERE profile_id = $1`, profileID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrProfileNotFound
			}
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO payment_terms_periods (payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7)
			RETURNING payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id`,
			id, tenantID, profileID, req.TermsCode, req.EffectiveFrom, req.EffectiveTo, principalID,
		).Scan(&t.PaymentTermsID, &t.TenantID, &t.ProfileID, &t.TermsCode, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedAt, &t.CreatedByPrincipalID); err != nil {
			if isExclusionViolation(err) {
				return domain.ErrOverlappingPaymentTerms
			}
			return err
		}

		return s.recordEvent(ctx, tx, tenantID, profileID, domain.EventPaymentTermsChanged, "", req.TermsCode, "", principalID)
	})
	if errors.Is(err, domain.ErrProfileNotFound) || errors.Is(err, domain.ErrOverlappingPaymentTerms) {
		return nil, err
	}
	if err != nil {
		s.log.Error("pg ChangePaymentTerms failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &t, nil
}

func (s *PgStore) ListPaymentTerms(ctx context.Context, profileID string) ([]domain.PaymentTermsPeriod, error) {
	var out []domain.PaymentTermsPeriod
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id
			FROM payment_terms_periods WHERE profile_id = $1 ORDER BY effective_from ASC`, profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.PaymentTermsPeriod
			if err := rows.Scan(&t.PaymentTermsID, &t.TenantID, &t.ProfileID, &t.TermsCode, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedAt, &t.CreatedByPrincipalID); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListPaymentTerms failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── high-risk change requests ────────────────────────────────────────────────

const changeRequestColumns = `
	change_request_id, tenant_id, profile_id, field, old_value, new_value, reason, status,
	proposed_by_principal_id, proposed_at, decided_by_principal_id, decided_at, decision_reason`

func scanChangeRequest(row pgx.Row) (*domain.HighRiskChangeRequest, error) {
	c := &domain.HighRiskChangeRequest{}
	err := row.Scan(&c.ChangeRequestID, &c.TenantID, &c.ProfileID, &c.Field, &nullString{&c.OldValue}, &c.NewValue,
		&nullString{&c.Reason}, &c.Status, &c.ProposedByPrincipalID, &c.ProposedAt, &c.DecidedByPrincipalID, &c.DecidedAt, &nullString{&c.DecisionReason})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) ProposeHighRiskChange(ctx context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, error) {
	id := uuid.New().String()
	var c *domain.HighRiskChangeRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var oldValue *string
		var column string
		switch req.Field {
		case domain.FieldPayeeReference:
			column = "payee_reference"
		case domain.FieldPaymentMethodPreference:
			column = "payment_method_preference"
		}
		if err := tx.QueryRow(ctx, `SELECT tenant_id, `+column+` FROM supplier_financial_profiles WHERE profile_id = $1`, profileID).Scan(&tenantID, &oldValue); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrProfileNotFound
			}
			return err
		}
		old := ""
		if oldValue != nil {
			old = *oldValue
		}

		var err error
		c, err = scanChangeRequest(tx.QueryRow(ctx, `
			INSERT INTO high_risk_change_requests (`+changeRequestColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING_APPROVAL', $8, NOW(), NULL, NULL, NULL)
			RETURNING `+changeRequestColumns,
			id, tenantID, profileID, req.Field, strPtrOrNil(old), req.NewValue, strPtrOrNil(req.Reason), principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, tenantID, profileID, domain.EventHighRiskProposed, old, req.NewValue, req.Reason, principalID)
	})
	if errors.Is(err, domain.ErrProfileNotFound) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		s.log.Error("pg ProposeHighRiskChange failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) FindChangeRequest(ctx context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error) {
	var c *domain.HighRiskChangeRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanChangeRequest(tx.QueryRow(ctx, `SELECT `+changeRequestColumns+` FROM high_risk_change_requests WHERE change_request_id = $1`, changeRequestID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrChangeRequestNotFound
	}
	if err != nil {
		s.log.Error("pg FindChangeRequest failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// DecideHighRiskChange applies or rejects a pending change atomically.
// The caller (handler) is responsible for the authorization-svc own-
// object SoD check BEFORE calling this — this method enforces only that
// the request is still PENDING_APPROVAL, not who may decide it.
func (s *PgStore) DecideHighRiskChange(ctx context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error) {
	var c *domain.HighRiskChangeRequest
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		newStatus := domain.ChangeRequestRejected
		eventType := domain.EventHighRiskRejected
		if req.Approve {
			newStatus = domain.ChangeRequestApproved
			eventType = domain.EventHighRiskApplied
		}

		var err error
		c, err = scanChangeRequest(tx.QueryRow(ctx, `
			UPDATE high_risk_change_requests SET
				status = $2, decided_by_principal_id = $3, decided_at = NOW(), decision_reason = $4
			WHERE change_request_id = $1 AND status = 'PENDING_APPROVAL'
			RETURNING `+changeRequestColumns,
			changeRequestID, newStatus, principalID, strPtrOrNil(req.Reason),
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrChangeRequestNotPending
			}
			return err
		}

		if req.Approve {
			var column string
			switch c.Field {
			case domain.FieldPayeeReference:
				column = "payee_reference"
			case domain.FieldPaymentMethodPreference:
				column = "payment_method_preference"
			}
			p, err = scanProfile(tx.QueryRow(ctx, `
				UPDATE supplier_financial_profiles SET `+column+` = $2, updated_at = NOW()
				WHERE profile_id = $1
				RETURNING `+profileColumns,
				c.ProfileID, c.NewValue,
			))
			if err != nil {
				return err
			}
		} else {
			p, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles WHERE profile_id = $1`, c.ProfileID))
			if err != nil {
				return err
			}
		}

		return s.recordEvent(ctx, tx, c.TenantID, c.ProfileID, eventType, c.OldValue, c.NewValue, req.Reason, principalID)
	})
	if errors.Is(err, domain.ErrChangeRequestNotPending) {
		return nil, nil, domain.ErrChangeRequestNotPending
	}
	if err != nil {
		s.log.Error("pg DecideHighRiskChange failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, p, nil
}

// ── change events ────────────────────────────────────────────────────────────

func (s *PgStore) ListChangeEvents(ctx context.Context, profileID string) ([]domain.ProfileChangeEvent, error) {
	var out []domain.ProfileChangeEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, profile_id, event_type, prior_value, new_value, reason, actor_principal_id, created_at
			FROM profile_change_events WHERE profile_id = $1 ORDER BY created_at ASC`, profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ProfileChangeEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.ProfileID, &e.EventType, &nullString{&e.PriorValue}, &nullString{&e.NewValue}, &nullString{&e.Reason}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListChangeEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
