package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
)

// ArchiveStore is AUD-10's own persistence contract for this service — kept
// separate from Store above, same "self-contained composed interface" shape
// used for every other AUD-0X capability built this session.
//
// Every method takes the caller's resolved tenant. Archive records are
// tenant-owned and tenant-read: migration 000005 adds tenant_id and a
// tenant-only RLS policy to both archive tables, and each method sets
// app.tenant_id for its transaction so the policy is active. An empty tenant
// fails closed with domain.ErrTenantRequired rather than running unscoped.
type ArchiveStore interface {
	CreateArchive(ctx context.Context, p domain.CreateArchiveParams) (*domain.Archive, error)
	GetArchive(ctx context.Context, archiveID, tenantID string) (*domain.Archive, error)
	VerifyArchive(ctx context.Context, p domain.VerifyArchiveParams) (*domain.ArchiveVerification, error)
	ListVerifications(ctx context.Context, archiveID, tenantID string) ([]domain.ArchiveVerification, error)
}

// setTenantScope sets app.tenant_id for the current transaction so the
// tenant-only RLS policies on audit_archives / audit_archive_verifications
// (migration 000005) are active. is_local=true keeps it transaction-scoped.
func setTenantScope(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}

type chainRow struct {
	seq      int64
	payload  string  // stored payload_hash
	previous *string // stored previous_event_hash
	// recomputed is the SHA-256 of the row's stored payload, computed by the
	// database at read time. It is nil for legacy rows (payload_hash_scheme IS
	// NULL), whose hash was taken over bytes the JSONB column no longer holds
	// and therefore cannot be re-derived.
	recomputed *string
}

