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

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isCommandIdempotencyViolation distinguishes the command_idempotency primary
// key from payee_destinations' own uniqueness indexes. Both are 23505, but
// only the former means two first attempts raced on the same key.
func isCommandIdempotencyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "command_idempotency_pkey"
}

type Store interface {
	// Every command returns replay=true when the same (tenant,
	// Idempotency-Key, command, destination) was already committed. The
	// returned destination is the row as it now stands; no state change or
	// event is produced by a replay.
	ProposeDestination(ctx context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error)
	FindDestination(ctx context.Context, destinationID string) (*domain.PayeeDestination, error)
	ListVersions(ctx context.Context, partyRef string) ([]domain.PayeeDestination, error)
	FindActiveDestination(ctx context.Context, partyRef, scope string) (*domain.PayeeDestination, error)

	VerifyDestination(ctx context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID, idempotencyKey string) (*domain.PayeeDestination, bool, error)
	ApproveDestination(ctx context.Context, destinationID, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error)
	ActivateDestination(ctx context.Context, destinationID, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error)
	SuspendDestination(ctx context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error)
	SupersedeDestination(ctx context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error)

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

// findCommandClaim reads the one idempotency claim for this tenant/key. It
// deliberately returns the target as text: ProposeDestination has no target
// before it inserts, so the caller decides whether a found claim is a replay
// or a conflict.
func (s *PgStore) findCommandClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey string) (command, destinationID string, found bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT command, destination_id::text
		FROM command_idempotency
		WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, idempotencyKey,
	).Scan(&command, &destinationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return command, destinationID, true, nil
}

