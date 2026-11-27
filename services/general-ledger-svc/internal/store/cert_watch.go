package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// CertWatchRepo is the persistence of the certification watch (migration 000036): the certifications the ledger relies on, and
// the flags raised on what rests on one that was withdrawn. Every method runs in one tenant-scoped transaction under row-level
// security, with the tenant also named in every predicate.
type CertWatchRepo struct{ pool *pgxpool.Pool }

func NewCertWatchRepo(pool *pgxpool.Pool) *CertWatchRepo { return &CertWatchRepo{pool: pool} }

var (
	_ certwatch.Store       = (*CertWatchRepo)(nil)
	_ certwatch.ScopeLister = (*CertWatchRepo)(nil)
	_ certwatch.Locker      = (*CertWatchRepo)(nil)
)

func (r *CertWatchRepo) tx(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	if _, err := uuid.Parse(tenant); err != nil {
		return fmt.Errorf("%w: tenant", certwatch.ErrInvalid)
	}
	t, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit(ctx)
}

func needEntity(entity string) error {
	if _, err := uuid.Parse(entity); err != nil {
		return fmt.Errorf("%w: legal_entity_id must be a UUID", certwatch.ErrInvalid)
	}
	return nil
}

func (r *CertWatchRepo) Watch(ctx context.Context, tenant, entity string, ref certreliance.Ref) error {
	if err := needEntity(entity); err != nil {
		return err
	}
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		_, err := t.Exec(ctx, `INSERT INTO certification_watch (tenant_id, legal_entity_id, certification_service, run_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenant, entity, ref.Service, ref.RunID)
		return err
	})
}

func (r *CertWatchRepo) ListWatched(ctx context.Context, tenant, entity string) ([]certwatch.Watched, error) {
	if err := needEntity(entity); err != nil {
		return nil, err
	}
	var out []certwatch.Watched
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT certification_service, run_id, COALESCE(last_status, ''), withdrawn_at, COALESCE(revocation_id, ''),
				COALESCE(withdrawal_reason, '')
			FROM certification_watch WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY first_relied_at, certification_service, run_id`, tenant, entity)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w := certwatch.Watched{LegalEntityID: entity}
			if err := rows.Scan(&w.Ref.Service, &w.Ref.RunID, &w.LastStatus, &w.WithdrawnAt, &w.RevocationID, &w.Reason); err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

func (r *CertWatchRepo) RecordCheck(ctx context.Context, tenant, entity string, ref certreliance.Ref, state string) error {
	if err := needEntity(entity); err != nil {
		return err
	}
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		_, err := t.Exec(ctx, `UPDATE certification_watch SET last_status = $5, last_checked_at = now()
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND certification_service = $3 AND run_id = $4 AND withdrawn_at IS NULL`,
			tenant, entity, ref.Service, ref.RunID, state)
		return err
	})
}

// RecordWithdrawal: see certwatch.Store. The first revocation recorded for a certification is the one that stays; later calls
// (a re-scan) use what is stored, so a retry can never overwrite why a certification was withdrawn.
func (r *CertWatchRepo) RecordWithdrawal(ctx context.Context, tenant, entity string, ref certreliance.Ref, revocationID, reason string) (int, error) {
	if err := needEntity(entity); err != nil {
		return 0, err
	}
	if strings.TrimSpace(revocationID) == "" || strings.TrimSpace(reason) == "" {
		return 0, fmt.Errorf("%w: a withdrawal needs the revocation and its reason", certwatch.ErrInvalid)
	}
	created := 0
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `UPDATE certification_watch SET last_status = 'REVOKED', last_checked_at = now(), withdrawn_at = now(),
				revocation_id = $5, withdrawal_reason = $6
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND certification_service = $3 AND run_id = $4 AND withdrawn_at IS NULL`,
			tenant, entity, ref.Service, ref.RunID, revocationID, reason); err != nil {
			return err
		}
		var storedRev, storedReason string
		if err := t.QueryRow(ctx, `SELECT revocation_id, withdrawal_reason FROM certification_watch
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND certification_service = $3 AND run_id = $4`,
			tenant, entity, ref.Service, ref.RunID).Scan(&storedRev, &storedReason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s is not on the watch list for this entity", certwatch.ErrInvalid, ref)
			}
			return err
		}
		// Manual journals that cite it. A journal that came from a kernel posting is flagged as that posting instead, not twice.
		tag, err := t.Exec(ctx, `
			INSERT INTO certification_flags (flag_id, tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id,
				journal_id, revocation_id, reason)
			SELECT gen_random_uuid(), j.tenant_id, j.legal_entity_id, $3, $4, 'journal', j.journal_id::text, j.journal_id, $5, $6
			FROM journal_headers j
			WHERE j.tenant_id = $1 AND j.legal_entity_id = $2 AND j.evidence_refs @> ARRAY[$7]::text[] AND j.status IN ('FINALIZED', 'REVERSED')
			  AND NOT EXISTS (SELECT 1 FROM event_posting_links l WHERE l.tenant_id = j.tenant_id AND l.journal_id = j.journal_id)
			ON CONFLICT (tenant_id, certification_service, run_id, subject_type, subject_id) DO NOTHING`,
			tenant, entity, ref.Service, ref.RunID, storedRev, storedReason, ref.String())
		if err != nil {
			return fmt.Errorf("flag journals: %w", err)
		}
		created += int(tag.RowsAffected())
		// Kernel postings of accounting events that cite it, named exactly as their reliance was recorded: event:book:basis.
		tag, err = t.Exec(ctx, `
			INSERT INTO certification_flags (flag_id, tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id,
				journal_id, revocation_id, reason)
			SELECT gen_random_uuid(), e.tenant_id, e.legal_entity_id, $3, $4, 'accounting-posting',
				l.accounting_event_id::text || ':' || l.accounting_book_id::text || ':' || l.posting_basis, l.journal_id, $5, $6
			FROM accounting_events e
			JOIN event_posting_links l ON l.tenant_id = e.tenant_id AND l.accounting_event_id = e.accounting_event_id
			WHERE e.tenant_id = $1 AND e.legal_entity_id = $2 AND e.evidence_refs @> to_jsonb(ARRAY[$7]::text[])
			ON CONFLICT (tenant_id, certification_service, run_id, subject_type, subject_id) DO NOTHING`,
			tenant, entity, ref.Service, ref.RunID, storedRev, storedReason, ref.String())
		if err != nil {
			return fmt.Errorf("flag postings: %w", err)
		}
		created += int(tag.RowsAffected())
		return nil
	})
	return created, err
}

