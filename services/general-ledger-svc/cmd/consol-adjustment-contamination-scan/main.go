// Command consol-adjustment-contamination-scan is a one-time, READ-ONLY diagnostic for SPEC_DEVIATIONS.md A1/B12
// (consolidation-svc / general-ledger-svc). It never writes to the database.
//
// Between 2026-09-07 and 2026-09-30, consolidation-svc posted consolidation adjustments through
// general-ledger-svc's generic system-posting endpoint with no book identifier, so a posted adjustment could land
// as an ordinary journal in a group entity's own statutory book. That path was replaced (see SPEC_DEVIATIONS.md,
// entry B12) by one that posts only into the group's own accounting book. This tool finds journals in a real
// environment's database that structurally match what the old, now-replaced path would have produced: see
// internal/store/consol_adjustment_contamination_scan.go for the exact signals it checks and why each one holds.
//
// It reports CANDIDATES for manual review, not a final verdict — an operator should read each one's lines and
// description before treating it as confirmed contamination.
//
// Usage:
//
//	go run ./cmd/consol-adjustment-contamination-scan \
//	    -database-url "postgres://postgres:...@host:5432/zoiko_general_ledger?sslmode=disable"
//
// -database-url must be a role that bypasses row-level security (SUPERUSER or BYPASSRLS — the migration role),
// not the runtime zoiko_app role; the tool refuses to run otherwise rather than quietly scan nothing. The window
// defaults to the exact dates SPEC_DEVIATIONS.md names (2026-09-07 through 2026-09-30 inclusive); -window-start
// and -window-end (RFC3339 or YYYY-MM-DD, UTC) override it only if there is a documented reason to widen or
// narrow it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/general-ledger-svc/internal/store"
)

func parseBound(s string, fallback time.Time) (time.Time, error) {
	if s == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("not a valid RFC3339 or YYYY-MM-DD timestamp: %q", s)
}

func main() {
	databaseURL := flag.String("database-url", os.Getenv("SCAN_DATABASE_URL"), "Postgres DSN of a role that bypasses RLS (or SCAN_DATABASE_URL)")
	windowStartFlag := flag.String("window-start", "", "override the scan window's start (default: 2026-09-07, the exposure window SPEC_DEVIATIONS.md names)")
	windowEndFlag := flag.String("window-end", "", "override the scan window's end, exclusive (default: 2026-10-01, i.e. through end of 2026-09-30)")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall time limit")
	flag.Parse()
	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "consol-adjustment-contamination-scan: -database-url is required")
		flag.Usage()
		os.Exit(2)
	}

	defaultStart, defaultEnd := store.DefaultContaminationWindow()
	windowStart, err := parseBound(*windowStartFlag, defaultStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consol-adjustment-contamination-scan: -window-start: %v\n", err)
		os.Exit(2)
	}
	windowEnd, err := parseBound(*windowEndFlag, defaultEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consol-adjustment-contamination-scan: -window-end: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "consol-adjustment-contamination-scan: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	report, err := store.ScanHistoricalGroupBookContamination(ctx, pool, windowStart, windowEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consol-adjustment-contamination-scan: FAILED: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("consol-adjustment-contamination-scan: window %s .. %s (exclusive), %d group reporting entit(y/ies) checked, %d candidate(s) found\n",
		report.WindowStart.Format("2006-01-02"), report.WindowEnd.Format("2006-01-02"), report.GroupEntityCount, len(report.Candidates))
	if len(report.Candidates) == 0 {
		fmt.Println("consol-adjustment-contamination-scan: no candidates. This is read-only evidence, not a guarantee — it only rules out the exact shape the old posting path produced.")
		return
	}
	fmt.Println("consol-adjustment-contamination-scan: CANDIDATES — each needs manual review (read its lines and description) before being treated as confirmed contamination:")
	for _, c := range report.Candidates {
		fmt.Printf("  tenant=%s journal_id=%s journal_number=%q legal_entity_id=%s group_id=%s fiscal_period=%s status=%s created_at=%s source_event_id=%s description=%q\n",
			c.TenantID, c.JournalID, c.JournalNumber, c.LegalEntityID, c.GroupID, c.FiscalPeriod, c.Status, c.CreatedAt.Format(time.RFC3339), c.SourceEventID, c.Description)
	}
	os.Exit(1)
}