// readChainRange reads audit_events in [from,to] ordered by sequence_number,
// under app.platform_scope — an archive spans the GLOBAL chain (migration
// 000003's own rationale: sequence numbers are not tenant-scoped), so this
// deliberately bypasses tenant RLS the same way PgStore.Store's chain-tip
// read does.
func readChainRange(ctx context.Context, tx pgx.Tx, from, to int64) ([]chainRow, error) {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}
	rows, err := tx.Query(ctx,
		`SELECT sequence_number, payload_hash, previous_event_hash,
		        CASE WHEN payload_hash_scheme = '`+PayloadHashSchemeJSONBTextV1+`'
		             THEN encode(sha256(convert_to(payload::text, 'UTF8')), 'hex') END
		 FROM audit_events
		 WHERE sequence_number BETWEEN $1 AND $2 ORDER BY sequence_number ASC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read chain range: %w", err)
	}
	defer rows.Close()
	var out []chainRow
	for rows.Next() {
		var r chainRow
		if err := rows.Scan(&r.seq, &r.payload, &r.previous, &r.recomputed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// verifyLinks walks rows checking three things: (1) every row whose hash can be
// re-derived (payload_hash_scheme set) still hashes to its stored payload_hash —
// this is what detects a modified payload; (2) sequence numbers are contiguous
// (this chain is designed to never have gaps — see store.go's own nextSeq
// comment — so a gap here means a row is missing, not that a gap was ever
// legitimate); and (3) each row's previous_event_hash equals the prior row's
// payload_hash. Returns the sequence number of the first row that fails a
// check, or nil if the whole range is intact. Legacy rows (nil recomputed) get
// checks (2) and (3) only.
func verifyLinks(rows []chainRow) *int64 {
	for i := 0; i < len(rows); i++ {
		if rows[i].recomputed != nil && *rows[i].recomputed != rows[i].payload {
			seq := rows[i].seq
			return &seq
		}
		if i == 0 {
			continue
		}
		if rows[i].seq != rows[i-1].seq+1 {
			seq := rows[i-1].seq + 1
			return &seq
		}
		if rows[i].previous == nil || *rows[i].previous != rows[i-1].payload {
			seq := rows[i].seq
			return &seq
		}
	}
	return nil
}

// digestChain is the archive's own content digest: sha256 over the
// ordered, concatenated payload_hashes of every row in the range. Computed
// fresh both at CreateArchive time and at every later VerifyArchive call —
// a match proves nothing in the range has diverged since.
func digestChain(rows []chainRow) string {
	h := sha256.New()
	for _, r := range rows {
		h.Write([]byte(r.payload))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CreateArchive re-walks the hash chain across [from,to], verifies it is
// intact, and — only if it is — writes a permanent archive record. See
// ErrChainBroken's own doc comment: a broken chain writes nothing.
func (s *PgStore) CreateArchive(ctx context.Context, p domain.CreateArchiveParams) (*domain.Archive, error) {
	if p.TenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	if p.ToSequence < p.FromSequence {
		return nil, domain.ErrInvalidRange
	}

	var archive *domain.Archive
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Tenant scope for the archive table's RLS policy (migration 000005).
		// This is set for the whole transaction, alongside the platform scope
		// readChainRange sets below. The archive policy deliberately does not
		// reference app.platform_scope, so the INSERT's WITH CHECK is still
		// governed by this tenant — the two settings do not conflict.
		if err := setTenantScope(ctx, tx, p.TenantID); err != nil {
			return err
		}

		// Idempotency (migration 000007). Claimed before the chain is read, in
		// this same transaction: a replay returns the original archive, a
		// concurrent duplicate waits for the first to commit, and any failure
		// below rolls the claim back with everything else.
		if p.Idem != nil {
			replayed, err := claimIdempotency(ctx, tx, p.TenantID, p.Idem)
			if err != nil {
				return err
			}
			if replayed != nil {
				archive = replayed
				return nil
			}
		}

		rows, err := readChainRange(ctx, tx, p.FromSequence, p.ToSequence)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return domain.ErrEmptyRange
		}
		if divergedAt := verifyLinks(rows); divergedAt != nil {
			s.log.Warn("chain broken during archive attempt", zap.Int64("first_divergent_sequence", *divergedAt))
			return domain.ErrChainBroken
		}

		archiveID := "archive-" + uuid.New().String()
		digest := digestChain(rows)
		row := tx.QueryRow(ctx, `
			INSERT INTO audit_archives
				(archive_id, tenant_id, from_sequence, to_sequence, event_count, archive_digest,
				 first_event_hash, last_event_hash, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING archive_id, tenant_id, from_sequence, to_sequence, event_count, archive_digest,
				first_event_hash, last_event_hash, created_by_principal_id, created_at`,
			archiveID, p.TenantID, rows[0].seq, rows[len(rows)-1].seq, int64(len(rows)), digest,
			rows[0].payload, rows[len(rows)-1].payload, p.CreatedByPrincipalID)

		a := &domain.Archive{}
		if err := row.Scan(&a.ArchiveID, &a.TenantID, &a.FromSequence, &a.ToSequence, &a.EventCount, &a.ArchiveDigest,
			&a.FirstEventHash, &a.LastEventHash, &a.CreatedByPrincipalID, &a.CreatedAt); err != nil {
			return fmt.Errorf("insert archive: %w", err)
		}
		if p.Idem != nil {
			if err := recordIdempotency(ctx, tx, p.TenantID, p.Idem, a); err != nil {
				return err
			}
		}
		archive = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return archive, nil
}

// GetArchive fetches an archive by id, scoped to tenantID. A different tenant's
// archive is reported as ErrArchiveNotFound — the same response as a genuinely
// missing archive, so the endpoint leaks no existence information across
// tenants. The transaction sets app.tenant_id for the RLS policy and the query
// also carries an explicit tenant predicate (defense in depth).
func (s *PgStore) GetArchive(ctx context.Context, archiveID, tenantID string) (*domain.Archive, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	a := &domain.Archive{}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := setTenantScope(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			SELECT archive_id, tenant_id, from_sequence, to_sequence, event_count, archive_digest,
				first_event_hash, last_event_hash, created_by_principal_id, created_at
			FROM audit_archives WHERE archive_id=$1 AND tenant_id=$2`, archiveID, tenantID).
			Scan(&a.ArchiveID, &a.TenantID, &a.FromSequence, &a.ToSequence, &a.EventCount, &a.ArchiveDigest,
				&a.FirstEventHash, &a.LastEventHash, &a.CreatedByPrincipalID, &a.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrArchiveNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// VerifyArchive independently re-derives today's truth about the archived
// range rather than trusting the stored claim — it never compares against
// a per-row snapshot (none is kept; audit_events rows are immutable by
// their own triggers — migration 000006 — so keeping one would be redundant), it recomputes
// the same two checks CreateArchive made, live, and compares outcomes:
//
//  1. Row count: fewer rows than archived means something in range is gone.
//  2. Link and payload integrity: same contiguous-sequence + hash-link walk,
//     plus recomputing each (non-legacy) row's payload hash from its stored
//     payload.
//  3. Digest: the final defense-in-depth check, catching a divergence that
//     neither (1) nor (2) would (in practice: content of a stored payload
//     itself changing despite the immutability triggers).
//
// Every call — VERIFIED or DIVERGED — is itself recorded permanently.
func (s *PgStore) VerifyArchive(ctx context.Context, p domain.VerifyArchiveParams) (*domain.ArchiveVerification, error) {
	if p.TenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	// Tenant-scoped lookup: another tenant's archive is ErrArchiveNotFound, so
	// verification can never read (or even prove the existence of) it.
	archive, err := s.GetArchive(ctx, p.ArchiveID, p.TenantID)
	if err != nil {
		return nil, err
	}

	var verification *domain.ArchiveVerification
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Tenant scope for the verification table's RLS policy and its INSERT
		// WITH CHECK (migration 000005). readChainRange below additionally sets
		// app.platform_scope to read the global audit_events chain; that does
		// not weaken the archive/verification policies.
		if err := setTenantScope(ctx, tx, p.TenantID); err != nil {
			return err
		}

		rows, err := readChainRange(ctx, tx, archive.FromSequence, archive.ToSequence)
		if err != nil {
			return err
		}

		result := domain.VerificationVerified
		var divergedAt *int64

		switch {
		case int64(len(rows)) != archive.EventCount:
			result = domain.VerificationDiverged
			divergedAt = firstMissingSequence(rows, archive.FromSequence, archive.ToSequence)
		case verifyLinks(rows) != nil:
			result = domain.VerificationDiverged
			divergedAt = verifyLinks(rows)
		case digestChain(rows) != archive.ArchiveDigest:
			result = domain.VerificationDiverged
			seq := archive.FromSequence
			divergedAt = &seq
		}

		id := "archverif-" + uuid.New().String()
		row := tx.QueryRow(ctx, `
			INSERT INTO audit_archive_verifications
				(verification_id, archive_id, tenant_id, verified_by_principal_id, result, first_divergent_sequence)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING verification_id, archive_id, tenant_id, verified_by_principal_id, verified_at, result, first_divergent_sequence`,
			id, archive.ArchiveID, p.TenantID, p.VerifiedByPrincipalID, string(result), divergedAt)

		v := &domain.ArchiveVerification{}
		var resultStr string
		if err := row.Scan(&v.VerificationID, &v.ArchiveID, &v.TenantID, &v.VerifiedByPrincipalID, &v.VerifiedAt, &resultStr, &v.FirstDivergentSequence); err != nil {
			return fmt.Errorf("insert verification: %w", err)
		}
		v.Result = domain.VerificationResult(resultStr)
		verification = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return verification, nil
}

// firstMissingSequence finds the first sequence number in [from,to] that
// has no corresponding row in rows (rows is a strict subset when this is
// called — some sequence in range was deleted or never re-materialized).
func firstMissingSequence(rows []chainRow, from, to int64) *int64 {
	present := make(map[int64]bool, len(rows))
	for _, r := range rows {
		present[r.seq] = true
	}
	for seq := from; seq <= to; seq++ {
		if !present[seq] {
			s := seq
			return &s
		}
	}
	// Every expected sequence is present yet the count still differed —
	// should be unreachable given the loop above, but fail safe with the
	// range start rather than a nil that would violate the CHECK constraint.
	s := from
	return &s
}

// ListVerifications returns the verification history for an archive, scoped to
// tenantID. Ownership of the archive is checked first, so an archive belonging
// to another tenant yields ErrArchiveNotFound rather than an empty list —
// a tenant cannot enumerate another tenant's verification history. The query
// additionally carries an explicit tenant predicate on top of RLS.
func (s *PgStore) ListVerifications(ctx context.Context, archiveID, tenantID string) ([]domain.ArchiveVerification, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	var out []domain.ArchiveVerification
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := setTenantScope(ctx, tx, tenantID); err != nil {
			return err
		}

		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM audit_archives WHERE archive_id=$1 AND tenant_id=$2)`,
			archiveID, tenantID).Scan(&exists); err != nil {
			return fmt.Errorf("check archive ownership: %w", err)
		}
		if !exists {
			return domain.ErrArchiveNotFound
		}

		rows, err := tx.Query(ctx, `
			SELECT verification_id, archive_id, tenant_id, verified_by_principal_id, verified_at, result, first_divergent_sequence
			FROM audit_archive_verifications WHERE archive_id=$1 AND tenant_id=$2 ORDER BY verified_at DESC`, archiveID, tenantID)
		if err != nil {
			return fmt.Errorf("list verifications: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.ArchiveVerification
			var resultStr string
			if err := rows.Scan(&v.VerificationID, &v.ArchiveID, &v.TenantID, &v.VerifiedByPrincipalID, &v.VerifiedAt, &resultStr, &v.FirstDivergentSequence); err != nil {
				return err
			}
			v.Result = domain.VerificationResult(resultStr)
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
