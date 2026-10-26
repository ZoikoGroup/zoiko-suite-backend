package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/providers"
)

var _ providers.Store = (*PgStore)(nil)

// withScope runs fn in a transaction scoped to an explicit tenant (platform reference data, the sweeper).
func (s *PgStore) withScope(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	return s.withTenant(middleware.WithTenant(ctx, tenant), fn)
}

func unavailable(s *PgStore, op string, err error) error {
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func pgConstraint(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

// ── provider profiles ───────────────────────────────────────────────────────

const profileCols = `provider_key, version, kind, status, spec, spec_hash, created_by, COALESCE(certified_by, ''), certified_at, created_at`

func scanProfile(r pgx.Row) (*providers.ProfileRecord, error) {
	var p providers.ProfileRecord
	var raw []byte
	if err := r.Scan(&p.ProviderKey, &p.Version, &p.Kind, &p.Status, &raw, &p.SpecHash, &p.CreatedBy, &p.CertifiedBy, &p.CertifiedAt, &p.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p.Spec); err != nil {
		return nil, fmt.Errorf("stored provider profile spec: %w", err)
	}
	return &p, nil
}

func (s *PgStore) PutProfile(ctx context.Context, key string, spec adapter.Spec, createdBy string) (*providers.ProfileRecord, error) {
	spec = spec.Normalize()
	raw, _ := json.Marshal(spec)
	var out *providers.ProfileRecord
	err := s.withScope(ctx, providers.PlatformScopeID, func(tx pgx.Tx) error {
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM provider_profiles WHERE provider_key = $1`, key).Scan(&next); err != nil {
			return err
		}
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			INSERT INTO provider_profiles (provider_key, version, kind, status, spec, spec_hash, created_by)
			VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6) RETURNING `+profileCols,
			key, next, spec.Kind, raw, spec.Hash(), createdBy))
		return err
	})
	if pgCode(err) == "23505" {
		return nil, providers.ErrProfileExists
	}
	if err != nil {
		return nil, unavailable(s, "PutProfile", err)
	}
	return out, nil
}

