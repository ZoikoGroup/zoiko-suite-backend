package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/tokenization"
)

// The one-off raw-column scrub (SPEC_DEVIATIONS.md G6, phase 2): null
// bank_connections.account_number for every connection that already has a
// verified token in bank_connection_account_tokens (migration 010), leaving
// the token row and everything else untouched.
//
// This is deliberately the mirror image of cmd/tokenize-backfill's caution:
// that command tokenizes a row and writes nothing until the new token
// round-trips. This one goes further, because the thing it is about to
// destroy — the raw value — cannot be recovered afterward if the token
// turns out not to reproduce it: for every row it RE-VERIFIES the round trip
// itself, independently of whatever the backfill or the original write
// already checked, immediately before nulling that row, in the same
// transaction as the null write.
//
// Run by cmd/scrub-legacy-raw (see its doc for how and when).

// ErrScrubNeedsAllRows is ErrBackfillNeedsAllRows's twin: the scrub's
// database role must bypass RLS, or it would see only some tenants' rows and
// could report a successful run having scrubbed nothing.
var ErrScrubNeedsAllRows = errors.New("scrub: database role must be SUPERUSER or BYPASSRLS to see every tenant's rows")

// ScrubOptions configures one scrub run.
type ScrubOptions struct {
	// PrincipalID is who the vault records as detokenizing for this
	// re-verification. It needs TOKEN_DETOKENIZE at every legal entity being
	// scrubbed — the same grant the backfill needs, held by the same narrow
	// role (deployments/scripts/seed-demo-rbac.ps1's TOKENIZATION_BACKFILL).
	PrincipalID string
	// DryRun re-verifies every row's round trip but writes nothing.
	DryRun bool
}

// ScrubReport is what a run did.
type ScrubReport struct {
	RunID string
	// Candidates is the number of connections with a verified token whose
	// raw account_number was still populated.
	Candidates int
	// SkippedNoToken is connections with no row in
	// bank_connection_account_tokens at all — never tokenized, so never a
	// candidate. Left exactly as they were: the pre-existing, documented
	// gap this run does not close.
	SkippedNoToken int
	// SkippedRevoked is candidates that are REVOKED. bank_connections'
	// own immutability trigger (migration 003) refuses ANY update to a
	// REVOKED row, full stop — the same reason this scrub cannot touch
	// them as a legacy backfill could not have overwritten the column in
	// place. Left exactly as they were.
	SkippedRevoked int
	// Mismatched is candidates whose re-verified round trip did not
	// reproduce the raw value. Left exactly as they were and reported by
	// connection ID in Mismatches — this is the loud failure the task
	// calls for, not a silent skip.
	Mismatches []string
	// Scrubbed is the number of rows whose account_number was nulled (0 on
	// a dry run).
	Scrubbed int
	DryRun   bool
}

type scrubRow struct {
	connectionID  string
	tenantID      string
	legalEntityID string
	token         string
}

