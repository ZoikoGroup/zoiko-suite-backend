// Command tokenize-backfill tokenizes every payee destination whose account
// identifier was written raw, before TOKENIZATION_ENFORCED — the one-off
// Phase-2 backfill (SPEC_DEVIATIONS.md B12).
//
// For each legacy destination it calls key-management-svc's /v1/tokenize,
// immediately detokenizes the returned token and checks it equals the raw
// value, and only once EVERY row has passed does it write the tokens — into
// payee_destination_identifier_tokens (migration 000007), in one transaction.
// Any failure stops the run with nothing written. payee_destinations is never
// updated (000002's immutability trigger is untouched). A re-run is safe.
//
// Usage:
//
//	go run ./cmd/tokenize-backfill \
//	    -database-url  "postgres://postgres:...@host:5432/payee_banking_identity?sslmode=disable" \
//	    -tokenization-url http://localhost:8143 \
//	    -principal svc-tokenization-backfill \
//	    [-dry-run]
//
// -database-url must be a role that sees every tenant's rows (SUPERUSER or
// BYPASSRLS); the tool refuses to run otherwise. -principal must hold
// TOKEN_CREATE and TOKEN_DETOKENIZE at every legal entity being backfilled.
// Apply migration 000007 first.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payee-banking-identity-svc/internal/store"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("BACKFILL_DATABASE_URL"), "Postgres DSN of a role that bypasses RLS (or BACKFILL_DATABASE_URL)")
	tokenizationURL := flag.String("tokenization-url", os.Getenv("TOKENIZATION_SERVICE_URL"), "key-management-svc base URL (or TOKENIZATION_SERVICE_URL)")
	principal := flag.String("principal", "", "principal the vault records; needs TOKEN_CREATE and TOKEN_DETOKENIZE")
	dryRun := flag.Bool("dry-run", false, "tokenize and verify every row, write nothing")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall time limit")
	flag.Parse()
	if *databaseURL == "" || *tokenizationURL == "" || *principal == "" {
		fmt.Fprintln(os.Stderr, "tokenize-backfill: -database-url, -tokenization-url and -principal are required")
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
		fmt.Fprintf(os.Stderr, "tokenize-backfill: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	report, err := store.BackfillIdentifierTokens(ctx, pool, tokenization.NewClient(*tokenizationURL),
		store.BackfillOptions{PrincipalID: *principal, DryRun: *dryRun})
	fmt.Printf("run %s: %d legacy destination(s), %d tokenized (dry run: %v)\n",
		report.RunID, report.Candidates, report.Tokenized, report.DryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tokenize-backfill: FAILED: %v\n", err)
		os.Exit(1)
	}
}
