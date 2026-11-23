package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

// The one-off account-identifier backfill: tokenize every destination
// proposed raw before TOKENIZATION_ENFORCED, without touching
// payee_destinations.
//
// Run by cmd/tokenize-backfill. A tool, not a migration: each row needs a
// round trip through key-management-svc, which plain SQL cannot make, and it
// must be run deliberately by someone holding the grants it needs.

// ErrBackfillRoundTrip means a token did not detokenize back to the exact raw
// value it was issued for. The whole backfill stops on it.
var ErrBackfillRoundTrip = errors.New("backfill: token does not round-trip to the original value")

// ErrBackfillNeedsAllRows means the backfill's database role is subject to
// row-level security, so it would see only some tenants' rows and could
// report a successful backfill having tokenized nothing.
var ErrBackfillNeedsAllRows = errors.New("backfill: database role must be SUPERUSER or BYPASSRLS to see every tenant's rows")

// ErrBackfillNoTenant means a destination has no tenant (tenant_id NULL), so
// no token can be bound to it (SEC-T29). The backfill stops rather than
// skip it, leaving its raw identifier as the one row nobody noticed.
var ErrBackfillNoTenant = errors.New("backfill: destination has no tenant to bind a token to")

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
	RunID      string
	Candidates int // destinations that had no token row
	Tokenized  int // token rows written (0 on a dry run)
	DryRun     bool
}

type identifierRow struct {
	destinationID, legalEntityID, raw string
	tenantID                          *string
	token                             string
}

// BackfillIdentifierTokens tokenizes every legacy (raw) destination, with the
// same guarantees as banking-connector-svc's account-number backfill:
//
//  1. Round trip first: each row is tokenized, then the token detokenized
//     and compared exactly with the raw identifier, before anything is
//     written.
//  2. All or nothing: every row is verified before any is written; any
//     failure writes NOTHING, and the token rows are inserted in one
//     transaction.
//  3. Re-runnable: each row's Idempotency-Key is derived from its id and the
//     principal, so a re-run reuses the tokens a failed run minted, and a
//     finished row is never a candidate again.
//  4. payee_destinations is never updated: 000002's immutability trigger is
//     untouched, account_identifier is never overwritten, and SUSPENDED and
//     SUPERSEDED destinations — which that trigger refuses to update at all
//     — are backfilled like any other.
func BackfillIdentifierTokens(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, opt BackfillOptions) (BackfillReport, error) {
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
		SELECT d.destination_id::text, d.tenant_id::text, d.legal_entity_id, d.account_identifier
		FROM payee_destinations d
		WHERE NOT EXISTS (SELECT 1 FROM payee_destination_identifier_tokens t WHERE t.destination_id = d.destination_id)
		ORDER BY d.created_at, d.destination_id`)
	if err != nil {
		return report, fmt.Errorf("backfill: list legacy destinations: %w", err)
	}
	var legacy []identifierRow
	for rows.Next() {
		var r identifierRow
		if err := rows.Scan(&r.destinationID, &r.tenantID, &r.legalEntityID, &r.raw); err != nil {
			rows.Close()
			return report, fmt.Errorf("backfill: read legacy destination: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("backfill: list legacy destinations: %w", err)
	}
	report.Candidates = len(legacy)

	// Phase one: tokenize and verify every row. Nothing is written here.
	for i := range legacy {
		r := &legacy[i]
		if r.tenantID == nil || *r.tenantID == "" {
			return report, fmt.Errorf("%w: destination %s — nothing written", ErrBackfillNoTenant, r.destinationID)
		}
		scope := tokenization.Scope{
			TenantID:      *r.tenantID,
			PrincipalID:   opt.PrincipalID,
			LegalEntityID: r.legalEntityID,
			CorrelationID: "tokenize-backfill:" + report.RunID,
			// Per row AND per principal. Per row so a re-run gets back the
			// tokens a failed run minted; per principal because the vault
			// answers a key reused by a DIFFERENT principal with 409 — so a
			// key without the principal let one failed attempt by the wrong
			// principal (found in the 2026-11-13 live run) block every later
			// run by the right one.
			IdempotencyKey: "tokenize-backfill:payee_destinations:" + r.destinationID + ":" + opt.PrincipalID,
		}
		token, err := t.Tokenize(ctx, scope, tokenization.ClassificationRestricted, r.raw)
		if err != nil {
			return report, fmt.Errorf("backfill: tokenize destination %s (tenant %s, legal entity %s): %w — nothing written",
				r.destinationID, *r.tenantID, r.legalEntityID, err)
		}
		back, err := t.Detokenize(ctx, scope, token)
		if err != nil {
			return report, fmt.Errorf("backfill: verify destination %s (tenant %s, legal entity %s): %w — nothing written",
				r.destinationID, *r.tenantID, r.legalEntityID, err)
		}
		if back != r.raw {
			return report, fmt.Errorf("%w: destination %s — nothing written", ErrBackfillRoundTrip, r.destinationID)
		}
		r.token = token
	}
	if opt.DryRun || len(legacy) == 0 {
		return report, nil
	}

	// Phase two: every row verified; write them all, or none.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("backfill: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	for _, r := range legacy {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", *r.tenantID); err != nil {
			return report, fmt.Errorf("backfill: set tenant: %w", err)
		}
		if err := insertIdentifierToken(ctx, tx, r.destinationID, *r.tenantID, r.token, "BACKFILL"); err != nil {
			return report, fmt.Errorf("backfill: destination %s: %w — nothing written", r.destinationID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("backfill: commit: %w — nothing written", err)
	}
	report.Tokenized = len(legacy)
	return report, nil
}
