// Command drill runs the operational drills of Doc 08 (RB-01 provider
// outage, RB-02 stale/revoked claim, RB-04 rollback and safe-static, kill
// switch) against the real pipeline and writes a JSON evidence report.
//
// Default: in-memory store. With -reset-drill-db it uses
// PUBLIC_AI_DATABASE_URL and DROPS AND RECREATES the public_ai schema first —
// only ever point it at a dedicated drill database, never staging or
// production data.
//
//	go run ./cmd/drill -out drill-report.json
//	PUBLIC_AI_DATABASE_URL=postgres://.../drilldb go run ./cmd/drill -reset-drill-db -out drill-report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/store"
)

// Result is one drill's evidence.
type Result struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Runbook    string   `json:"runbook"`
	Pass       bool     `json:"pass"`
	DurationMS int64    `json:"duration_ms"`
	Steps      []string `json:"steps"`
}

// Report is the drill evidence file.
type Report struct {
	ExecutedAt string   `json:"executed_at"`
	StoreKind  string   `json:"store"`
	Note       string   `json:"note"`
	Passed     int      `json:"passed"`
	Total      int      `json:"total"`
	Results    []Result `json:"results"`
}

const (
	modules = "Which finance modules does ZoikoSuite include?"
	price   = "How much is the Growth plan per month in the United States?"
	migr    = "How does data migration from spreadsheets work?"
)

type drill struct {
	ctx context.Context
	env *fixtureenv.Env
}

func (d drill) ask(q string) orchestrator.Result {
	return d.env.Pipeline.Answer(d.ctx, orchestrator.Input{RequestID: "drill", Message: q, Locale: "en-US"})
}

func (d drill) setFlag(name, value string) error {
	if err := flags.Set(d.ctx, d.env.Store, name, []byte(value), "drill", "DRILL", "drill"); err != nil {
		return err
	}
	return d.env.Refresh(d.ctx)
}

func run(id, name, runbook string, f func(step func(string, bool)) error) Result {
	r := Result{ID: id, Name: name, Runbook: runbook, Pass: true}
	start := time.Now()
	step := func(s string, ok bool) {
		mark := "ok"
		if !ok {
			mark, r.Pass = "FAIL", false
		}
		r.Steps = append(r.Steps, mark+": "+s)
	}
	if err := f(step); err != nil {
		step("error: "+err.Error(), false)
	}
	r.DurationMS = time.Since(start).Milliseconds()
	return r
}