func (s *PgStore) GetProfile(ctx context.Context, key string, version int) (*providers.ProfileRecord, error) {
	var out *providers.ProfileRecord
	err := s.withScope(ctx, providers.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		if version == 0 {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM provider_profiles
				WHERE provider_key = $1 AND status = 'CERTIFIED' ORDER BY version DESC LIMIT 1`, key))
		} else {
			out, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileCols+` FROM provider_profiles WHERE provider_key = $1 AND version = $2`, key, version))
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, providers.ErrProfileNotFound
	}
	if err != nil {
		return nil, unavailable(s, "GetProfile", err)
	}
	return out, nil
}

func (s *PgStore) CertifyProfile(ctx context.Context, key string, version int, by string) (*providers.ProfileRecord, error) {
	var out *providers.ProfileRecord
	err := s.withScope(ctx, providers.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `
			UPDATE provider_profiles SET status = 'CERTIFIED', certified_by = $3, certified_at = now()
			WHERE provider_key = $1 AND version = $2 AND status = 'DRAFT' RETURNING `+profileCols, key, version, by))
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, providers.ErrProfileState
	case strings.Contains(pgConstraint(err), "provider_profile_certified_by_other"):
		return nil, providers.ErrProfileSelfCertify
	case err != nil:
		return nil, unavailable(s, "CertifyProfile", err)
	}
	return out, nil
}

func (s *PgStore) SetProfileStatus(ctx context.Context, key string, version int, to string) (*providers.ProfileRecord, error) {
	var out *providers.ProfileRecord
	err := s.withScope(ctx, providers.PlatformScopeID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProfile(tx.QueryRow(ctx, `UPDATE provider_profiles SET status = $3 WHERE provider_key = $1 AND version = $2 RETURNING `+profileCols, key, version, to))
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, providers.ErrProfileNotFound
	case pgCode(err) == "23000": // integrity_constraint_violation: an illegal status move
		return nil, providers.ErrProfileState
	case err != nil:
		return nil, unavailable(s, "SetProfileStatus", err)
	}
	return out, nil
}

func (s *PgStore) ListProfiles(ctx context.Context, key string) ([]providers.ProfileRecord, error) {
	var out []providers.ProfileRecord
	err := s.withScope(ctx, providers.PlatformScopeID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileCols+` FROM provider_profiles WHERE ($1 = '' OR provider_key = $1) ORDER BY provider_key, version`, key)
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
		return nil, unavailable(s, "ListProfiles", err)
	}
	return out, nil
}

// ── dispatch ────────────────────────────────────────────────────────────────

const dispatchCols = `attempt_id, COALESCE(tenant_id::text, ''), legal_entity_id::text, status, source_reference, idempotency_key,
	amount::text, currency, payer_account_ref, payee_ref, payment_reference, execution_date,
	provider_key, profile_version, rail, connector_id, sent_payload_hash, send_count, sent_at, in_flight_since, not_before,
	provider_request_id, provider_status, last_failure`

func scanDispatch(r pgx.Row) (*providers.Dispatch, error) {
	var d providers.Dispatch
	var st string
	if err := r.Scan(&d.AttemptID, &d.TenantID, &d.LegalEntityID, &st, &d.SourceReference, &d.IdempotencyKey,
		&d.AmountText, &d.Currency, &d.PayerAccountRef, &d.PayeeRef, &d.PaymentReference, &d.ExecutionDate,
		&d.ProviderKey, &d.ProfileVersion, &d.Rail, &d.ConnectorID, &d.SentPayloadHash, &d.SendCount, &d.SentAt, &d.InFlightSince, &d.NotBefore,
		&d.ProviderRequestID, &d.ProviderStatus, &d.LastFailure); err != nil {
		return nil, err
	}
	d.Status = domain.AttemptStatus(st)
	return &d, nil
}

func (s *PgStore) LoadDispatch(ctx context.Context, attemptID string) (*providers.Dispatch, error) {
	var d *providers.Dispatch
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDispatch(tx.QueryRow(ctx, `SELECT `+dispatchCols+` FROM payment_initiation_attempts WHERE attempt_id = $1`, attemptID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrAttemptNotFound
	}
	if err != nil {
		return nil, unavailable(s, "LoadDispatch", err)
	}
	return d, nil
}

// BeginSend is the compare-and-set that puts a request in flight. It is the only way an attempt becomes
// SUBMITTING, and it fails (changing nothing) unless the attempt is in `from`, is allowed to be sent now, and
// carries the same profile version and payload as any earlier send.
func (s *PgStore) BeginSend(ctx context.Context, attemptID string, from domain.AttemptStatus, profileVersion int, payloadHash, actor string) (*providers.Dispatch, error) {
	var d *providers.Dispatch
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = scanDispatch(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'SUBMITTING', in_flight_since = now(), sent_at = now(),
				send_count = send_count + 1, profile_version = COALESCE(profile_version, $3),
				sent_payload_hash = CASE WHEN sent_payload_hash = '' THEN $4 ELSE sent_payload_hash END,
				not_before = NULL, updated_at = now()
			WHERE attempt_id = $1 AND status = $2
			  AND (not_before IS NULL OR not_before <= now())
			  AND (sent_payload_hash = '' OR sent_payload_hash = $4)
			  AND (profile_version IS NULL OR profile_version = $3)
			RETURNING `+dispatchCols, attemptID, string(from), profileVersion, payloadHash))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, nullableTenant(d.TenantID), attemptID, "PROVIDER_SEND_STARTED", fmt.Sprintf("send %d via %s", d.SendCount, d.ProviderKey), actor)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// find out why, without guessing: the three guards above are the only reasons
		cur, lerr := s.LoadDispatch(ctx, attemptID)
		switch {
		case lerr != nil:
			return nil, lerr
		case cur.Status != from:
			return nil, providers.ErrInFlight
		case cur.NotBefore != nil && cur.NotBefore.After(time.Now()):
			return nil, providers.ErrNotBefore
		default:
			return nil, providers.ErrPayloadChanged
		}
	}
	if err != nil {
		return nil, unavailable(s, "BeginSend", err)
	}
	return d, nil
}

func (s *PgStore) insertTransport(ctx context.Context, tx pgx.Tx, tenant, attemptID string, t providers.TransportEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO provider_transport_log (tenant_id, attempt_id, provider_key, profile_version, operation, disposition, class, http_status,
			request_hash, response_hash, provider_ref, error_code, duration_ms, retry_after_s, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		tenant, attemptID, t.ProviderKey, t.ProfileVersion, t.Operation, t.Disposition, t.Class, t.HTTPStatus,
		t.RequestHash, t.ResponseHash, t.ProviderRef, t.ErrorCode, t.DurationMS, t.RetryAfterS, t.Actor)
	return err
}

