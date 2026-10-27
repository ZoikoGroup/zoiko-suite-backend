// Command evalrun runs a golden dataset against the in-process stack on the
// TEST-ONLY fixtures and writes a JSON report. It exits 1 when any P0 case
// fails (BLOCK_RELEASE, Doc 07 §19).
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

	"zoiko.io/public-ai-svc/internal/eval"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
)

func main() {
	dataset := flag.String("dataset", "testdata/golden/golden-v1.json", "golden dataset file")
	out := flag.String("out", "eval-report.json", "report output file")
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
	rep := eval.Run(ctx, env.Pipeline, d, "fake-v0")
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
	fmt.Printf("\n%s: %d/%d passed, %d P0 failures -> %s (report: %s)\n", rep.DatasetVersion, rep.Passed, rep.Total, rep.P0Failed, rep.Verdict, *out)
	if rep.Verdict != "PASS" {
		os.Exit(1)
	}
}