func (s *PgStore) recordCommandClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey, command, destinationID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO command_idempotency (tenant_id, idempotency_key, command, destination_id)
		VALUES ($1, $2, $3, $4)`,
		tenantID, idempotencyKey, command, destinationID,
	)
	return err
}

// lockDestination takes the row lock before the idempotency lookup. Two
// concurrent retries of the same command on the same destination then
// serialize: the second one sees the winning claim and replays instead of
// failing the old state guard.
func (s *PgStore) lockDestination(ctx context.Context, tx pgx.Tx, destinationID string) (*domain.PayeeDestination, error) {
	d, err := scanDestination(tx.QueryRow(ctx,
		`SELECT `+destColumns+` FROM payee_destinations WHERE destination_id = $1 FOR UPDATE`, destinationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidTransition
	}
	return d, err
}

func proposalMatches(d *domain.PayeeDestination, req domain.ProposeDestinationRequest) bool {
	scope := req.Scope
	if scope == "" {
		scope = "DEFAULT"
	}
	return d.LegalEntityID == req.LegalEntityID &&
		d.PartyRef == req.PartyRef &&
		d.Scope == scope &&
		d.FinancialInstitution == req.FinancialInstitution &&
		d.AccountIdentifier == req.AccountIdentifier &&
		d.CountryCode == req.CountryCode &&
		d.Currency == req.Currency &&
		d.PayeeName == req.PayeeName &&
		d.SourceType == req.SourceType
}

// tryReplayProposal is the concurrent-first-attempt path: the losing INSERT
// rolled back its whole transaction (including its destination), so re-read
// the winner's claim in a fresh transaction and return the candidate it
// created. found=false means there is no claim at all, which lets the caller
// distinguish a genuine duplicate destination from a lost retry race.
func (s *PgStore) tryReplayProposal(ctx context.Context, tenantID, idempotencyKey string, req domain.ProposeDestinationRequest) (*domain.PayeeDestination, bool, error) {
	var out *domain.PayeeDestination
	foundClaim := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, tenantID, idempotencyKey)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		foundClaim = true
		if command != domain.CommandProposeDestination {
			return domain.ErrIdempotencyKeyConflict
		}
		existing, err := scanDestination(tx.QueryRow(ctx,
			`SELECT `+destColumns+` FROM payee_destinations WHERE destination_id = $1`, claimedDest))
		if err != nil {
			return err
		}
		if !proposalMatches(existing, req) {
			return domain.ErrIdempotencyKeyConflict
		}
		out = existing
		return nil
	})
	return out, foundClaim, err
}

// replayProposal is the command_idempotency primary-key race path, where a
// claim is guaranteed to exist.
func (s *PgStore) replayProposal(ctx context.Context, tenantID, idempotencyKey string, req domain.ProposeDestinationRequest) (*domain.PayeeDestination, error) {
	existing, found, err := s.tryReplayProposal(ctx, tenantID, idempotencyKey, req)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, domain.ErrIdempotencyKeyConflict
	}
	return existing, nil
}

func (s *PgStore) ProposeDestination(ctx context.Context, tenantID string, req domain.ProposeDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	destinationID := uuid.New().String()
	fingerprint := domain.Fingerprint(req.FinancialInstitution, req.AccountIdentifier, req.Currency)
	last4 := domain.Last4(req.AccountIdentifier)
	scope := req.Scope
	if scope == "" {
		scope = "DEFAULT"
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, tenantID, idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandProposeDestination {
				return domain.ErrIdempotencyKeyConflict
			}
			existing, err := scanDestination(tx.QueryRow(ctx,
				`SELECT `+destColumns+` FROM payee_destinations WHERE destination_id = $1`, claimedDest))
			if err != nil {
				return err
			}
			if !proposalMatches(existing, req) {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = existing, true
			return nil
		}

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
		// The safe projection, never the full PayeeDestination (which carries
		// the unmasked AccountIdentifier) — same doctrine as maskIfNotPrivileged
		// at the HTTP layer, applied here because this is the one place the
		// full account identifier could otherwise leak into Kafka.
		if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.proposed", domain.EventPayeeDestinationProposed,
			"payee-destination", destinationID, tenantID, req.LegalEntityID, principalID, correlationID,
			domain.NewDestinationEventPayload(d, tenantID)); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, tenantID, idempotencyKey, domain.CommandProposeDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		existing, replayErr := s.replayProposal(ctx, tenantID, idempotencyKey, req)
		if replayErr != nil {
			return nil, false, replayErr
		}
		return existing, true, nil
	}
	if isUniqueViolation(err) {
		// The losing side of a concurrent same-key proposal can hit the
		// destination fingerprint index before it ever reaches the
		// command_idempotency insert: the winner's uncommitted destination
		// blocks it, then the winner commits. If this key now has a matching
		// claim, this was a retry race and must replay, not report a
		// duplicate destination. A genuine duplicate with a fresh key has no
		// claim and still returns ErrDuplicateDestination.
		existing, found, replayErr := s.tryReplayProposal(ctx, tenantID, idempotencyKey, req)
		if replayErr != nil {
			return nil, false, replayErr
		}
		if found {
			return existing, true, nil
		}
		return nil, false, domain.ErrDuplicateDestination
	}
	if err != nil {
		s.log.Error("pg ProposeDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
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

func (s *PgStore) VerifyDestination(ctx context.Context, destinationID string, req domain.VerifyDestinationRequest, principalID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := s.lockDestination(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandVerifyDestination || claimedDest != destinationID {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = current, true
			return nil
		}

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
		if err := s.recordEvent(ctx, tx, d.TenantID, destinationID, domain.EventPayeeDestinationVerified, req.VerificationMethod, principalID); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey, domain.CommandVerifyDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		return nil, false, domain.ErrIdempotencyKeyConflict
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || errors.Is(err, domain.ErrInvalidTransition) {
		return nil, false, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg VerifyDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
}

func (s *PgStore) ApproveDestination(ctx context.Context, destinationID, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := s.lockDestination(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandApproveDestination || claimedDest != destinationID {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = current, true
			return nil
		}

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
		// Safe projection — see ProposeDestination's enqueueEvent call for why.
		if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.approved", domain.EventPayeeDestinationApproved,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID,
			domain.NewDestinationEventPayload(d, strOrEmpty(d.TenantID))); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey, domain.CommandApproveDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		return nil, false, domain.ErrIdempotencyKeyConflict
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || errors.Is(err, domain.ErrInvalidTransition) {
		return nil, false, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ApproveDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
}

// ActivateDestination is where "only one active version per party/scope"
// is enforced for real: activating this destination, in the same
// transaction, supersedes whatever destination (if any) is currently
// ACTIVE for the same party/scope — the database's own partial unique
// index is the second, structural layer of the same guarantee.
func (s *PgStore) ActivateDestination(ctx context.Context, destinationID, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := s.lockDestination(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandActivateDestination || claimedDest != destinationID {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = current, true
			return nil
		}
		if current.Status != domain.StatusApprovalPending {
			return domain.ErrInvalidTransition
		}

		var oldActive *domain.PayeeDestination
		oldActive, err = scanDestination(tx.QueryRow(ctx, `
			SELECT `+destColumns+` FROM payee_destinations
			WHERE party_ref = $1 AND scope = $2 AND status = 'ACTIVE'
			FOR UPDATE`, current.PartyRef, current.Scope))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if oldActive != nil {
			oldActive, err = scanDestination(tx.QueryRow(ctx, `
				UPDATE payee_destinations
				SET status = 'SUPERSEDED', superseded_by_destination_id = $1, updated_at = NOW()
				WHERE destination_id = $2
				RETURNING `+destColumns, destinationID, oldActive.DestinationID))
			if err != nil {
				return err
			}
			if err := s.recordEvent(ctx, tx, oldActive.TenantID, oldActive.DestinationID, domain.EventPayeeDestinationSuperseded, "superseded by "+destinationID, principalID); err != nil {
				return err
			}
			// "Any active-destination change emits an immediate high-risk
			// event": activation changing the party/scope's active row is
			// such a change, not only an explicit SupersedeDestination.
			// Safe projection — see ProposeDestination's enqueueEvent call.
			if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.superseded", domain.EventPayeeDestinationSuperseded,
				"payee-destination", oldActive.DestinationID, strOrEmpty(oldActive.TenantID), oldActive.LegalEntityID, principalID, correlationID,
				domain.NewDestinationEventPayload(oldActive, strOrEmpty(oldActive.TenantID))); err != nil {
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
		if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.activated", domain.EventPayeeDestinationActivated,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID,
			domain.NewDestinationEventPayload(d, strOrEmpty(d.TenantID))); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey, domain.CommandActivateDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		return nil, false, domain.ErrIdempotencyKeyConflict
	}
	if errors.Is(err, domain.ErrInvalidTransition) || isInvalidUUID(err) {
		return nil, false, domain.ErrInvalidTransition
	}
	if isUniqueViolation(err) {
		return nil, false, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ActivateDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
}

func (s *PgStore) SuspendDestination(ctx context.Context, destinationID string, req domain.SuspendDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := s.lockDestination(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandSuspendDestination || claimedDest != destinationID {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = current, true
			return nil
		}

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
		// Safe projection — see ProposeDestination's enqueueEvent call for why.
		if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.suspended", domain.EventPayeeDestinationSuspended,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID,
			domain.NewDestinationEventPayload(d, strOrEmpty(d.TenantID))); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey, domain.CommandSuspendDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		return nil, false, domain.ErrIdempotencyKeyConflict
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || errors.Is(err, domain.ErrInvalidTransition) {
		return nil, false, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SuspendDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
}

func (s *PgStore) SupersedeDestination(ctx context.Context, destinationID string, req domain.SupersedeDestinationRequest, principalID, correlationID, idempotencyKey string) (*domain.PayeeDestination, bool, error) {
	if idempotencyKey == "" {
		return nil, false, domain.ErrIdempotencyKeyRequired
	}
	var d *domain.PayeeDestination
	replay := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := s.lockDestination(ctx, tx, destinationID)
		if err != nil {
			return err
		}
		command, claimedDest, found, err := s.findCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if command != domain.CommandSupersedeDestination || claimedDest != destinationID {
				return domain.ErrIdempotencyKeyConflict
			}
			d, replay = current, true
			return nil
		}

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
		// Safe projection — see ProposeDestination's enqueueEvent call for why.
		if err := s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payee-destination.superseded", domain.EventPayeeDestinationSuperseded,
			"payee-destination", destinationID, strOrEmpty(d.TenantID), d.LegalEntityID, principalID, correlationID,
			domain.NewDestinationEventPayload(d, strOrEmpty(d.TenantID))); err != nil {
			return err
		}
		return s.recordCommandClaim(ctx, tx, middleware.TenantFromContext(ctx), idempotencyKey, domain.CommandSupersedeDestination, destinationID)
	})
	if errors.Is(err, domain.ErrIdempotencyKeyConflict) || errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		return nil, false, err
	}
	if isCommandIdempotencyViolation(err) {
		return nil, false, domain.ErrIdempotencyKeyConflict
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || errors.Is(err, domain.ErrInvalidTransition) {
		return nil, false, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SupersedeDestination failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, replay, nil
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