const flagColumns = `flag_id, legal_entity_id, certification_service, run_id, subject_type, subject_id, journal_id, revocation_id, reason, status,
	raised_at, COALESCE(acknowledged_by, ''), acknowledged_at, COALESCE(acknowledgement, ''), COALESCE(remediation_journal_id::text, '')`

func scanFlag(row pgx.Row) (*certwatch.Flag, error) {
	var f certwatch.Flag
	var service, run string
	if err := row.Scan(&f.FlagID, &f.LegalEntityID, &service, &run, &f.SubjectType, &f.SubjectID, &f.JournalID, &f.RevocationID, &f.Reason, &f.Status,
		&f.RaisedAt, &f.AcknowledgedBy, &f.AcknowledgedAt, &f.Acknowledgement, &f.RemediationJournalID); err != nil {
		return nil, err
	}
	f.Certification = certreliance.Ref{Service: service, RunID: run}.String()
	return &f, nil
}

func (r *CertWatchRepo) ListFlags(ctx context.Context, tenant, entity, status string) ([]certwatch.Flag, error) {
	if err := needEntity(entity); err != nil {
		return nil, err
	}
	if status != "" && status != certwatch.FlagOpen && status != certwatch.FlagAcknowledged {
		return nil, fmt.Errorf("%w: status must be OPEN or ACKNOWLEDGED", certwatch.ErrInvalid)
	}
	out := []certwatch.Flag{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+flagColumns+` FROM certification_flags
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR status = $3) ORDER BY raised_at, flag_id`, tenant, entity, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			f, err := scanFlag(rows)
			if err != nil {
				return err
			}
			out = append(out, *f)
		}
		return rows.Err()
	})
	return out, err
}

func (r *CertWatchRepo) GetFlag(ctx context.Context, tenant, flagID string) (*certwatch.Flag, error) {
	if _, err := uuid.Parse(flagID); err != nil {
		return nil, certwatch.ErrFlagNotFound
	}
	var out *certwatch.Flag
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		f, err := scanFlag(t.QueryRow(ctx, `SELECT `+flagColumns+` FROM certification_flags WHERE tenant_id = $1 AND flag_id = $2`, tenant, flagID))
		if errors.Is(err, pgx.ErrNoRows) {
			return certwatch.ErrFlagNotFound
		}
		out = f
		return err
	})
	return out, err
}

