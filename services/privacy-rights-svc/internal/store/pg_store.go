// Package store implements privacy-rights-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02) — see
// privacy-purpose-registry-svc's identical helper for the full rationale.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// Idem is the idempotency claim of one mutation (§18.1). The claim, the
// business change, the outbox event and the stored replay response are written
// in ONE transaction, so either all of them exist or none does: a retry after
// any failure re-executes cleanly, and concurrent identical requests execute
// exactly once.
type Idem struct {
	Key         string
	PrincipalID string
	Endpoint    string
	RequestHash string
	// Render builds the HTTP status and body to replay from the operation's
	// result (the typed value the operation returned).
	Render func(result any) (status int, body []byte, err error)
}

// Store is the interface the handler depends on. Every mutating method takes
// the idempotency claim and returns the already-stored record instead of
// executing when the key was claimed before.
type Store interface {
	CreateRequest(ctx context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error)
	FindRequest(ctx context.Context, requestID string) (*domain.RightsRequest, error)
	ListRequestsBySubject(ctx context.Context, subjectRef string) ([]domain.RightsRequest, error)

	RecordIdentityVerification(ctx context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string, idem *Idem) (*IdentityResult, *domain.IdempotencyRecord, error)
	AttachDiscoveryManifest(ctx context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string, idem *Idem) (*ManifestResult, *domain.IdempotencyRecord, error)
	ListDiscoveryManifests(ctx context.Context, requestID string) ([]domain.DiscoveryManifest, error)
	CloseRequest(ctx context.Context, requestID string, req domain.CloseRequestRequest, principalID string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error)
	AttachWFCProcessRef(ctx context.Context, requestID, wfcProcessRef string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error)

	// GetIdempotency looks up a stored response by (tenant, principal, key).
	GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error)
}

// IdentityResult / ManifestResult are the typed results of the two evidence
// mutations (the evidence row plus the updated case).
type IdentityResult struct {
	Event   *domain.IdentityVerificationEvent
	Request *domain.RightsRequest
}