// FinishSend applies the outcome of a provider call, and records its transport evidence, in one transaction.
func (s *PgStore) FinishSend(ctx context.Context, attemptID string, o providers.SendOutcome, actor, correlationID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		switch o.Kind {
		case providers.OutSubmitted:
			a, err = scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET status = 'SUBMITTED', provider_request_id = $2, provider_response_ref = $3,
					provider_status = $4, submitted_at = now(), in_flight_since = NULL, not_before = NULL, last_failure = '', updated_at = now()
				WHERE attempt_id = $1 AND status = $5 RETURNING `+attemptColumns,
				attemptID, o.ProviderRef, o.Transport.ResponseHash, o.Provider, string(o.From)))
		case providers.OutRejected:
			a, err = scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET status = 'REJECTED_BEFORE_SUBMISSION', rejection_reason = $2, provider_status = $3,
					in_flight_since = NULL, not_before = NULL, updated_at = now()
				WHERE attempt_id = $1 AND status = $4 RETURNING `+attemptColumns,
				attemptID, o.Reason, o.Provider, string(o.From)))
		case providers.OutUnknown:
			a, err = scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET status = 'PENDING_UNKNOWN', in_flight_since = NULL, last_failure = $2, updated_at = now()
				WHERE attempt_id = $1 AND status = $3 RETURNING `+attemptColumns,
				attemptID, o.Failure, string(o.From)))
		case providers.OutRelease:
			a, err = scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET status = $2, in_flight_since = NULL, not_before = $3, last_failure = $4, updated_at = now()
				WHERE attempt_id = $1 AND status = $5 RETURNING `+attemptColumns,
				attemptID, string(o.ReleaseTo), o.NotBefore, o.Failure, string(o.From)))
		case providers.OutNone:
			a, err = scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET last_failure = $2, updated_at = now()
				WHERE attempt_id = $1 AND status = $3 RETURNING `+attemptColumns,
				attemptID, o.Failure, string(o.From)))
		default:
			return fmt.Errorf("unknown outcome %q", o.Kind)
		}
		if err != nil {
			return err
		}
		tenant := ""
		if a.TenantID != nil {
			tenant = *a.TenantID
		}
		o.Transport.Actor = actor
		if err := s.insertTransport(ctx, tx, tenant, attemptID, o.Transport); err != nil {
			return err
		}
		switch o.Kind {
		case providers.OutSubmitted:
			if err := s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventPaymentSubmitted, o.ProviderRef, actor); err != nil {
				return err
			}
			return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payment-initiation-attempt.submitted", "PAYMENT_SUBMITTED",
				"payment-initiation-attempt", attemptID, tenant, a.LegalEntityID, actor, correlationID, submittedData(a, o))
		case providers.OutRejected:
			if err := s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventSubmissionRejected, o.Reason, actor); err != nil {
				return err
			}
			return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payment-initiation-attempt.rejected", "PAYMENT_SUBMISSION_REJECTED",
				"payment-initiation-attempt", attemptID, tenant, a.LegalEntityID, actor, correlationID, submittedData(a, o))
		case providers.OutUnknown:
			if err := s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventSubmissionAmbiguous, o.Failure, actor); err != nil {
				return err
			}
			return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payment-initiation-attempt.ambiguous", "PAYMENT_SUBMISSION_AMBIGUOUS",
				"payment-initiation-attempt", attemptID, tenant, a.LegalEntityID, actor, correlationID, submittedData(a, o))
		case providers.OutRelease:
			return s.recordEvent(ctx, tx, a.TenantID, attemptID, "PROVIDER_SEND_RELEASED", o.Failure, actor)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// the attempt is no longer in the state this outcome was for (a sweeper moved it, or it was
		// resolved elsewhere). The call's evidence is still worth keeping, but only for an attempt this
		// tenant can see: the evidence table's foreign key does not honour row-level security, so logging
		// for an invisible attempt would let a foreign tenant probe for ids and write junk against them.
		if _, lerr := s.LoadDispatch(ctx, attemptID); lerr == nil {
			_ = s.logOnly(ctx, attemptID, o.Transport, actor)
		}
		return nil, providers.ErrInFlight
	}
	if err != nil {
		return nil, unavailable(s, "FinishSend", err)
	}
	return a, nil
}

func submittedData(a *domain.PaymentInitiationAttempt, o providers.SendOutcome) map[string]any {
	return map[string]any{
		"attempt_id": a.AttemptID, "status": a.Status, "provider_ref": o.ProviderRef, "provider_status": o.Provider,
		"idempotency_key": a.IdempotencyKey, "currency": a.Currency,
	}
}

func (s *PgStore) logOnly(ctx context.Context, attemptID string, t providers.TransportEntry, actor string) error {
	return s.withTenant(ctx, func(tx pgx.Tx) error {
		tenant := middleware.TenantFromContext(ctx)
		t.Actor = actor
		return s.insertTransport(ctx, tx, tenant, attemptID, t)
	})
}

// RejectUnsent is a definite refusal of an attempt nothing was ever sent for (the profile cannot carry it).
func (s *PgStore) RejectUnsent(ctx context.Context, attemptID, reason, actor, correlationID string) (*domain.PaymentInitiationAttempt, error) {
	a, err := s.MarkRejected(ctx, attemptID, reason, actor, correlationID)
	if errors.Is(err, domain.ErrInvalidTransition) {
		return nil, providers.ErrInFlight
	}
	return a, err
}

func (s *PgStore) ListTransport(ctx context.Context, attemptID string) ([]providers.TransportEntry, error) {
	var out []providers.TransportEntry
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT log_id, attempt_id, provider_key, profile_version, operation, disposition, class, http_status, request_hash, response_hash,
				provider_ref, error_code, duration_ms, retry_after_s, actor, created_at
			FROM provider_transport_log WHERE attempt_id = $1 ORDER BY created_at, log_id`, attemptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t providers.TransportEntry
			if err := rows.Scan(&t.LogID, &t.AttemptID, &t.ProviderKey, &t.ProfileVersion, &t.Operation, &t.Disposition, &t.Class, &t.HTTPStatus,
				&t.RequestHash, &t.ResponseHash, &t.ProviderRef, &t.ErrorCode, &t.DurationMS, &t.RetryAfterS, &t.Actor, &t.CreatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(s, "ListTransport", err)
	}
	return out, nil
}

