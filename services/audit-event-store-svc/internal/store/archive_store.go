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
type ArchiveStore interface {
	CreateArchive(ctx context.Context, p domain.CreateArchiveParams) (*domain.Archive, error)
	GetArchive(ctx context.Context, archiveID string) (*domain.Archive, error)
	VerifyArchive(ctx context.Context, p domain.VerifyArchiveParams) (*domain.ArchiveVerification, error)
	ListVerifications(ctx context.Context, archiveID string) ([]domain.ArchiveVerification, error)
}

type chainRow struct {
	seq      int64
	payload  string
	previous *string
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
		`SELECT sequence_number, payload_hash, previous_event_hash FROM audit_events
		 WHERE sequence_number BETWEEN $1 AND $2 ORDER BY sequence_number ASC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read chain range: %w", err)
	}
	defer rows.Close()
	var out []chainRow
	for rows.Next() {
		var r chainRow
		if err := rows.Scan(&r.seq, &r.payload, &r.previous); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// verifyLinks walks rows checking two things: (1) sequence numbers are
// contiguous (this chain is designed to never have gaps — see store.go's
// own nextSeq comment — so a gap here means a row is missing, not that a
// gap was ever legitimate), and (2) each row's previous_event_hash equals
// the prior row's payload_hash. Returns the sequence number of the first
// row that fails either check, or nil if the whole range is intact.
func verifyLinks(rows []chainRow) *int64 {
	for i := 1; i < len(rows); i++ {
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
	if p.ToSequence < p.FromSequence {
		return nil, domain.ErrInvalidRange
	}

	var archive *domain.Archive
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
				(archive_id, from_sequence, to_sequence, event_count, archive_digest,
				 first_event_hash, last_event_hash, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING archive_id, from_sequence, to_sequence, event_count, archive_digest,
				first_event_hash, last_event_hash, created_by_principal_id, created_at`,
			archiveID, rows[0].seq, rows[len(rows)-1].seq, int64(len(rows)), digest,
			rows[0].payload, rows[len(rows)-1].payload, p.CreatedByPrincipalID)

		a := &domain.Archive{}
		if err := row.Scan(&a.ArchiveID, &a.FromSequence, &a.ToSequence, &a.EventCount, &a.ArchiveDigest,
			&a.FirstEventHash, &a.LastEventHash, &a.CreatedByPrincipalID, &a.CreatedAt); err != nil {
			return fmt.Errorf("insert archive: %w", err)
		}
		archive = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return archive, nil
}

func (s *PgStore) GetArchive(ctx context.Context, archiveID string) (*domain.Archive, error) {
	a := &domain.Archive{}
	err := s.pool.QueryRow(ctx, `
		SELECT archive_id, from_sequence, to_sequence, event_count, archive_digest,
			first_event_hash, last_event_hash, created_by_principal_id, created_at
		FROM audit_archives WHERE archive_id=$1`, archiveID).
		Scan(&a.ArchiveID, &a.FromSequence, &a.ToSequence, &a.EventCount, &a.ArchiveDigest,
			&a.FirstEventHash, &a.LastEventHash, &a.CreatedByPrincipalID, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrArchiveNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// VerifyArchive independently re-derives today's truth about the archived
// range rather than trusting the stored claim — it never compares against
// a per-row snapshot (none is kept; audit_events rows are immutable by
// their own triggers, so keeping one would be redundant), it recomputes
// the same two checks CreateArchive made, live, and compares outcomes:
//
//  1. Row count: fewer rows than archived means something in range is gone.
//  2. Link integrity: same contiguous-sequence + hash-link walk.
//  3. Digest: the final defense-in-depth check, catching a divergence that
//     neither (1) nor (2) would (in practice: content of a stored payload
//     itself changing despite the immutability triggers).
//
// Every call — VERIFIED or DIVERGED — is itself recorded permanently.
func (s *PgStore) VerifyArchive(ctx context.Context, p domain.VerifyArchiveParams) (*domain.ArchiveVerification, error) {
	archive, err := s.GetArchive(ctx, p.ArchiveID)
	if err != nil {
		return nil, err
	}

	var verification *domain.ArchiveVerification
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
				(verification_id, archive_id, verified_by_principal_id, result, first_divergent_sequence)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING verification_id, archive_id, verified_by_principal_id, verified_at, result, first_divergent_sequence`,
			id, archive.ArchiveID, p.VerifiedByPrincipalID, string(result), divergedAt)

		v := &domain.ArchiveVerification{}
		var resultStr string
		if err := row.Scan(&v.VerificationID, &v.ArchiveID, &v.VerifiedByPrincipalID, &v.VerifiedAt, &resultStr, &v.FirstDivergentSequence); err != nil {
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

func (s *PgStore) ListVerifications(ctx context.Context, archiveID string) ([]domain.ArchiveVerification, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT verification_id, archive_id, verified_by_principal_id, verified_at, result, first_divergent_sequence
		FROM audit_archive_verifications WHERE archive_id=$1 ORDER BY verified_at DESC`, archiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ArchiveVerification
	for rows.Next() {
		var v domain.ArchiveVerification
		var resultStr string
		if err := rows.Scan(&v.VerificationID, &v.ArchiveID, &v.VerifiedByPrincipalID, &v.VerifiedAt, &resultStr, &v.FirstDivergentSequence); err != nil {
			return nil, err
		}
		v.Result = domain.VerificationResult(resultStr)
		out = append(out, v)
	}
	return out, rows.Err()
}
