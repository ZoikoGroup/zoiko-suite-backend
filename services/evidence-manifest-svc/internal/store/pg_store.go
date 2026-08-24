// Package store is evidence-manifest-svc's Postgres persistence layer.
//
// Tenant scoping here is both an explicit tenant_id predicate and the RLS
// policy from migration 000003. Before this change there was neither: every
// method keyed on manifest_id alone, and the service read no X-Tenant-Id at
// all, so any caller holding (or guessing) a manifest id could read that
// manifest and — through ListRecords — the full record_snapshot payload of
// every record in it.
//
// That payload is the reason this ranked as the worst read exposure found so
// far. manifest_records.record_snapshot is a verbatim JSON snapshot of the
// source record as it existed at generation time: governance decisions,
// access decisions and workflow instances, deliberately copied in full so
// the manifest stays reconstructable if the source service is later
// unavailable. It is an evidence bundle assembled for an auditor, regulator
// or legal-discovery request (Doc 03 §14.4) — not metadata about one, but
// the evidence itself.
//
// manifest_records carries no tenant_id of its own; it reaches the tenant
// through manifest_id. Both the SQL predicate here and the RLS policy
// therefore resolve through evidence_manifests. See migration 000003 for
// what that coupling does when the parent policy is changed — the two ways
// of removing it behave in opposite directions, which is not the intuitive
// reading.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so migration 000003's policies have a value to enforce
// against.
//
// A transaction is required rather than incidental: set_config's third
// argument is is_local, and only a transaction-local setting is safe on a
// pooled connection. Setting it session-wide would leak one request's tenant
// into whichever request acquires that connection next.
//
// The tenant comes from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a request body or URL. It
// returns "" when absent rather than a fabricated default, and "" matches no
// tenant_id, so a call arriving without a verified tenant sees and writes
// nothing.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateManifest opens a new manifest for the caller's own tenant.
//
// The tenant is taken from the request context, not from m.TenantID as the
// caller supplied it. The handler binds the two and refuses a mismatch, so
// this is belt-and-braces rather than a second source of truth: previously
// the body field was the ONLY source, which made the tenant caller-declared.
func (s *PgStore) CreateManifest(ctx context.Context, m *domain.EvidenceManifest) error {
	m.TenantID = svcmiddleware.TenantFromContext(ctx)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO evidence_manifests (tenant_id, legal_entity_id, scenario_type, requested_by, status)
			VALUES ($1, $2, $3, $4, 'PENDING')
			RETURNING manifest_id, requested_at
		`, m.TenantID, m.LegalEntityID, string(m.ScenarioType), m.RequestedBy).Scan(&m.ManifestID, &m.RequestedAt)
	})
	if err != nil {
		return fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	m.Status = domain.StatusPending
	return nil
}

// AddRecord appends one immutable ManifestRecord — never updated, never
// deleted, same doctrine as every other evidential store in this platform
// (enforced by the trigger in migration 000002, which binds even a
// superuser).
//
// The insert carries no tenant column because manifest_records has none. Its
// boundary is the RLS policy, which resolves manifest_id through
// evidence_manifests — so WITH CHECK refuses a record appended to a manifest
// the caller cannot see, and there is no way to express "append to another
// tenant's manifest" here.
func (s *PgStore) AddRecord(ctx context.Context, r *domain.ManifestRecord) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO manifest_records (manifest_id, source_type, source_record_id, record_snapshot)
			VALUES ($1, $2, $3, $4)
			RETURNING manifest_record_id, fetched_at
		`, r.ManifestID, string(r.SourceType), r.SourceRecordID, r.RecordSnapshot,
		).Scan(&r.ManifestRecordID, &r.FetchedAt)
	})
	if err != nil {
		return fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return nil
}

// FinalizeGenerated marks a manifest GENERATED with its checksum. This is the
// LAST mutation ever applied to a manifest row — after this, it and its
// records are immutable evidence.
//
// The tenant predicate is new, and this was an unscoped WRITE on an evidence
// record. Any caller holding another tenant's manifest_id could set that
// manifest's status and checksum_sha256 — writing a wrong checksum onto an
// evidence bundle destined for a regulator, and doing it irreversibly, since
// GENERATED is terminal and a retry produces a new manifest rather than
// repairing this one.
func (s *PgStore) FinalizeGenerated(ctx context.Context, manifestID, checksumSHA256 string) (*domain.EvidenceManifest, error) {
	m, err := s.finalize(ctx, manifestID, `
		UPDATE evidence_manifests SET status = 'GENERATED', checksum_sha256 = $2, generated_at = now()
		WHERE manifest_id = $1 AND tenant_id::text = $3
		RETURNING manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
			checksum_sha256, failure_reason, requested_at, generated_at
	`, checksumSHA256)
	return m, err
}

