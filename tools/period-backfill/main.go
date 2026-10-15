// period-backfill brings REF-04 (fiscal-calendar-svc) and REF-05
// (accounting-period-svc) in line with the periods that already exist in
// financial-close-svc. It talks to all three ONLY over their HTTP APIs, is a
// dry run unless --apply is given, and never forces: anything it cannot map
// cleanly is reported, not repaired. See README.md.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain returns the exit code: 0 ok, 1 the run hit errors (or --strict
// found unmapped/unmatched/blocking anomalies), 2 usage.
func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("period-backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg config
	var entities listFlag
	var entitiesFile, reportPath string
	var strict bool
	var timeout time.Duration

	fs.StringVar(&cfg.Tenant, "tenant", "", "tenant id (required)")
	fs.Var(&entities, "entity", "legal entity id; repeatable or comma-separated")
	fs.StringVar(&entitiesFile, "entities-file", "", "file with one legal entity id per line (# comments allowed)")
	fs.IntVar(&cfg.StartMonth, "fy-start-month", 1, "fiscal-year start month (1-12)")
	fs.IntVar(&cfg.StartDay, "fy-start-day", 1, "fiscal-year start day (1-28)")
	fs.IntVar(&cfg.FromFY, "from-fy", 0, "first fiscal year to cover (default: derived from the entity's legacy periods)")
	fs.IntVar(&cfg.ToFY, "to-fy", 0, "last fiscal year to cover (default: derived from the entity's legacy periods)")
	fs.StringVar(&cfg.Maker, "maker", "", "maker principal id: proposes the calendar, materializes (required)")
	fs.StringVar(&cfg.Checker, "checker", "", "checker principal id: approves and activates the calendar; must differ from --maker (required)")
	fs.StringVar(&cfg.MirrorPrincipal, "mirror-principal", "", "principal for --mirror-closed calls (default: --maker)")
	fs.StringVar(&cfg.CalendarURL, "calendar-url", "http://localhost:8173", "fiscal-calendar-svc base URL")
	fs.StringVar(&cfg.PeriodURL, "period-url", "http://localhost:8174", "accounting-period-svc base URL")
	fs.StringVar(&cfg.CloseURL, "close-url", "http://localhost:8104", "financial-close-svc base URL")
	fs.StringVar(&cfg.CalendarCode, "calendar-code", "CALENDAR-MONTHS", "code of the CALENDAR_MONTHS calendar to ensure per entity")
	fs.StringVar(&cfg.Scope, "scope", "PRIMARY", "fiscal-calendar scope (REF-04 requires a non-empty one)")
	fs.StringVar(&cfg.BookScope, "book-scope", "", "accounting-period book_scope for the materialised periods; empty = entity-wide (covers every request)")
	fs.StringVar(&cfg.Reason, "reason", "REF-05 cutover: backfill calendar and periods from financial-close-svc", "reason recorded on every command")
	fs.BoolVar(&cfg.Apply, "apply", false, "execute the plan; without it the tool only reads and prints")
	fs.BoolVar(&cfg.MirrorClosed, "mirror-closed", false, "last stage: replay non-OPEN legacy periods into REF-05 via financial-close-svc (needs PERIOD_SERVICE_MIRROR=on)")
	fs.BoolVar(&strict, "strict", false, "exit non-zero when unmapped/unmatched periods or blocking anomalies exist")
	fs.StringVar(&reportPath, "report", "", "write the machine-readable JSON report to this file")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	all := []string(entities)
	if entitiesFile != "" {
		more, err := readEntitiesFile(entitiesFile)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
		all = append(all, more...)
	}
	cfg.Entities = dedupe(all)
	if cfg.MirrorPrincipal == "" {
		cfg.MirrorPrincipal = cfg.Maker
	}
	cfg.CalendarURL, cfg.PeriodURL, cfg.CloseURL = strings.TrimRight(cfg.CalendarURL, "/"), strings.TrimRight(cfg.PeriodURL, "/"), strings.TrimRight(cfg.CloseURL, "/")
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	runID := newRunID()
	rn := &runner{cfg: cfg, c: newClient(timeout, cfg.Tenant, runID)}
	rep := rn.run(context.Background())

	fmt.Fprint(stdout, rep.Human())
	if reportPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(reportPath, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(stderr, "error: writing report:", err)
			return 1
		}
		fmt.Fprintln(stdout, "report written to", reportPath)
	}
	if rep.Failed(strict) {
		return 1
	}
	return 0
}

// validate is where SoD is enforced up front: before any request is made.
func (c *config) validate() error {
	var missing []string
	if c.Tenant == "" {
		missing = append(missing, "--tenant")
	}
	if len(c.Entities) == 0 {
		missing = append(missing, "--entity or --entities-file")
	}
	if c.Maker == "" {
		missing = append(missing, "--maker")
	}
	if c.Checker == "" {
		missing = append(missing, "--checker")
	}
	if len(missing) > 0 {
		return fmt.Errorf("required: %s", strings.Join(missing, ", "))
	}
	if strings.EqualFold(strings.TrimSpace(c.Maker), strings.TrimSpace(c.Checker)) {
		return errors.New("segregation of duties: --maker and --checker must be two different principals (the proposer of a calendar version may not approve it)")
	}
	if c.StartMonth < 1 || c.StartMonth > 12 {
		return errors.New("--fy-start-month must be 1-12")
	}
	if c.StartDay < 1 || c.StartDay > 28 {
		return errors.New("--fy-start-day must be 1-28 (fiscal-calendar-svc caps it at 28)")
	}
	if c.FromFY != 0 && c.ToFY != 0 && c.ToFY < c.FromFY {
		return errors.New("--to-fy must not be before --from-fy")
	}
	if c.Scope == "" || c.CalendarCode == "" || c.Reason == "" {
		return errors.New("--scope, --calendar-code and --reason must not be empty")
	}
	return nil
}

func readEntitiesFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		k := strings.ToLower(s)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "period-backfill-" + hex.EncodeToString(b[:])
}
