// Command evalrun runs a golden dataset against the in-process stack on the
// TEST-ONLY fixtures and writes a JSON report. It exits 1 when any P0 case
// fails (BLOCK_RELEASE, Doc 07 §19) and 1 on an unwaived P1 failure or a
// baseline regression (NO_GO, Doc 07 §21).
//
//	go run ./cmd/evalrun -dataset testdata/golden/golden-v1.json -out eval-report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/eval"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
)

func main() {
	dataset := flag.String("dataset", "testdata/golden/golden-v1.json", "golden dataset file")
	out := flag.String("out", "eval-report.json", "report output file")
	repeats := flag.Int("repeats", 1, "runs per case (P0 cases run at least 3 times)")
	waivers := flag.String("waivers", "", "JSON file of approved P1 waivers: case id to waiver reference")
	baseline := flag.String("baseline", "", "approved baseline report; a case that passed there and fails now is NO_GO")
	sha := flag.String("sha", "", "source revision under test (CI passes -sha $GITHUB_SHA)")
	flag.Parse()

	ctx := context.Background()
	env, err := fixtureenv.New(ctx, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evalrun:", err)
		os.Exit(2)
	}
	d, err := eval.Load(*dataset)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evalrun:", err)
		os.Exit(2)
	}
	// The in-process stack runs the deterministic mock provider; the profile
	// names it the way the server and the release manifest do, so kbctl
	// manifest refuses this report for any other model (Doc 08 §06).
	profile := config.ModelProfile("mock", config.MockModelID)
	opts := eval.Options{Repeats: *repeats, ModelProfile: profile, ModelID: config.MockModelID, GitSHA: *sha}
	if *waivers != "" {
		if err := readJSON(*waivers, &opts.Waivers); err != nil {
			fmt.Fprintln(os.Stderr, "evalrun: waivers:", err)
			os.Exit(2)
		}
	}
	rep := eval.RunWith(ctx, env.Pipeline, d, opts)
	if *baseline != "" {
		var base eval.Report
		if err := readJSON(*baseline, &base); err != nil {
			fmt.Fprintln(os.Stderr, "evalrun: baseline:", err)
			os.Exit(2)
		}
		rep.CompareBaseline(base)
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "evalrun:", err)
		os.Exit(2)
	}
	for _, r := range rep.Results {
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		fmt.Printf("%-4s %-7s %s/%s %-14s %v\n", mark, r.ID, r.Partition, r.Priority, r.Status, r.Failures)
	}
	fmt.Printf("\n%s: %d/%d passed, %d P0 failures, %d flaky, %d high-risk false permits, %d regressions -> %s (report: %s)\n",
		rep.DatasetVersion, rep.Passed, rep.Total, rep.P0Failed, rep.Flaky, rep.Intents.HighRiskFalsePermits, len(rep.Regressions), rep.Verdict, *out)
	if rep.Verdict != "PASS" {
		os.Exit(1)
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