// FinalizeFailed marks a manifest FAILED with a reason — still a terminal,
// immutable state; a retry creates a brand-new manifest, it never resurrects
// this one.
//
// The tenant predicate is new. Unscoped, this was a denial-of-evidence
// primitive: any caller holding another tenant's manifest_id could mark
// their in-flight evidence bundle FAILED with an arbitrary reason, and
// because FAILED is terminal that tenant's request is dead rather than
// retryable in place. Doc 03 §22 requires evidence to fail safe; being able
// to fail somebody else's on demand is the inverse.
func (s *PgStore) FinalizeFailed(ctx context.Context, manifestID, reason string) (*domain.EvidenceManifest, error) {
	m, err := s.finalize(ctx, manifestID, `
		UPDATE evidence_manifests SET status = 'FAILED', failure_reason = $2
		WHERE manifest_id = $1 AND tenant_id::text = $3
		RETURNING manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
			checksum_sha256, failure_reason, requested_at, generated_at
	`, reason)
	return m, err
}

func (s *PgStore) finalize(ctx context.Context, manifestID, query, arg2 string) (*domain.EvidenceManifest, error) {
	var m domain.EvidenceManifest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, manifestID, arg2, svcmiddleware.TenantFromContext(ctx)).Scan(
			&m.ManifestID, &m.TenantID, &m.LegalEntityID, &m.ScenarioType, &m.RequestedBy, &m.Status,
			&m.ChecksumSHA256, &m.FailureReason, &m.RequestedAt, &m.GeneratedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManifestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return &m, nil
}

// FindManifestByID reads one manifest, scoped to the caller's tenant.
//
// Returns ErrManifestNotFound for another tenant's manifest — the same error
// as a genuinely absent one, never a distinct forbidden, so this cannot be
// used to confirm that another tenant's manifest_id exists.
func (s *PgStore) FindManifestByID(ctx context.Context, manifestID string) (*domain.EvidenceManifest, error) {
	var m domain.EvidenceManifest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
				checksum_sha256, failure_reason, requested_at, generated_at
			FROM evidence_manifests WHERE manifest_id = $1 AND tenant_id::text = $2
		`, manifestID, svcmiddleware.TenantFromContext(ctx)).Scan(
			&m.ManifestID, &m.TenantID, &m.LegalEntityID, &m.ScenarioType, &m.RequestedBy, &m.Status,
			&m.ChecksumSHA256, &m.FailureReason, &m.RequestedAt, &m.GeneratedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManifestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return &m, nil
}

// ListRecords reads a manifest's records, scoped to the caller's tenant.
//
// This was the worst of the unscoped reads: manifest_records has no
// tenant_id, the query filtered on manifest_id alone, and record_snapshot is
// the verbatim JSON of each source record. So a caller with another tenant's
// manifest id received that tenant's governance decisions, access decisions
// and workflow instances in full.
//
// The tenant is enforced by joining to evidence_manifests rather than by
// trusting the caller's manifest_id, and the RLS policy enforces the same
// path independently.
func (s *PgStore) ListRecords(ctx context.Context, manifestID string) ([]domain.ManifestRecord, error) {
	var out []domain.ManifestRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.manifest_record_id, r.manifest_id, r.source_type, r.source_record_id,
				r.record_snapshot, r.fetched_at
			FROM manifest_records r
			JOIN evidence_manifests m ON m.manifest_id = r.manifest_id
			WHERE r.manifest_id = $1 AND m.tenant_id::text = $2
			ORDER BY r.fetched_at ASC
		`, manifestID, svcmiddleware.TenantFromContext(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r domain.ManifestRecord
			if err := rows.Scan(&r.ManifestRecordID, &r.ManifestID, &r.SourceType, &r.SourceRecordID,
				&r.RecordSnapshot, &r.FetchedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return out, nil
}
