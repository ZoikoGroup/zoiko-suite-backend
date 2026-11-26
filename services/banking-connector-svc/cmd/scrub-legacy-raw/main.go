// Command scrub-legacy-raw nulls bank_connections.account_number for every
// connection that already has a verified token in
// bank_connection_account_tokens — the one-off Phase-2 cleanup that follows
// cmd/tokenize-backfill (SPEC_DEVIATIONS.md G6).
//
// For each candidate it re-verifies the round trip ITSELF, independently of
// whatever tokenized it originally: fetch the row's current raw value,
// detokenize its stored token, byte-compare the two, and only on an exact
// match null the column — in the same transaction as that row's lock, so a
// concurrent write cannot race the check. A mismatch for any row stops
// nothing else: that one row is left untouched and reported loudly; every
// other row is still processed.
//
// Never touches a row with no token at all (never tokenized — the
// pre-existing, documented gap this does not close) or a REVOKED connection
// (bank_connections' own immutability trigger refuses any update to one,
// full stop, the same as it would have refused a column backfill in place).
//
// Usage:
//
//	go run ./cmd/scrub-legacy-raw \
//	    -database-url  "postgres://postgres:...@host:5432/zoiko_banking_connector?sslmode=disable" \
//	    -tokenization-url http://localhost:8143 \
//	    -principal svc-tokenization-backfill \
//	    [-dry-run]
//
// -database-url must be a role that sees every tenant's rows (SUPERUSER or
// BYPASSRLS — the migration role), not the runtime zoiko_app role; the tool
// refuses to run otherwise rather than quietly scrub nothing. -principal
// must hold TOKEN_DETOKENIZE at every legal entity being scrubbed — the same
// grant and role cmd/tokenize-backfill uses
// (deployments/scripts/seed-demo-rbac.ps1's TOKENIZATION_BACKFILL). Apply
// migration 011 first (it drops account_number's NOT NULL constraint; this
// tool's own UPDATE is refused outright otherwise).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
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

	report, err := store.ScrubLegacyRawAccountNumbers(ctx, pool, tokenization.NewClient(*tokenizationURL),
		store.ScrubOptions{PrincipalID: *principal, DryRun: *dryRun})
	fmt.Printf("run %s: %d candidate(s), %d scrubbed, %d without a token (left alone), %d REVOKED (left alone) (dry run: %v)\n",
		report.RunID, report.Candidates, report.Scrubbed, report.SkippedNoToken, report.SkippedRevoked, report.DryRun)
	if len(report.Mismatches) > 0 {
		fmt.Fprintf(os.Stderr, "scrub-legacy-raw: %d connection(s) FAILED round-trip re-verification and were left untouched: %s\n",
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
