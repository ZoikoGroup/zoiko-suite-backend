// Command tokenize-backfill tokenizes every bank connection whose account
// number was written raw, before TOKENIZATION_ENFORCED — the one-off Phase-2
// backfill (SPEC_DEVIATIONS.md G6).
//
// For each legacy connection it calls key-management-svc's /v1/tokenize,
// immediately detokenizes the returned token and checks it equals the raw
// value, and only once EVERY row has passed does it write the tokens — into
// bank_connection_account_tokens (migration 010), in one transaction. Any
// failure stops the run with nothing written. bank_connections is never
// updated. A re-run is safe: finished rows are not candidates, and each row's
// Idempotency-Key is derived from its id, so the vault returns the tokens a
// failed run already minted.
//
// Usage:
//
//	go run ./cmd/tokenize-backfill \
//	    -database-url  "postgres://postgres:...@host:5432/zoiko_banking_connector?sslmode=disable" \
//	    -tokenization-url http://localhost:8143 \
//	    -principal svc-tokenization-backfill \
//	    [-dry-run]
//
// -database-url must be a role that sees every tenant's rows (SUPERUSER or
// BYPASSRLS — the migration role), not the runtime zoiko_app role; the tool
// refuses to run otherwise rather than quietly backfill nothing. -principal
// must hold TOKEN_CREATE and TOKEN_DETOKENIZE at every legal entity being
// backfilled (deployments/scripts/seed-demo-rbac.ps1 grants the local one).
// Apply migration 010 first.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
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

	report, err := store.BackfillAccountTokens(ctx, pool, tokenization.NewClient(*tokenizationURL),
		store.BackfillOptions{PrincipalID: *principal, DryRun: *dryRun})
	fmt.Printf("run %s: %d legacy connection(s), %d without an account number, %d tokenized (dry run: %v)\n",
		report.RunID, report.Candidates, report.SkippedEmpty, report.Tokenized, report.DryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tokenize-backfill: FAILED: %v\n", err)
		os.Exit(1)
	}
}
