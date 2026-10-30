// gak-rollout prepares one tenant for the Global Accounting Kernel: the accounting book, the account mappings and the rule packs, and then
// checks that everything the packs name can resolve. It talks to general-ledger-svc over HTTP only, is a dry run unless told to --apply,
// and never overwrites, reactivates or forces anything. It does not switch any service over: it prints what to set.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	exitOK        = 0
	exitAttention = 1
	exitUsage     = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("gak-rollout", flag.ContinueOnError)
	fs.SetOutput(errw)
	base := fs.String("base", "http://127.0.0.1:8098", "general-ledger-svc base URL")
	tenant := fs.String("tenant", "", "the tenant to prepare (required)")
	principal := fs.String("principal", "", "the principal the calls are made as; it needs the book, rule-management and mapping rights (required)")
	cfgPath := fs.String("config", "", "the rollout configuration, a JSON file (required)")
	apply := fs.Bool("apply", false, "make the changes; without it the tool only reads and prints the plan")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *tenant == "" || *principal == "" || *cfgPath == "" {
		fmt.Fprintln(errw, "--tenant, --principal and --config are required")
		fs.Usage()
		return exitUsage
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fmt.Fprintln(errw, err)
		return exitUsage
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fmt.Fprintln(errw, "the configuration is not valid JSON for this tool:", err)
		return exitUsage
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(errw, err)
		return exitUsage
	}

	c := newClient(*base, *tenant, *principal, cfg.LegalEntityID, *timeout)
	rep, err := Run(context.Background(), c, &cfg, *apply, time.Now())
	if err != nil {
		fmt.Fprintln(errw, err)
		return exitUsage
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		render(out, rep, &cfg)
	}
	if rep.NeedsAttention() {
		return exitAttention
	}
	return exitOK
}

func render(w io.Writer, rep *Report, cfg *Config) {
	mode := "DRY RUN - nothing was changed. Run again with --apply to make the changes."
	if rep.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintln(w, mode)
	fmt.Fprintln(w)
	area := ""
	for _, s := range rep.Steps {
		if s.Area != area {
			area = s.Area
			fmt.Fprintf(w, "%s\n", strings.ToUpper(area))
		}
		fmt.Fprintf(w, "  [%-8s] %s", s.Status, s.Name)
		if s.Detail != "" {
			fmt.Fprintf(w, " - %s", s.Detail)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	if rep.NeedsAttention() {
		fmt.Fprintln(w, "Something needs a person (CONFLICT, BLOCKED or FAILED above). Fix it, then run again: the tool is safe to repeat.")
		return
	}
	if rep.BookID == "" {
		return
	}
	fmt.Fprintln(w, "TO SWITCH A SERVICE OVER (after its rule sets are active; the tool sets nothing):")
	fmt.Fprintf(w, "  LEDGER_KERNEL_BOOK_ID=%s       payroll-run-svc, asset-management-svc, inventory-management-svc\n", rep.BookID)
	fmt.Fprintf(w, "  LEDGER_KERNEL_CURRENCY=%s         asset-management-svc, inventory-management-svc\n", rep.Currency)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "STILL FOR PEOPLE:")
	fmt.Fprintln(w, "  1. Test each instantiated rule set (POST /v1/posting-rule-sets/{id}/test).")
	fmt.Fprintln(w, "  2. Approve it - by someone other than its author (…/approve).")
	fmt.Fprintln(w, "  3. Activate it - by a third person (…/activate).")
	fmt.Fprintln(w, "  4. Run this tool again: every readiness line should be OK. Then switch one domain over at a time.")
}