// ScrubLegacyRawAccountNumbers nulls account_number for every tokenized,
// non-REVOKED connection, one row at a time:
//
//  1. Lock the row (SELECT ... FOR UPDATE) inside a fresh transaction, so a
//     concurrent write cannot race the check.
//  2. Detokenize the row's own stored token and byte-compare the result
//     against the raw value sitting in that same locked row right now.
//  3. Only on an exact match: UPDATE account_number to NULL, in that same
//     transaction, then commit. On any mismatch, or any error, roll back
//     and move on — this row is reported and left untouched; it never
//     blocks any other row.
//
// Safe to re-run: a row whose account_number is already NULL fails the
// candidate WHERE clause and is not reselected.
func ScrubLegacyRawAccountNumbers(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, opt ScrubOptions) (ScrubReport, error) {
	report := ScrubReport{RunID: uuid.NewString(), DryRun: opt.DryRun}
	if t == nil {
		return report, ErrTokenizationNotConfigured
	}
	if opt.PrincipalID == "" {
		return report, errors.New("scrub: a principal is required")
	}
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		return report, fmt.Errorf("scrub: read database role: %w", err)
	}
	if !super && !bypass {
		return report, ErrScrubNeedsAllRows
	}

	if n, err := countNoToken(ctx, pool); err != nil {
		return report, fmt.Errorf("scrub: count untokenized connections: %w", err)
	} else {
		report.SkippedNoToken = n
	}

	rows, err := pool.Query(ctx, `
		SELECT c.connection_id, c.tenant_id, c.legal_entity_id, c.status, t.account_number_token
		FROM bank_connections c
		JOIN bank_connection_account_tokens t ON t.connection_id = c.connection_id
		WHERE c.account_number IS NOT NULL
		  -- A WRITE-source token means this row was tokenized AT CREATION,
		  -- under TOKENIZATION_ENFORCED=true: account_number already holds
		  -- the TOKEN, not a raw value (see populateAliases in handler.go),
		  -- by design, from the moment it was written. There is no raw
		  -- value in that column to scrub, and detokenizing its own token
		  -- would never equal the column's current content (the token
		  -- string itself) -- that is a category error in candidate
		  -- selection, not a real round-trip failure, and was found live:
		  -- a freshly tokenized connection reported a false "mismatch"
		  -- before this filter existed. Only BACKFILL rows are legacy
		  -- connections whose column still holds the original raw number.
		  AND t.source = 'BACKFILL'
		ORDER BY c.created_at, c.connection_id`)
	if err != nil {
		return report, fmt.Errorf("scrub: list candidates: %w", err)
	}
	var candidates []scrubRow
	for rows.Next() {
		var r scrubRow
		var status string
		if err := rows.Scan(&r.connectionID, &r.tenantID, &r.legalEntityID, &status, &r.token); err != nil {
			rows.Close()
			return report, fmt.Errorf("scrub: read candidate: %w", err)
		}
		if status == "REVOKED" {
			report.SkippedRevoked++
			continue
		}
		candidates = append(candidates, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("scrub: list candidates: %w", err)
	}
	report.Candidates = len(candidates)

	for _, r := range candidates {
		scrubbed, err := scrubOneConnection(ctx, pool, t, r, report.RunID, opt)
		if err != nil {
			return report, fmt.Errorf("scrub: connection %s: %w", r.connectionID, err)
		}
		if scrubbed {
			report.Scrubbed++
		} else {
			report.Mismatches = append(report.Mismatches, r.connectionID)
		}
	}
	return report, nil
}

// scrubOneConnection re-verifies and, on success, nulls exactly one row in
// its own transaction. It returns (true, nil) on a successful scrub (or dry
// run), (false, nil) on a round-trip mismatch — the row is left untouched
// and the caller must report it loudly — and a non-nil error only for an
// infrastructure failure (the vault unreachable, the database unavailable).
func scrubOneConnection(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, r scrubRow, runID string, opt ScrubOptions) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	var raw string
	if err := tx.QueryRow(ctx,
		`SELECT account_number FROM bank_connections WHERE connection_id = $1 FOR UPDATE`,
		r.connectionID,
	).Scan(&raw); err != nil {
		return false, fmt.Errorf("lock row: %w", err)
	}
	if raw == "" {
		// Scrubbed by a concurrent run since the candidate list was built.
		return true, nil
	}

	scope := tokenization.Scope{
		TenantID:       r.tenantID,
		PrincipalID:    opt.PrincipalID,
		LegalEntityID:  r.legalEntityID,
		CorrelationID:  "scrub-legacy-raw:" + runID,
		IdempotencyKey: "scrub-legacy-raw:bank_connections:" + r.connectionID + ":" + runID,
	}
	back, err := t.Detokenize(ctx, scope, r.token)
	if err != nil {
		return false, fmt.Errorf("re-verify detokenize: %w", err)
	}
	if back != raw {
		// LOUD, not silent: this is the one case the task exists to catch.
		// Leave the row exactly as it is and let the caller log it.
		return false, nil
	}

	if opt.DryRun {
		return true, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE bank_connections SET account_number = NULL WHERE connection_id = $1`,
		r.connectionID,
	); err != nil {
		return false, fmt.Errorf("null account_number: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

func countNoToken(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM bank_connections c
		WHERE NOT EXISTS (SELECT 1 FROM bank_connection_account_tokens t WHERE t.connection_id = c.connection_id)
		  AND c.account_number IS NOT NULL AND c.account_number <> ''`,
	).Scan(&n)
	return n, err
}
