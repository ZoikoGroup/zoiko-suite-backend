// Command traceability joins `go test -json` output with the machine-readable
// traceability matrix (Doc 06 §26, Doc 07 §34) and fails when a P0 test ID
// mapped to Go tests has one that failed or did not run. A skipped test is
// reported and fails only with -fail-on-skip.
//
//	go test -json ./... > test.json
//	go run ./cmd/traceability -csv ../../docs/suivo/sec-t-traceability.csv -json test.json
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	csvPath := flag.String("csv", "../../docs/suivo/sec-t-traceability.csv", "traceability matrix (id,priority,title,status,go_tests,...)")
	jsonPath := flag.String("json", "-", "go test -json output (- for stdin)")
	priority := flag.String("priority", "P0", "priority whose mapped tests must all pass")
	failOnSkip := flag.Bool("fail-on-skip", false, "treat a skipped mapped test as a failure")
	flag.Parse()

	matrix, err := os.Open(*csvPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traceability:", err)
		os.Exit(2)
	}
	defer matrix.Close()
	var results io.Reader = os.Stdin
	if *jsonPath != "-" {
		f, err := os.Open(*jsonPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "traceability:", err)
			os.Exit(2)
		}
		defer f.Close()
		results = f
	}
	rep, err := check(matrix, results, *priority, *failOnSkip)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traceability:", err)
		os.Exit(2)
	}
	for _, p := range rep.Problems {
		fmt.Println(p)
	}
	fmt.Printf("traceability: %d %s IDs checked, %d mapped tests, %d problems, %d skipped\n",
		rep.IDs, *priority, rep.Tests, len(rep.Problems), rep.Skipped)
	if len(rep.Problems) > 0 {
		os.Exit(1)
	}
}

// Report is the outcome of a check.
type Report struct {
	IDs, Tests, Skipped int
	Problems            []string // one line per failing, missing or (strict) skipped test
}

// key names a top-level test; its outcome is "pass", "fail" or "skip".
type key struct{ pkg, test string }

// check reads the matrix and the test events. A mapped test is
// "pkgdir.TestName"; pkgdir is the directory under the module root, so it
// matches any package import path ending in "/pkgdir".
func check(matrix, events io.Reader, priority string, failOnSkip bool) (Report, error) {
	outcomes, err := readEvents(events)
	if err != nil {
		return Report{}, err
	}
	rows, err := csv.NewReader(matrix).ReadAll()
	if err != nil {
		return Report{}, fmt.Errorf("matrix: %w", err)
	}
	if len(rows) == 0 {
		return Report{}, fmt.Errorf("matrix is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, h := range []string{"id", "priority", "go_tests"} {
		if _, ok := col[h]; !ok {
			return Report{}, fmt.Errorf("matrix has no %q column", h)
		}
	}
	var rep Report
	for _, r := range rows[1:] {
		if r[col["priority"]] != priority || strings.TrimSpace(r[col["go_tests"]]) == "" {
			continue
		}
		rep.IDs++
		for _, t := range strings.Split(r[col["go_tests"]], ";") {
			t = strings.TrimSpace(t)
			dot := strings.LastIndex(t, ".")
			if dot <= 0 {
				return Report{}, fmt.Errorf("%s: malformed test reference %q (want pkgdir.TestName)", r[col["id"]], t)
			}
			rep.Tests++
			switch got := find(outcomes, t[:dot], t[dot+1:]); got {
			case "pass":
			case "skip":
				rep.Skipped++
				if failOnSkip {
					rep.Problems = append(rep.Problems, fmt.Sprintf("SKIPPED %s %s", r[col["id"]], t))
				}
			case "":
				rep.Problems = append(rep.Problems, fmt.Sprintf("MISSING %s %s (did not run)", r[col["id"]], t))
			default:
				rep.Problems = append(rep.Problems, fmt.Sprintf("FAILED  %s %s", r[col["id"]], t))
			}
		}
	}
	sort.Strings(rep.Problems)
	return rep, nil
}

func find(outcomes map[key]string, pkgDir, test string) string {
	for k, v := range outcomes {
		if k.test == test && (k.pkg == pkgDir || strings.HasSuffix(k.pkg, "/"+pkgDir)) {
			return v
		}
	}
	return ""
}

// readEvents keeps the final action of every top-level test.
func readEvents(r io.Reader) (map[key]string, error) {
	out := map[key]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue // build output interleaved with events
		}
		var ev struct {
			Action, Package, Test string
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("test events: %w", err)
		}
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			out[key{ev.Package, ev.Test}] = ev.Action
		}
	}
	return out, sc.Err()
}