type ManifestResult struct {
	Manifest *domain.DiscoveryManifest
	Request  *domain.RightsRequest
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withTenant runs fn in a transaction scoped by the verified tenant carried in
// ctx (transaction-local app.tenant_id). A missing tenant is refused outright
// rather than degrading to an empty scope.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tenant := strings.TrimSpace(middleware.TenantFromContext(ctx))
	if tenant == "" {
		return errors.New("verified tenant is required for store operations")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var errReplay = errors.New("idempotent replay")

func isDomainError(err error) bool {
	for _, e := range []error{domain.ErrRequestNotFound, domain.ErrRequestAlreadyClosed, domain.ErrIdentityNotVerified, domain.ErrNoDiscoveryManifest} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// mutate runs fn atomically with the idempotency claim (when idem != nil).
// On a prior claim it executes nothing and returns the stored record.
func (s *PgStore) mutate(ctx context.Context, op string, idem *Idem, fn func(tx pgx.Tx) (any, error)) (any, *domain.IdempotencyRecord, error) {
	tenant := strings.TrimSpace(middleware.TenantFromContext(ctx))
	var result any
	var existing *domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if idem != nil {
			tag, err := tx.Exec(ctx, `
				INSERT INTO rights_idempotency_keys
					(idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at)
				VALUES ($1, $2, $3, $4, $5, 0, ''::bytea, NOW())
				ON CONFLICT (tenant_id, principal_id, idempotency_key) DO NOTHING`,
				idem.Key, tenant, idem.PrincipalID, idem.Endpoint, idem.RequestHash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				rec, err := getIdempotencyTx(ctx, tx, tenant, idem.PrincipalID, idem.Key)
				if err != nil {
					return err
				}
				existing = rec
				return errReplay
			}
		}
		r, err := fn(tx)
		if err != nil {
			return err
		}
		result = r
		if idem != nil {
			code, body, err := idem.Render(r)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE rights_idempotency_keys SET response_code = $1, response_body = $2
				WHERE tenant_id = $3 AND principal_id = $4 AND idempotency_key = $5`,
				code, body, tenant, idem.PrincipalID, idem.Key); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errReplay):
		return nil, existing, nil
	case err == nil:
		return result, nil, nil
	case isDomainError(err):
		return nil, nil, err
	default:
		s.log.Error("pg "+op+" failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const requestColumns = `
	request_id, tenant_id, subject_ref, right_family, jurisdiction, requester_ref, submitted_via,
	status, identity_verified, outcome, response_evidence_hash, response_package_version, wfc_process_ref,
	created_at, created_by_principal_id, closed_at`

func scanRequest(row pgx.Row) (*domain.RightsRequest, error) {
	r := &domain.RightsRequest{}
	var outcome *string
	err := row.Scan(&r.RequestID, &r.TenantID, &r.SubjectRef, &r.RightFamily, &nullString{&r.Jurisdiction}, &nullString{&r.RequesterRef}, &nullString{&r.SubmittedVia},
		&r.Status, &r.IdentityVerified, &outcome, &r.ResponseEvidenceHash, &r.ResponsePackageVersion, &r.WFCProcessRef,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.ClosedAt)
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		o := domain.Outcome(*outcome)
		r.Outcome = &o
	}
	return r, nil
}

// lockOpenRequest locks the case row and refuses a missing or CLOSED case, so
// the closed-case check and the write happen under the same lock.
func lockOpenRequest(ctx context.Context, tx pgx.Tx, requestID string) (tenantID *string, err error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT tenant_id, status FROM rights_requests WHERE request_id = $1 FOR UPDATE`, requestID).Scan(&tenantID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return nil, domain.ErrRequestNotFound
		}
		return nil, err
	}
	if status == string(domain.StatusClosed) {
		return nil, domain.ErrRequestAlreadyClosed
	}
	return tenantID, nil
}

func (s *PgStore) CreateRequest(ctx context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, existing, err := s.mutate(ctx, "CreateRequest", idem, func(tx pgx.Tx) (any, error) {
		request, err := scanRequest(tx.QueryRow(ctx, `
			INSERT INTO rights_requests (`+requestColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'RECEIVED', false, NULL, NULL, 0, NULL, NOW(), $8, NULL)
			RETURNING `+requestColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.RightFamily, strPtrOrNil(req.Jurisdiction),
			strPtrOrNil(req.RequesterRef), strPtrOrNil(req.SubmittedVia), principalID,
		))
		if err != nil {
			return nil, err
		}
		return request, nil
	})
	if err != nil || existing != nil {
		return nil, existing, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

func (s *PgStore) FindRequest(ctx context.Context, requestID string) (*domain.RightsRequest, error) {
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM rights_requests WHERE request_id = $1`, requestID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrRequestNotFound
	}
	if err != nil {
		s.log.Error("pg FindRequest failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return request, nil
}

func (s *PgStore) ListRequestsBySubject(ctx context.Context, subjectRef string) ([]domain.RightsRequest, error) {
	var out []domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM rights_requests WHERE subject_ref = $1 ORDER BY created_at DESC`, subjectRef)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequest(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListRequestsBySubject failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) RecordIdentityVerification(ctx context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string, idem *Idem) (*IdentityResult, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, existing, err := s.mutate(ctx, "RecordIdentityVerification", idem, func(tx pgx.Tx) (any, error) {
		tenantID, err := lockOpenRequest(ctx, tx, requestID)
		if err != nil {
			return nil, err
		}
		var event domain.IdentityVerificationEvent
		if err := tx.QueryRow(ctx, `
			INSERT INTO identity_verification_events (event_id, tenant_id, request_id, verified, method, note, verified_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING event_id, tenant_id, request_id, verified, method, note, verified_by_principal_id, created_at`,
			id, tenantID, requestID, req.Verified, req.Method, strPtrOrNil(req.Note), principalID,
		).Scan(&event.EventID, &event.TenantID, &event.RequestID, &event.Verified, &event.Method, &nullString{&event.Note}, &event.VerifiedByPrincipalID, &event.CreatedAt); err != nil {
			return nil, err
		}

		// Only a VERIFIED=true event advances status — a failed attempt is
		// recorded as evidence but must never silently advance the case.
		var request *domain.RightsRequest
		if req.Verified {
			request, err = scanRequest(tx.QueryRow(ctx, `
				UPDATE rights_requests SET identity_verified = true,
					status = CASE WHEN status = 'RECEIVED' THEN 'IDENTITY_VERIFIED' ELSE status END
				WHERE request_id = $1
				RETURNING `+requestColumns,
				requestID,
			))
		} else {
			request, err = scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM rights_requests WHERE request_id = $1`, requestID))
		}
		if err != nil {
			return nil, err
		}
		return &IdentityResult{Event: &event, Request: request}, nil
	})
	if err != nil || existing != nil {
		return nil, existing, err
	}
	return res.(*IdentityResult), nil, nil
}

func (s *PgStore) AttachDiscoveryManifest(ctx context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string, idem *Idem) (*ManifestResult, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, existing, err := s.mutate(ctx, "AttachDiscoveryManifest", idem, func(tx pgx.Tx) (any, error) {
		tenantID, err := lockOpenRequest(ctx, tx, requestID)
		if err != nil {
			return nil, err
		}
		var manifest domain.DiscoveryManifest
		if err := tx.QueryRow(ctx, `
			INSERT INTO discovery_manifests (manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, evidence_ref, submitted_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, evidence_ref, submitted_by_principal_id, created_at`,
			id, tenantID, requestID, req.Domain, req.ContentHash, req.CandidateCount, strPtrOrNil(req.EvidenceRef), principalID,
		).Scan(&manifest.ManifestID, &manifest.TenantID, &manifest.RequestID, &manifest.Domain, &manifest.ContentHash,
			&manifest.CandidateCount, &nullString{&manifest.EvidenceRef}, &manifest.SubmittedByPrincipalID, &manifest.CreatedAt); err != nil {
			return nil, err
		}
		request, err := scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET
				status = CASE WHEN status = 'IDENTITY_VERIFIED' THEN 'IN_DISCOVERY' ELSE status END
			WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID,
		))
		if err != nil {
			return nil, err
		}
		return &ManifestResult{Manifest: &manifest, Request: request}, nil
	})
	if err != nil || existing != nil {
		return nil, existing, err
	}
	return res.(*ManifestResult), nil, nil
}

