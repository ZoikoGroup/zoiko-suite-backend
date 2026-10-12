// Package store is the Postgres persistence layer for
// payee-banking-identity-svc.
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

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
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
	ProposeDestination(ctx context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error)
	FindDestination(ctx context.Context, destinationID string) (*domain.PayeeDestination, error)
	ListVersions(ctx context.Context, partyRef string) ([]domain.PayeeDestination, error)
	FindActiveDestination(ctx context.Context, partyRef, scope string) (*domain.PayeeDestination, error)

	VerifyDestination(ctx context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID string) (*domain.PayeeDestination, error)
	ApproveDestination(ctx context.Context, destinationID, principalID, correlationID string) (*domain.PayeeDestination, error)
	ActivateDestination(ctx context.Context, destinationID, principalID, correlationID string) (*domain.PayeeDestination, error)
	SuspendDestination(ctx context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error)
	SupersedeDestination(ctx context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error)

	ListEvents(ctx context.Context, destinationID string) ([]domain.ChangeEvent, error)

	// QueryDestinationChanges serves the destination-changes control population.
	QueryDestinationChanges(ctx context.Context, tenantID string, q domain.DestinationChangesQuery) (*domain.ControlPopulationPage, error)
}

type PgStore struct {
	pool        *pgxpool.Pool
	log         *zap.Logger
	eventRegion string
}

// Option configures a PgStore at construction.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion — required, a controlled
// lower-case code such as "uk" or "eu-west").
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger, opts ...Option) *PgStore {
	s := &PgStore{pool: pool, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// enqueueEvent builds and enqueues one event inside the caller's own
// transaction — the entire point of the transactional outbox (ZS-EVENT-001
// §6): the event and the state change it reports commit atomically, or
// neither does. legacyType is the pre-standard event_type string existing
// consumers filter on; always pass it explicitly — envelope.New silently
// defaults it to the canonical type when empty, which would silently break
// any consumer still reading the old name.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, canonicalType, legacyType, aggregateType, aggregateID, tenantID, legalEntityID, actorID, correlationID string, data any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            canonicalType,
		LegacyType:      legacyType,
		Service:         "payee-banking-identity-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   aggregateType,
		AggregateID:     aggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", canonicalType, err)
	}
	return nil
}

// strOrEmpty dereferences a nullable tenant_id for use as a plain string in
// an event envelope, which requires a non-empty TenantID.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

const destColumns = `
	destination_id, tenant_id, legal_entity_id, party_ref, scope, financial_institution,
	account_identifier, account_last4, country_code, currency, payee_name, source_type, fingerprint, status,
	verification_method, verification_evidence_ref, verified_by_principal_id, verified_at,
	approved_by_principal_id, approved_at, superseded_by_destination_id, suspend_reason,
	proposed_by_principal_id, created_at, updated_at`

func scanDestination(row pgx.Row) (*domain.PayeeDestination, error) {
	d := &domain.PayeeDestination{}
	var supersededBy *string
	err := row.Scan(&d.DestinationID, &d.TenantID, &d.LegalEntityID, &d.PartyRef, &d.Scope, &d.FinancialInstitution,
		&d.AccountIdentifier, &d.AccountLast4, &d.CountryCode, &d.Currency, &d.PayeeName, &d.SourceType, &d.Fingerprint, &d.Status,
		&d.VerificationMethod, &d.VerificationEvidenceRef, &d.VerifiedByPrincipalID, &d.VerifiedAt,
		&d.ApprovedByPrincipalID, &d.ApprovedAt, &supersededBy, &d.SuspendReason,
		&d.ProposedByPrincipalID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if supersededBy != nil {
		d.SupersededByDestinationID = *supersededBy
	}
	return d, nil
}

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, destinationID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payee_destination_events (event_id, tenant_id, destination_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, destinationID, eventType, detail, actorPrincipalID,
	)
	return err
}

