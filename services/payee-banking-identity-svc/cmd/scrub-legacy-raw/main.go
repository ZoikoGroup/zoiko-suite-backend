// Command scrub-legacy-raw nulls payee_destinations.account_identifier for
// every destination that already has a verified token in
// payee_destination_identifier_tokens — the one-off Phase-2 cleanup that
// follows cmd/tokenize-backfill (SPEC_DEVIATIONS.md B12). Mirrors
// banking-connector-svc's cmd/scrub-legacy-raw exactly; see its doc for the
// full reasoning.
//
// For each candidate it re-verifies the round trip ITSELF, independently of
// whatever tokenized it originally: fetch the row's current raw value,
// detokenize its stored token, byte-compare the two, and only on an exact
// match null the column — in the same transaction as that row's lock. A
// mismatch for any row stops nothing else: that one row is left untouched
// and reported loudly; every other row is still processed.
//
// Never touches a row with no token at all (never tokenized — the
// pre-existing, documented gap this does not close) or a SUSPENDED/
// SUPERSEDED destination (payee_destinations' own immutability trigger
// refuses any update to one, full stop).
//
// Usage:
//
//	go run ./cmd/scrub-legacy-raw \
//	    -database-url  "postgres://postgres:...@host:5432/payee_banking_identity?sslmode=disable" \
//	    -tokenization-url http://localhost:8143 \
//	    -principal svc-tokenization-backfill \
//	    [-dry-run]
//
// -database-url must be a role that sees every tenant's rows (SUPERUSER or
// BYPASSRLS), not the runtime zoiko_app role; the tool refuses to run
// otherwise. -principal must hold TOKEN_DETOKENIZE at every legal entity
// being scrubbed — the same grant and role cmd/tokenize-backfill uses. Apply
// migrations 000008 (drops account_identifier's NOT NULL constraint) and
// 000009 (carves out the one immutability-trigger exception this tool's
// UPDATE needs) first.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payee-banking-identity-svc/internal/store"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("BACKFILL_DATABASE_URL"), "Postgres DSN of a role that bypasses RLS (or BACKFILL_DATABASE_URL)")
	tokenizationURL := flag.String("tokenization-url", os.Getenv("TOKENIZATION_SERVICE_URL"), "key-management-svc base URL (or TOKENIZATION_SERVICE_URL)")
	principal := flag.String("principal", "", "principal the vault records for this re-verification; needs TOKEN_DETOKENIZE")
	dryRun := flag.Bool("dry-run", false, "re-verify every candidate's round trip, null nothing")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall time limit")
	flag.Parse()
	if *databaseURL == "" || *tokenizationURL == "" || *principal == "" {
		fmt.Fprintln(os.Stderr, "scrub-legacy-raw: -database-url, -tokenization-url and -principal are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrub-legacy-raw: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	report, err := store.ScrubIdentifierTokens(ctx, pool, tokenization.NewClient(*tokenizationURL),
		store.ScrubOptions{PrincipalID: *principal, DryRun: *dryRun})
	fmt.Printf("run %s: %d candidate(s), %d scrubbed, %d without a token (left alone), %d terminal (left alone) (dry run: %v)\n",
		report.RunID, report.Candidates, report.Scrubbed, report.SkippedNoToken, report.SkippedTerminal, report.DryRun)
	if len(report.Mismatches) > 0 {
		fmt.Fprintf(os.Stderr, "scrub-legacy-raw: %d destination(s) FAILED round-trip re-verification and were left untouched: %s\n",
			len(report.Mismatches), strings.Join(report.Mismatches, ", "))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrub-legacy-raw: FAILED: %v\n", err)
		os.Exit(1)
	}
	if len(report.Mismatches) > 0 {
		os.Exit(1)
	}
}