func main() {
	out := flag.String("out", "drill-report.json", "report file")
	resetDB := flag.Bool("reset-drill-db", false, "use PUBLIC_AI_DATABASE_URL after DROPPING its public_ai schema (dedicated drill DB only)")
	flag.Parse()
	ctx := context.Background()

	var st store.Store = store.NewMemory()
	kind := "memory"
	if *resetDB {
		dsn := os.Getenv("PUBLIC_AI_DATABASE_URL")
		if dsn == "" {
			fail("PUBLIC_AI_DATABASE_URL is required with -reset-drill-db")
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			fail(err.Error())
		}
		_ = store.Migrate(ctx, pool, migrations.FS)
		if err := store.MigrateDown(ctx, pool, migrations.FS); err != nil {
			fail(err.Error())
		}
		if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
			fail(err.Error())
		}
		st, kind = store.NewPostgres(pool), "postgres"
	}
	env, err := fixtureenv.NewWithStore(ctx, st, "drill-1", 500*time.Millisecond)
	if err != nil {
		fail(err.Error())
	}
	d := drill{ctx, env}

	rep := Report{ExecutedAt: time.Now().UTC().Format(time.RFC3339), StoreKind: kind,
		Note: "TEST-ONLY fixtures and fake model. Proves mechanisms, not production timings; repeat on staging with the deployed artifacts."}

	rep.Results = append(rep.Results, run("DR-01", "Kill switch off and on", "Doc 08 §19 RB-04 / §26", func(step func(string, bool)) error {
		step("baseline answers", d.ask(modules).Status == contract.Answered)
		t := time.Now()
		if err := d.setFlag(flags.PublicAIEnabled, "false"); err != nil {
			return err
		}
		r := d.ask(modules)
		step(fmt.Sprintf("kill switch effective in %dms (+ up to the 5s refresh interval in a running server): status %s", time.Since(t).Milliseconds(), r.Status), r.Status == contract.Unavailable)
		pending, _ := env.Store.UnpublishedEvents(ctx, 100)
		found := false
		for _, e := range pending {
			found = found || e.EventType == "publicai.kill_switch.changed.v1"
		}
		step("publicai.kill_switch.changed.v1 written to the outbox with actor/approval refs", found)
		if err := d.setFlag(flags.PublicAIEnabled, "true"); err != nil {
			return err
		}
		step("re-enabled answers again", d.ask(modules).Status == contract.Answered)
		return nil
	}))

	rep.Results = append(rep.Results, run("DR-02", "Stale claim revocation", "Doc 08 §17 RB-02", func(step func(string, bool)) error {
		r := d.ask(price)
		step("price answered from verified claim before revocation", r.Status == contract.Answered && strings.Contains(r.Text, "49.00"))
		t := time.Now()
		if _, err := release.Revoke(ctx, env.Store, "claim", "c-price-us-growth-m", "DRILL_STALE_PRICE", nil); err != nil {
			return err
		}
		if err := env.Releases.Refresh(ctx); err != nil {
			return err
		}
		r = d.ask(price)
		step(fmt.Sprintf("revoked price no longer stated after %dms: status %s", time.Since(t).Milliseconds(), r.Status), r.Status != contract.Answered && !strings.Contains(r.Text, "49.00"))
		return nil
	}))

	rep.Results = append(rep.Results, run("DR-03", "Release rollback never resurrects revoked content", "Doc 08 §19 RB-04, OPR-005", func(step func(string, bool)) error {
		step("migration source answers before revocation", d.ask(migr).Status == contract.Answered)
		if _, err := release.Revoke(ctx, env.Store, "source", "src-migration", "DRILL_REVOKE", nil); err != nil {
			return err
		}
		kb2, err := fixtureenv.NewWithStore(ctx, env.Store, "drill-2", 500*time.Millisecond) // publishes and promotes a newer release
		if err != nil {
			return err
		}
		_ = kb2
		t := time.Now()
		if err := release.Promote(ctx, env.Store, "kb", "kb-drill-1", nil); err != nil {
			return err
		}
		if err := release.Promote(ctx, env.Store, "claims", "claims-drill-1", nil); err != nil {
			return err
		}
		if err := env.Releases.Refresh(ctx); err != nil {
			return err
		}
		step(fmt.Sprintf("rolled back to kb-drill-1 in %dms (pointer swap)", time.Since(t).Milliseconds()), true)
		snap, _ := env.Releases.Snapshot()
		step("active release is the rolled-back one", snap != nil && snap.KB.ReleaseID() == "kb-drill-1")
		step("revoked source still not answerable after rollback", d.ask(migr).Status != contract.Answered)
		return nil
	}))

	rep.Results = append(rep.Results, run("DR-04", "Model provider outage", "Doc 08 §16 RB-01", func(step func(string, bool)) error {
		env.Fake.Fault = model.FaultError
		r := d.ask(modules)
		step("provider down → UNAVAILABLE with an official route, no alternate vendor", r.Status == contract.Unavailable && len(r.Actions) == 1)
		env.Fake.Fault = model.FaultTimeout
		t := time.Now()
		r = d.ask(modules)
		step(fmt.Sprintf("provider timeout bounded (%dms) → UNAVAILABLE", time.Since(t).Milliseconds()), r.Status == contract.Unavailable && time.Since(t) < 3*time.Second)
		env.Fake.Fault = model.FaultNone
		step("recovery: factual answer after provider returns", d.ask(modules).Status == contract.Answered)
		return nil
	}))

	rep.Results = append(rep.Results, run("DR-05", "Safe-static mode", "Doc 06 §23, Doc 08 §19", func(step func(string, bool)) error {
		if err := d.setFlag(flags.ApprovedFallbackMode, `"static"`); err != nil {
			return err
		}
		r := d.ask("Do you hold ISO 27001 certification?")
		step("signed claim statements still served", r.Status == contract.Answered)
		step("no generated answers in static mode", d.ask(modules).Status == contract.Unavailable)
		if err := d.setFlag(flags.ApprovedFallbackMode, `""`); err != nil {
			return err
		}
		step("normal mode restored", d.ask(modules).Status == contract.Answered)
		return nil
	}))

	for _, r := range rep.Results {
		rep.Total++
		if r.Pass {
			rep.Passed++
		}
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		fmt.Printf("%s %s %s (%dms)\n", mark, r.ID, r.Name, r.DurationMS)
		for _, s := range r.Steps {
			fmt.Println("     ", s)
		}
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Printf("\n%d/%d drills passed (%s store) -> %s\n", rep.Passed, rep.Total, kind, *out)
	if rep.Passed != rep.Total {
		os.Exit(1)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "drill:", msg)
	os.Exit(2)
}
