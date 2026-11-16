package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

// The one-off account-number backfill: tokenize every connection written raw
// before TOKENIZATION_ENFORCED, without touching bank_connections.
//
// Run by cmd/tokenize-backfill (see its doc for how and when). It is a tool,
// not a migration, because each row needs a round trip through
// key-management-svc, which plain SQL cannot make, and because it must be run
// deliberately — by someone holding the grants it needs — rather than as a
// side effect of applying schema.

// ErrBackfillRoundTrip means a token did not detokenize back to the exact raw
// value it was issued for. The whole backfill stops on it.
var ErrBackfillRoundTrip = errors.New("backfill: token does not round-trip to the original value")

// ErrBackfillNeedsAllRows means the backfill's database role is subject to
// row-level security, so it would see only some (or no) tenants' rows and
// could report a successful backfill having tokenized nothing.
var ErrBackfillNeedsAllRows = errors.New("backfill: database role must be SUPERUSER or BYPASSRLS to see every tenant's rows")

// BackfillOptions configures one backfill run.
type BackfillOptions struct {
	// PrincipalID is who the vault records as tokenizing and verifying. It
	// needs TOKEN_CREATE and TOKEN_DETOKENIZE at every legal entity being
	// backfilled.
	PrincipalID string
	// DryRun tokenizes and verifies every row but writes nothing.
	DryRun bool
}

// BackfillReport is what a run did.
type BackfillReport struct {
	RunID string
	// Candidates is the number of connections that had no token row.
	Candidates int
	// SkippedEmpty is how many of those had no account number to tokenize
	// (BNK-02 connections initiated without one).
	SkippedEmpty int
	// Tokenized is the number of token rows written (0 on a dry run).
	Tokenized int
	DryRun    bool
}

type backfillRow struct {
	conn domain.BankConnection
	raw  string
}

// BackfillAccountTokens tokenizes every legacy (raw) connection.
//
// The guarantees, in order:
//
//  1. Round trip first. Each row is tokenized, then the token is
//     detokenized and compared with the original raw value, before anything
//     is written. The comparison is exact.
//  2. All or nothing. Every row is verified before any is written; any
//     failure — a refused tokenize, a refused detokenize, a mismatch —
//     returns an error and writes NOTHING. The token rows are then inserted
//     in one transaction, so a database failure part-way also writes nothing.
//  3. Re-runnable. Each row's Idempotency-Key is derived from its id and the
//     principal, so a re-run after a failure gets the same tokens back from
//     the vault rather than minting new ones, and a completed row (one with a
//     token row) is never a candidate again.
//  4. bank_connections is never updated. Its raw account_number stays (it
//     is evidence, and dropping it is a later decision); the existing
//     REVOKED-row trigger is untouched, and revoked connections are
//     backfilled like any other.
func BackfillAccountTokens(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, opt BackfillOptions) (BackfillReport, error) {
	report := BackfillReport{RunID: uuid.NewString(), DryRun: opt.DryRun}
	if t == nil {
		return report, ErrTokenizationNotConfigured
	}
	if opt.PrincipalID == "" {
		return report, errors.New("backfill: a principal is required")
	}
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		return report, fmt.Errorf("backfill: read database role: %w", err)
	}
	if !super && !bypass {
		return report, ErrBackfillNeedsAllRows
	}

	rows, err := pool.Query(ctx, `
		SELECT c.connection_id, c.tenant_id, c.legal_entity_id, c.account_number
		FROM bank_connections c
		WHERE NOT EXISTS (SELECT 1 FROM bank_connection_account_tokens t WHERE t.connection_id = c.connection_id)
		ORDER BY c.created_at, c.connection_id`)
	if err != nil {
		return report, fmt.Errorf("backfill: list legacy connections: %w", err)
	}
	var legacy []backfillRow
	for rows.Next() {
		var r backfillRow
		if err := rows.Scan(&r.conn.ConnectionID, &r.conn.TenantID, &r.conn.LegalEntityID, &r.raw); err != nil {
			rows.Close()
			return report, fmt.Errorf("backfill: read legacy connection: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("backfill: list legacy connections: %w", err)
	}
	report.Candidates = len(legacy)

	// Phase one: tokenize and verify every row. Nothing is written here.
	var verified []*domain.BankConnection
	for i := range legacy {
		r := &legacy[i]
		if r.raw == "" {
			report.SkippedEmpty++
			continue
		}
		scope := tokenization.Scope{
			TenantID:      r.conn.TenantID,
			PrincipalID:   opt.PrincipalID,
			LegalEntityID: r.conn.LegalEntityID,
			CorrelationID: "tokenize-backfill:" + report.RunID,
			// Per row AND per principal. Per row so a re-run gets back the
			// tokens a failed run minted; per principal because the vault
			// answers a key reused by a DIFFERENT principal with 409 — so a
			// key without the principal let one failed attempt by the wrong
			// principal (found in the 2026-11-13 live run) block every later
			// run by the right one.
			IdempotencyKey: "tokenize-backfill:bank_connections:" + r.conn.ConnectionID + ":" + opt.PrincipalID,
		}
		token, err := t.Tokenize(ctx, scope, tokenization.ClassificationRestricted, r.raw)
		if err != nil {
			return report, fmt.Errorf("backfill: tokenize connection %s (tenant %s, legal entity %s): %w — nothing written",
				r.conn.ConnectionID, r.conn.TenantID, r.conn.LegalEntityID, err)
		}
		back, err := t.Detokenize(ctx, scope, token)
		if err != nil {
			return report, fmt.Errorf("backfill: verify connection %s (tenant %s, legal entity %s): %w — nothing written",
				r.conn.ConnectionID, r.conn.TenantID, r.conn.LegalEntityID, err)
		}
		if back != r.raw {
			return report, fmt.Errorf("%w: connection %s — nothing written", ErrBackfillRoundTrip, r.conn.ConnectionID)
		}
		c := r.conn
		c.AccountNumberToken, c.AccountNumberMasked = token, domain.MaskAccountNumber(r.raw)
		verified = append(verified, &c)
	}
	if opt.DryRun || len(verified) == 0 {
		return report, nil
	}

	// Phase two: every row verified; write them all, or none.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("backfill: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	for _, c := range verified {
		// Scoped like a request, so the insert satisfies the token table's
		// WITH CHECK even for a role that does not bypass RLS.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", c.TenantID); err != nil {
			return report, fmt.Errorf("backfill: set tenant: %w", err)
		}
		if err := insertAccountToken(ctx, tx, c, "BACKFILL"); err != nil {
			return report, fmt.Errorf("backfill: connection %s: %w — nothing written", c.ConnectionID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("backfill: commit: %w — nothing written", err)
	}
	report.Tokenized = len(verified)
	return report, nil
}