func (s *PgStore) ListDiscoveryManifests(ctx context.Context, requestID string) ([]domain.DiscoveryManifest, error) {
	var out []domain.DiscoveryManifest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, evidence_ref, submitted_by_principal_id, created_at
			FROM discovery_manifests WHERE request_id = $1 ORDER BY created_at ASC`, requestID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.DiscoveryManifest
			if err := rows.Scan(&m.ManifestID, &m.TenantID, &m.RequestID, &m.Domain, &m.ContentHash,
				&m.CandidateCount, &nullString{&m.EvidenceRef}, &m.SubmittedByPrincipalID, &m.CreatedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListDiscoveryManifests failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// CloseRequest enforces what this service can verify of §15.2's DISCLOSURE
// GATE at the store layer, atomically with the close: FULFILLED requires that
// a VERIFIED identity-assurance event actually exists for the case (not merely
// that the denormalised flag is set) AND at least one discovery manifest.
// Applicable-right determination, scope review, exemptions, third-party
// protection, redaction and approved response assembly are WFC/reviewer-owned
// (§14.1) and are not verifiable here — see the README.
// I21: response package versioning — increments on every FULFILLED closure.
func (s *PgStore) CloseRequest(ctx context.Context, requestID string, req domain.CloseRequestRequest, principalID string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	res, existing, err := s.mutate(ctx, "CloseRequest", idem, func(tx pgx.Tx) (any, error) {
		var status string
		var identityVerified bool
		var currentVersion int
		if err := tx.QueryRow(ctx, `SELECT status, identity_verified, response_package_version FROM rights_requests WHERE request_id = $1 FOR UPDATE`, requestID).
			Scan(&status, &identityVerified, &currentVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return nil, domain.ErrRequestNotFound
			}
			return nil, err
		}
		if status == string(domain.StatusClosed) {
			return nil, domain.ErrRequestAlreadyClosed
		}

		if req.Outcome == domain.OutcomeFulfilled {
			var verifiedEvents int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM identity_verification_events WHERE request_id = $1 AND verified = true`, requestID).Scan(&verifiedEvents); err != nil {
				return nil, err
			}
			if !identityVerified || verifiedEvents == 0 {
				return nil, domain.ErrIdentityNotVerified
			}
			var manifestCount int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM discovery_manifests WHERE request_id = $1`, requestID).Scan(&manifestCount); err != nil {
				return nil, err
			}
			if manifestCount == 0 {
				return nil, domain.ErrNoDiscoveryManifest
			}
		}

		nextVersion := currentVersion
		if req.Outcome == domain.OutcomeFulfilled {
			nextVersion = currentVersion + 1
		}
		request, err := scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET
				status = 'CLOSED', outcome = $2, response_evidence_hash = $3, response_package_version = $4, closed_at = NOW()
			WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID, req.Outcome, strPtrOrNil(req.ResponseEvidenceHash), nextVersion,
		))
		if err != nil {
			return nil, err
		}
		return request, nil
	})
	if err != nil || existing != nil {
		return nil, existing, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

func (s *PgStore) AttachWFCProcessRef(ctx context.Context, requestID, wfcProcessRef string, idem *Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	res, existing, err := s.mutate(ctx, "AttachWFCProcessRef", idem, func(tx pgx.Tx) (any, error) {
		if _, err := lockOpenRequest(ctx, tx, requestID); err != nil {
			return nil, err
		}
		return scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET wfc_process_ref = $2 WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID, wfcProcessRef,
		))
	})
	if err != nil || existing != nil {
		return nil, existing, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer)
// string field on a domain struct: several fields here (Note, EvidenceRef,
// Jurisdiction, RequesterRef, SubmittedVia) are documented as
// always-present-but-possibly-empty strings on the wire, so the scan target
// must bridge NULL -> "" rather than fail outright.
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

// ── Idempotency (§18.1) ──────────────────────────────────────────────────────

func getIdempotencyTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := tx.QueryRow(ctx, `
		SELECT idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at
		FROM rights_idempotency_keys
		WHERE tenant_id = $1 AND principal_id = $2 AND idempotency_key = $3`,
		tenantID, principalID, key,
	).Scan(&rec.Key, &rec.TenantID, &rec.PrincipalID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec *domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		r, err := getIdempotencyTx(ctx, tx, tenantID, principalID, key)
		rec = r
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return rec, nil
}