// Acknowledge records a person's decision on a flag. The remediation journal, when named, must be a journal of the same tenant and
// legal entity as the flagged one.
func (r *CertWatchRepo) Acknowledge(ctx context.Context, tenant, flagID, by, acknowledgement, remediationJournalID string) (*certwatch.Flag, error) {
	if _, err := uuid.Parse(flagID); err != nil {
		return nil, certwatch.ErrFlagNotFound
	}
	if strings.TrimSpace(by) == "" || strings.TrimSpace(acknowledgement) == "" || len(acknowledgement) > 2000 {
		return nil, fmt.Errorf("%w: an acknowledgement needs who and a reason (at most 2000 characters)", certwatch.ErrInvalid)
	}
	var remediation any
	if remediationJournalID != "" {
		if _, err := uuid.Parse(remediationJournalID); err != nil {
			return nil, fmt.Errorf("%w: remediation_journal_id must be a journal id", certwatch.ErrInvalid)
		}
		remediation = remediationJournalID
	}
	var out *certwatch.Flag
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var status, entity string
		err := t.QueryRow(ctx, `SELECT status, legal_entity_id::text FROM certification_flags WHERE tenant_id = $1 AND flag_id = $2 FOR UPDATE`,
			tenant, flagID).Scan(&status, &entity)
		if errors.Is(err, pgx.ErrNoRows) {
			return certwatch.ErrFlagNotFound
		}
		if err != nil {
			return err
		}
		if status != certwatch.FlagOpen {
			return certwatch.ErrFlagAlreadyAcknowledged
		}
		if remediation != nil {
			var ok bool
			if err := t.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_headers WHERE tenant_id = $1 AND legal_entity_id = $2 AND journal_id = $3::uuid)`,
				tenant, entity, remediationJournalID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: the remediation journal is not a journal of the flagged entity", certwatch.ErrInvalid)
			}
		}
		f, err := scanFlag(t.QueryRow(ctx, `UPDATE certification_flags SET status = 'ACKNOWLEDGED', acknowledged_by = $3, acknowledged_at = now(),
				acknowledgement = $4, remediation_journal_id = $5::uuid
			WHERE tenant_id = $1 AND flag_id = $2 RETURNING `+flagColumns, tenant, flagID, by, strings.TrimSpace(acknowledgement), remediation))
		out = f
		return err
	})
	return out, err
}

// ListScopes lists every (tenant, legal entity) that has anything on the watch list, across tenants, for the scheduled sweep. It
// runs under the read-only capability of migration 000037 (a transaction-scoped setting a policy recognises, as the outbox relay's
// is) and selects only the two ids; each sweep then runs under ordinary tenant isolation.
func (r *CertWatchRepo) ListScopes(ctx context.Context) ([]certwatch.Scope, error) {
	t, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.certification_sweeper', 'true', true)"); err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT DISTINCT tenant_id::text, legal_entity_id::text FROM certification_watch ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []certwatch.Scope
	for rows.Next() {
		var s certwatch.Scope
		if err := rows.Scan(&s.TenantID, &s.LegalEntityID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// certSweepLockKey identifies the sweep's advisory lock; any constant unique to this job will do.
const certSweepLockKey int64 = 0x5a53_4357_5350 // "ZSCWSP"

// TryLock takes the sweep's session-level advisory lock on a dedicated connection, so that only one replica sweeps at a time. It
// returns ok=false, not an error, when another replica holds it. The lock is released by the returned function, and is released by
// the database if the connection or process dies, so a crashed replica never holds the sweep.
func (r *CertWatchRepo) TryLock(ctx context.Context) (func(), bool, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, certSweepLockKey).Scan(&got); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// Unlock on a context that outlives a cancelled run: a lock left held would stop every replica sweeping until the
		// connection is recycled.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, certSweepLockKey)
		conn.Release()
	}, true, nil
}

// ListWatchers lists every legal entity of the tenant that has the certification on its watch list, with the row as the watch holds
// it. A certification nobody here relies on is an empty list, not an error.
func (r *CertWatchRepo) ListWatchers(ctx context.Context, tenant string, ref certreliance.Ref) ([]certwatch.Watched, error) {
	var out []certwatch.Watched
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT legal_entity_id::text, COALESCE(last_status, ''), withdrawn_at, COALESCE(revocation_id, ''), COALESCE(withdrawal_reason, '')
			FROM certification_watch WHERE tenant_id = $1 AND certification_service = $2 AND run_id = $3 ORDER BY legal_entity_id`,
			tenant, ref.Service, ref.RunID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w := certwatch.Watched{Ref: ref}
			if err := rows.Scan(&w.LegalEntityID, &w.LastStatus, &w.WithdrawnAt, &w.RevocationID, &w.Reason); err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// OpenFlagStats reads, across tenants, how many flags are OPEN and how long the oldest has waited, for the scrape-time gauges. It runs
// under the same read-only capability as ListScopes (migrations 000037 and 000038) and reads only an aggregate, never a flag.
func (r *CertWatchRepo) OpenFlagStats(ctx context.Context) (certwatch.OpenFlagStats, error) {
	var out certwatch.OpenFlagStats
	t, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.certification_sweeper', 'true', true)"); err != nil {
		return out, err
	}
	var ageSeconds float64
	if err := t.QueryRow(ctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(raised_at)), 0)
		FROM certification_flags WHERE status = 'OPEN'`).Scan(&out.Open, &ageSeconds); err != nil {
		return out, err
	}
	out.Oldest = time.Duration(ageSeconds * float64(time.Second))
	return out, nil
}
