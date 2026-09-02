// Package store implements privacy-rights-svc's persistence.
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

// Store is the interface the handler depends on.
type Store interface {
	CreateRequest(ctx context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string) (*domain.RightsRequest, error)
	FindRequest(ctx context.Context, requestID string) (*domain.RightsRequest, error)
	ListRequestsBySubject(ctx context.Context, subjectRef string) ([]domain.RightsRequest, error)

	RecordIdentityVerification(ctx context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string) (*domain.IdentityVerificationEvent, *domain.RightsRequest, error)
	AttachDiscoveryManifest(ctx context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string) (*domain.DiscoveryManifest, *domain.RightsRequest, error)
	ListDiscoveryManifests(ctx context.Context, requestID string) ([]domain.DiscoveryManifest, error)
	CloseRequest(ctx context.Context, requestID string, req domain.CloseRequestRequest, principalID string) (*domain.RightsRequest, error)
	AttachWFCProcessRef(ctx context.Context, requestID, wfcProcessRef string) (*domain.RightsRequest, error)
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

const requestColumns = `
	request_id, tenant_id, subject_ref, right_family, jurisdiction, requester_ref, submitted_via,
	status, identity_verified, outcome, response_evidence_hash, wfc_process_ref,
	created_at, created_by_principal_id, closed_at`

func scanRequest(row pgx.Row) (*domain.RightsRequest, error) {
	r := &domain.RightsRequest{}
	var outcome *string
	err := row.Scan(&r.RequestID, &r.TenantID, &r.SubjectRef, &r.RightFamily, &nullString{&r.Jurisdiction}, &nullString{&r.RequesterRef}, &nullString{&r.SubmittedVia},
		&r.Status, &r.IdentityVerified, &outcome, &r.ResponseEvidenceHash, &r.WFCProcessRef,
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

func (s *PgStore) CreateRequest(ctx context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string) (*domain.RightsRequest, error) {
	id := uuid.New().String()
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `
			INSERT INTO rights_requests (`+requestColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'RECEIVED', false, NULL, NULL, NULL, NOW(), $8, NULL)
			RETURNING `+requestColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.RightFamily, strPtrOrNil(req.Jurisdiction),
			strPtrOrNil(req.RequesterRef), strPtrOrNil(req.SubmittedVia), principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateRequest failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return request, nil
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

func (s *PgStore) RecordIdentityVerification(ctx context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string) (*domain.IdentityVerificationEvent, *domain.RightsRequest, error) {
	id := uuid.New().String()
	var event domain.IdentityVerificationEvent
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM rights_requests WHERE request_id = $1`, requestID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrRequestNotFound
			}
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO identity_verification_events (event_id, tenant_id, request_id, verified, method, note, verified_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING event_id, tenant_id, request_id, verified, method, note, verified_by_principal_id, created_at`,
			id, tenantID, requestID, req.Verified, req.Method, strPtrOrNilForNote(req.Note), principalID,
		).Scan(&event.EventID, &event.TenantID, &event.RequestID, &event.Verified, &event.Method, &nullString{&event.Note}, &event.VerifiedByPrincipalID, &event.CreatedAt); err != nil {
			return err
		}

		// Only a VERIFIED=true event advances status — a failed attempt is
		// recorded as evidence but must never silently advance the case.
		if req.Verified {
			var err error
			request, err = scanRequest(tx.QueryRow(ctx, `
				UPDATE rights_requests SET identity_verified = true,
					status = CASE WHEN status = 'RECEIVED' THEN 'IDENTITY_VERIFIED' ELSE status END
				WHERE request_id = $1
				RETURNING `+requestColumns,
				requestID,
			))
			return err
		}
		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM rights_requests WHERE request_id = $1`, requestID))
		return err
	})
	if errors.Is(err, domain.ErrRequestNotFound) {
		return nil, nil, domain.ErrRequestNotFound
	}
	if err != nil {
		s.log.Error("pg RecordIdentityVerification failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &event, request, nil
}

func (s *PgStore) AttachDiscoveryManifest(ctx context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string) (*domain.DiscoveryManifest, *domain.RightsRequest, error) {
	id := uuid.New().String()
	var manifest domain.DiscoveryManifest
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM rights_requests WHERE request_id = $1`, requestID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrRequestNotFound
			}
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO discovery_manifests (manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, evidence_ref, submitted_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, evidence_ref, submitted_by_principal_id, created_at`,
			id, tenantID, requestID, req.Domain, req.ContentHash, req.CandidateCount, strPtrOrNil(req.EvidenceRef), principalID,
		).Scan(&manifest.ManifestID, &manifest.TenantID, &manifest.RequestID, &manifest.Domain, &manifest.ContentHash,
			&manifest.CandidateCount, &nullString{&manifest.EvidenceRef}, &manifest.SubmittedByPrincipalID, &manifest.CreatedAt); err != nil {
			return err
		}

		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET
				status = CASE WHEN status = 'IDENTITY_VERIFIED' THEN 'IN_DISCOVERY' ELSE status END
			WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID,
		))
		return err
	})
	if errors.Is(err, domain.ErrRequestNotFound) {
		return nil, nil, domain.ErrRequestNotFound
	}
	if err != nil {
		s.log.Error("pg AttachDiscoveryManifest failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &manifest, request, nil
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

// CloseRequest enforces §15.2's DISCLOSURE GATE at the store layer, not
// just the handler: FULFILLED requires identity_verified=true AND at
// least one discovery manifest — checked atomically inside the same
// transaction that performs the close, so a race between two callers
// cannot slip a FULFILLED closure past the gate.
func (s *PgStore) CloseRequest(ctx context.Context, requestID string, req domain.CloseRequestRequest, principalID string) (*domain.RightsRequest, error) {
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		var identityVerified bool
		if err := tx.QueryRow(ctx, `SELECT status, identity_verified FROM rights_requests WHERE request_id = $1`, requestID).Scan(&status, &identityVerified); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrRequestNotFound
			}
			return err
		}
		if status == string(domain.StatusClosed) {
			return domain.ErrRequestAlreadyClosed
		}

		if req.Outcome == domain.OutcomeFulfilled {
			if !identityVerified {
				return domain.ErrIdentityNotVerified
			}
			var manifestCount int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM discovery_manifests WHERE request_id = $1`, requestID).Scan(&manifestCount); err != nil {
				return err
			}
			if manifestCount == 0 {
				return domain.ErrNoDiscoveryManifest
			}
		}

		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET
				status = 'CLOSED', outcome = $2, response_evidence_hash = $3, closed_at = NOW()
			WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID, req.Outcome, strPtrOrNil(req.ResponseEvidenceHash),
		))
		return err
	})
	switch {
	case errors.Is(err, domain.ErrRequestNotFound), errors.Is(err, domain.ErrRequestAlreadyClosed),
		errors.Is(err, domain.ErrIdentityNotVerified), errors.Is(err, domain.ErrNoDiscoveryManifest):
		return nil, err
	case err != nil:
		s.log.Error("pg CloseRequest failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return request, nil
}

func (s *PgStore) AttachWFCProcessRef(ctx context.Context, requestID, wfcProcessRef string) (*domain.RightsRequest, error) {
	var request *domain.RightsRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		request, err = scanRequest(tx.QueryRow(ctx, `
			UPDATE rights_requests SET wfc_process_ref = $2 WHERE request_id = $1
			RETURNING `+requestColumns,
			requestID, wfcProcessRef,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrRequestNotFound
	}
	if err != nil {
		s.log.Error("pg AttachWFCProcessRef failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return request, nil
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer)
// string field on a domain struct — several fields here (Note,
// EvidenceRef, Jurisdiction, RequesterRef, SubmittedVia) are documented
// as always-present-but-possibly-empty strings on the wire, not
// omitempty pointers, so the scan target must bridge NULL -> "" rather
// than fail outright. A gap in this coverage (scanRequest originally
// scanned Jurisdiction/RequesterRef/SubmittedVia directly into *string)
// was caught by live-stack testing, not the unit suite, whose stub store
// has no real SQL NULL to expose the mismatch.
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

func strPtrOrNilForNote(s string) *string {
	return strPtrOrNil(s)
}