// RecentCalls returns the health of this tenant's most recent submit calls to a provider (true = the
// provider answered usefully), newest first.
func (s *PgStore) RecentCalls(ctx context.Context, providerKey string, since time.Time, limit int) ([]bool, error) {
	var out []bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT (disposition = 'ANSWERED' AND class IN ('ACKNOWLEDGED', 'PENDING', 'REJECTED', 'RATE_LIMITED'))
			FROM provider_transport_log
			WHERE provider_key = $1 AND operation = 'SUBMIT' AND created_at >= $2
			ORDER BY created_at DESC LIMIT $3`, providerKey, since, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ok bool
			if err := rows.Scan(&ok); err != nil {
				return err
			}
			out = append(out, ok)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, unavailable(s, "RecentCalls", err)
	}
	return out, nil
}

// SweepStale moves attempts stuck in SUBMITTING since before staleBefore to PENDING_UNKNOWN. The process that
// started such a call died (or hung) and no one recorded what came back: whether the request reached the
// provider is precisely what is unknown, so nothing is resent. It discovers work across tenants with a named
// flag (not an unscoped connection) and resolves each attempt in its own tenant's transaction.
func (s *PgStore) SweepStale(ctx context.Context, staleBefore time.Time, actor string) (int, error) {
	type row struct{ id, tenant string }
	var due []row
	if err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.provider_sweeper', 'true', true)"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT attempt_id::text, COALESCE(tenant_id::text, '') FROM payment_initiation_attempts
			WHERE status = 'SUBMITTING' AND in_flight_since < $1 ORDER BY in_flight_since LIMIT 200`, staleBefore)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.tenant); err != nil {
				return err
			}
			due = append(due, r)
		}
		return rows.Err()
	}(); err != nil {
		return 0, unavailable(s, "SweepStale", err)
	}
	n := 0
	for _, r := range due {
		if r.tenant == "" {
			continue
		}
		err := s.withScope(ctx, r.tenant, func(tx pgx.Tx) error {
			a, err := scanAttempt(tx.QueryRow(ctx, `
				UPDATE payment_initiation_attempts SET status = 'PENDING_UNKNOWN', in_flight_since = NULL,
					last_failure = 'in-flight request was interrupted', updated_at = now()
				WHERE attempt_id = $1 AND status = 'SUBMITTING' AND in_flight_since < $2 RETURNING `+attemptColumns, r.id, staleBefore))
			if err != nil {
				return err
			}
			if err := s.recordEvent(ctx, tx, a.TenantID, r.id, domain.EventSubmissionAmbiguous, "in-flight request was interrupted", actor); err != nil {
				return err
			}
			return s.enqueueEvent(ctx, tx, "com.zoikosuite.banking.payment-initiation-attempt.ambiguous", "PAYMENT_SUBMISSION_AMBIGUOUS",
				"payment-initiation-attempt", r.id, r.tenant, a.LegalEntityID, actor, "", map[string]any{"attempt_id": r.id, "reason": "interrupted"})
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // resolved by its own call in the meantime
		}
		if err != nil {
			s.log.Error("sweep: failed to release a stale attempt", zap.String("attempt_id", r.id), zap.Error(err))
			continue
		}
		n++
	}
	return n, nil
}

var _ = uuid.New
