package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

// The one-off raw-column scrub (SPEC_DEVIATIONS.md B12, phase 2): null
// payee_destinations.account_identifier for every destination that already
// has a verified token in payee_destination_identifier_tokens, leaving the
// token row and everything else untouched. Mirrors
// banking-connector-svc/internal/store/scrub.go exactly; see its doc for the
// full reasoning.
//
// 000002_immutability.up.sql's reject_payee_destination_mutation forbids ANY
// change to account_identifier, for every row, unconditionally — the same
// guard that is the whole reason identifier tokens live in a side table
// instead of overwriting this column. Migration 000009 carves out exactly
// one exception for this command: account_identifier may move from a
// non-empty value to NULL, and only to NULL, once. Apply it first.
//
// Run by cmd/scrub-legacy-raw.

// ErrScrubNeedsAllRows is ErrBackfillNeedsAllRows's twin for the scrub.
var ErrScrubNeedsAllRows = errors.New("scrub: database role must be SUPERUSER or BYPASSRLS to see every tenant's rows")

// ScrubOptions configures one scrub run.
type ScrubOptions struct {
	// PrincipalID is who the vault records as detokenizing for this
	// re-verification. It needs TOKEN_DETOKENIZE at every legal entity being
	// scrubbed.
	PrincipalID string
	// DryRun re-verifies every row's round trip but writes nothing.
	DryRun bool
}

// ScrubReport is what a run did.
type ScrubReport struct {
	RunID string
	// Candidates is the number of destinations with a verified token whose
	// account_identifier was still populated.
	Candidates int
	// SkippedNoToken is destinations with no row in
	// payee_destination_identifier_tokens at all — never tokenized, so
	// never a candidate. Left exactly as they were: the pre-existing,
	// documented gap this run does not close.
	SkippedNoToken int
	// SkippedTerminal is candidates that are SUSPENDED or SUPERSEDED.
	// payee_destinations' own immutability trigger (migration 000002)
	// refuses ANY update to one of those, full stop. Left exactly as they
	// were.
	SkippedTerminal int
	// Mismatches is candidates whose re-verified round trip did not
	// reproduce the raw identifier, by destination ID — the loud failure
	// the task calls for, not a silent skip. Left exactly as they were.
	Mismatches []string
	// Scrubbed is the number of rows whose account_identifier was nulled
	// (0 on a dry run).
	Scrubbed int
	DryRun   bool
}

type scrubRow struct {
	destinationID, legalEntityID string
	tenantID                     *string
	token                        string
}

// ScrubIdentifierTokens nulls account_identifier for every tokenized,
// non-terminal destination, one row at a time: lock the row (SELECT ... FOR
// UPDATE) in a fresh transaction, detokenize its own stored token, byte-
// compare the result against the raw value sitting in that same locked row
// right now, and only on an exact match UPDATE it to NULL and commit. A
// mismatch or any error rolls back and moves on to the next row — this row
// is reported and left untouched; it never blocks any other row.
//
// Safe to re-run: a row whose account_identifier is already NULL fails the
// candidate WHERE clause and is not reselected.
func ScrubIdentifierTokens(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, opt ScrubOptions) (ScrubReport, error) {
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
		return report, fmt.Errorf("scrub: count untokenized destinations: %w", err)
	} else {
		report.SkippedNoToken = n
	}

	rows, err := pool.Query(ctx, `
		SELECT d.destination_id::text, d.tenant_id::text, d.legal_entity_id, d.status, t.account_identifier_token
		FROM payee_destinations d
		JOIN payee_destination_identifier_tokens t ON t.destination_id = d.destination_id
		WHERE d.account_identifier IS NOT NULL AND d.account_identifier <> ''
		  -- A WRITE-source token means this destination was tokenized AT
		  -- PROPOSAL, under TOKENIZATION_ENFORCED=true: account_identifier
		  -- already holds the TOKEN, not a raw value, from the moment it
		  -- was written. There is no raw value in that column to scrub --
		  -- found live, the same false "mismatch" as
		  -- banking-connector-svc's identical bug: detokenizing a WRITE
		  -- row's own token never equals the column's current content (the
		  -- token string itself). Only BACKFILL rows are legacy
		  -- destinations whose column still holds the original raw
		  -- identifier.
		  AND t.source = 'BACKFILL'
		ORDER BY d.created_at, d.destination_id`)
	if err != nil {
		return report, fmt.Errorf("scrub: list candidates: %w", err)
	}
	var candidates []scrubRow
	for rows.Next() {
		var r scrubRow
		var status string
		if err := rows.Scan(&r.destinationID, &r.tenantID, &r.legalEntityID, &status, &r.token); err != nil {
			rows.Close()
			return report, fmt.Errorf("scrub: read candidate: %w", err)
		}
		if status == "SUSPENDED" || status == "SUPERSEDED" {
			report.SkippedTerminal++
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
		scrubbed, err := scrubOneDestination(ctx, pool, t, r, report.RunID, opt)
		if err != nil {
			return report, fmt.Errorf("scrub: destination %s: %w", r.destinationID, err)
		}
		if scrubbed {
			report.Scrubbed++
		} else {
			report.Mismatches = append(report.Mismatches, r.destinationID)
		}
	}
	return report, nil
}

func scrubOneDestination(ctx context.Context, pool *pgxpool.Pool, t tokenization.Tokenizer, r scrubRow, runID string, opt ScrubOptions) (bool, error) {
	if r.tenantID == nil || *r.tenantID == "" {
		return false, fmt.Errorf("destination has no tenant to re-verify a token against")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", *r.tenantID); err != nil {
		return false, fmt.Errorf("set tenant: %w", err)
	}

	var raw string
	if err := tx.QueryRow(ctx,
		`SELECT account_identifier FROM payee_destinations WHERE destination_id = $1 FOR UPDATE`,
		r.destinationID,
	).Scan(&raw); err != nil {
		return false, fmt.Errorf("lock row: %w", err)
	}
	if raw == "" {
		// Scrubbed by a concurrent run since the candidate list was built.
		return true, nil
	}

	scope := tokenization.Scope{
		TenantID:       *r.tenantID,
		PrincipalID:    opt.PrincipalID,
		LegalEntityID:  r.legalEntityID,
		CorrelationID:  "scrub-legacy-raw:" + runID,
		IdempotencyKey: "scrub-legacy-raw:payee_destinations:" + r.destinationID + ":" + runID,
	}
	back, err := t.Detokenize(ctx, scope, r.token)
	if err != nil {
		return false, fmt.Errorf("re-verify detokenize: %w", err)
	}
	if back != raw {
		// LOUD, not silent: this is the one case the task exists to catch.
		return false, nil
	}

	if opt.DryRun {
		return true, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE payee_destinations SET account_identifier = NULL WHERE destination_id = $1`,
		r.destinationID,
	); err != nil {
		return false, fmt.Errorf("null account_identifier: %w", err)
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
		FROM payee_destinations d
		WHERE NOT EXISTS (SELECT 1 FROM payee_destination_identifier_tokens t WHERE t.destination_id = d.destination_id)
		  AND d.account_identifier IS NOT NULL AND d.account_identifier <> ''`,
	).Scan(&n)
	return n, err
}