func (s *PgStore) ProposeDestination(ctx context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error) {
	destinationID := uuid.New().String()
	fingerprint := domain.Fingerprint(req.FinancialInstitution, req.AccountIdentifier, req.Currency)
	last4 := domain.Last4(req.AccountIdentifier)
	scope := req.Scope
	if scope == "" {
		scope = "DEFAULT"
	}
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `
			INSERT INTO payee_destinations (`+destColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'CANDIDATE',
				'', '', '', NULL, '', NULL, NULL, '', $14, NOW(), NOW())
			RETURNING `+destColumns,
			destinationID, nullableTenant(tenantID), req.LegalEntityID, req.PartyRef, scope, req.FinancialInstitution,
			req.AccountIdentifier, last4, req.CountryCode, req.Currency, req.PayeeName, req.SourceType, fingerprint,
			principalID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationProposed, "", principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.proposed", domain.EventPayeeDestinationProposed,
			"payee-destination", destinationID, tenantID, req.LegalEntityID, principalID, correlationID, d)
	})
	if isUniqueViolation(err) {
		return nil, domain.ErrDuplicateDestination
	}
	if err != nil {
		s.log.Error("pg ProposeDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) FindDestination(ctx context.Context, destinationID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `SELECT `+destColumns+` FROM payee_destinations WHERE destination_id = $1`, destinationID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrDestinationNotFound
	}
	if err != nil {
		s.log.Error("pg FindDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) ListVersions(ctx context.Context, partyRef string) ([]domain.PayeeDestination, error) {
	var out []domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+destColumns+` FROM payee_destinations WHERE party_ref = $1 ORDER BY created_at ASC`, partyRef)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDestination(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListVersions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) FindActiveDestination(ctx context.Context, partyRef, scope string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `SELECT `+destColumns+` FROM payee_destinations WHERE party_ref = $1 AND scope = $2 AND status = 'ACTIVE'`, partyRef, scope))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoActiveDestination
	}
	if err != nil {
		s.log.Error("pg FindActiveDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) VerifyDestination(ctx context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `
			UPDATE payee_destinations SET status = 'VERIFIED', verification_method = $1, verification_evidence_ref = $2,
				verified_by_principal_id = $3, verified_at = NOW(), updated_at = NOW()
			WHERE destination_id = $4 AND status IN ('CANDIDATE', 'VERIFICATION_PENDING')
			RETURNING `+destColumns,
			req.VerificationMethod, req.VerificationEvidenceRef, principalID, destinationID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationVerified, req.VerificationMethod, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg VerifyDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) ApproveDestination(ctx context.Context, destinationID, principalID, correlationID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `
			UPDATE payee_destinations SET status = 'APPROVAL_PENDING', approved_by_principal_id = $1, approved_at = NOW(), updated_at = NOW()
			WHERE destination_id = $2 AND status = 'VERIFIED'
			RETURNING `+destColumns,
			principalID, destinationID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationApproved, "", principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.approved", domain.EventPayeeDestinationApproved,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID, d)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ApproveDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// ActivateDestination is where "only one active version per party/scope"
// is enforced for real: activating this destination, in the same
// transaction, supersedes whatever destination (if any) is currently
// ACTIVE for the same party/scope — the database's own partial unique
// index is the second, structural layer of the same guarantee.
func (s *PgStore) ActivateDestination(ctx context.Context, destinationID, principalID, correlationID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var partyRef, scope string
		if err := tx.QueryRow(ctx, `SELECT party_ref, scope FROM payee_destinations WHERE destination_id = $1 AND status = 'APPROVAL_PENDING'`, destinationID).
			Scan(&partyRef, &scope); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrInvalidTransition
			}
			return err
		}

		var oldActiveID *string
		err := tx.QueryRow(ctx, `SELECT destination_id FROM payee_destinations WHERE party_ref = $1 AND scope = $2 AND status = 'ACTIVE'`, partyRef, scope).Scan(&oldActiveID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if oldActiveID != nil {
			if _, err := tx.Exec(ctx, `UPDATE payee_destinations SET status = 'SUPERSEDED', superseded_by_destination_id = $1, updated_at = NOW() WHERE destination_id = $2`,
				destinationID, *oldActiveID); err != nil {
				return err
			}
			if err := s.recordEvent(ctx, tx, nil, *oldActiveID, domain.EventPayeeDestinationSuperseded, "superseded by "+destinationID, principalID); err != nil {
				return err
			}
		}

		d, err = scanDestination(tx.QueryRow(ctx, `
			UPDATE payee_destinations SET status = 'ACTIVE', updated_at = NOW()
			WHERE destination_id = $1
			RETURNING `+destColumns,
			destinationID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationActivated, "", principalID); err != nil {
			return err
		}
		// The old destination auto-superseded above (if any) is recorded
		// to the internal audit log (recordEvent) but was never published
		// by the old dual-write — only the newly-activated destination's
		// own event was. Not expanding scope here: only the activated
		// event is enqueued, matching the exact prior behavior.
		return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.activated", domain.EventPayeeDestinationActivated,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID, d)
	})
	if errors.Is(err, domain.ErrInvalidTransition) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if isUniqueViolation(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ActivateDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) SuspendDestination(ctx context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `
			UPDATE payee_destinations SET status = 'SUSPENDED', suspend_reason = $1, updated_at = NOW()
			WHERE destination_id = $2 AND status = 'ACTIVE'
			RETURNING `+destColumns,
			req.Reason, destinationID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationSuspended, req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.suspended", domain.EventPayeeDestinationSuspended,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID, d)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SuspendDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) SupersedeDestination(ctx context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID, correlationID string) (*domain.PayeeDestination, error) {
	var d *domain.PayeeDestination
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDestination(tx.QueryRow(ctx, `
			UPDATE payee_destinations SET status = 'SUPERSEDED', updated_at = NOW()
			WHERE destination_id = $1 AND status IN ('ACTIVE', 'APPROVAL_PENDING', 'VERIFIED')
			RETURNING `+destColumns,
			destinationID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationSuperseded, req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.superseded", domain.EventPayeeDestinationSuperseded,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID, d)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SupersedeDestination failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) ListEvents(ctx context.Context, destinationID string) ([]domain.ChangeEvent, error) {
	var out []domain.ChangeEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, destination_id, event_type, detail, actor_principal_id, created_at
			FROM payee_destination_events WHERE destination_id = $1 ORDER BY created_at ASC`, destinationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ChangeEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.DestinationID, &e.EventType, &e.Detail, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
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
