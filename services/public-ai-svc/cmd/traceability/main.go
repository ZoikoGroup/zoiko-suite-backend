// Command traceability joins `go test -json` output with the machine-readable
// traceability matrix (Doc 06 §26, Doc 07 §34) and fails when a P0 test ID
// mapped to Go tests has one that failed or did not run. A skipped test is
// reported and fails only with -fail-on-skip.
//
// It also keeps the matrix honest. Every row whose status is not Built is
// listed as OPEN, so a Blocked or Partial P0 row is never reported as passing
// (-fail-on-open makes it fatal). A Built row with no Go test and no checkable
// frontend reference fails. With -frontend, each frontend reference
// (frontend:<path> "<test title>") must name an existing file that contains
// that test title.
//
//	go test -json ./... > test.json
//	go run ./cmd/traceability -csv ../../docs/suivo/sec-t-traceability.csv -json test.json [-frontend <frontend repo>]
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	csvPath := flag.String("csv", "../../docs/suivo/sec-t-traceability.csv", "traceability matrix (id,priority,title,status,go_tests,...)")
	jsonPath := flag.String("json", "-", "go test -json output (- for stdin)")
	priority := flag.String("priority", "P0", "priority whose mapped tests must all pass")
	failOnSkip := flag.Bool("fail-on-skip", false, "treat a skipped mapped test as a failure")
	failOnOpen := flag.Bool("fail-on-open", false, "treat a row whose status is not Built as a failure")
	frontend := flag.String("frontend", "", "frontend repository root; when set, frontend references are checked")
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
	rep, err := check(matrix, results, Options{Priority: *priority, FailOnSkip: *failOnSkip, FailOnOpen: *failOnOpen, Frontend: *frontend})
	if err != nil {
		fmt.Fprintln(os.Stderr, "traceability:", err)
		os.Exit(2)
	}
	for _, o := range rep.Open {
		fmt.Println(o)
	}
	for _, p := range rep.Problems {
		fmt.Println(p)
	}
	fmt.Printf("traceability: %d %s IDs (%d Built, %d not built), %d mapped Go tests, %d frontend references checked, %d unchecked, %d problems, %d skipped\n",
		rep.IDs, *priority, rep.IDs-len(rep.Open), len(rep.Open), rep.Tests, rep.FrontendChecked, rep.FrontendUnchecked, len(rep.Problems), rep.Skipped)
	if len(rep.Problems) > 0 {
		os.Exit(1)
	}
}

// Options select what a check enforces.
type Options struct {
	Priority   string
	FailOnSkip bool
	FailOnOpen bool
	Frontend   string // frontend repository root; "" leaves frontend references unchecked
}

// Report is the outcome of a check.
type Report struct {
	IDs, Tests, Skipped                int
	FrontendChecked, FrontendUnchecked int
	Open                               []string // "OPEN <id> <status>" for rows not Built
	Problems                           []string // one line per failing, missing or (strict) skipped test
}

// frontendRef matches `path/to/file.ts "test title"` inside frontend_evidence.
var frontendRef = regexp.MustCompile(`([A-Za-z0-9_./-]+\.(?:tsx?|jsx?))\s+"([^"]+)"`)

// key names a top-level test; its outcome is "pass", "fail" or "skip".
type key struct{ pkg, test string }

// check reads the matrix and the test events. A mapped test is
// "pkgdir.TestName"; pkgdir is the directory under the module root, so it
// matches any package import path ending in "/pkgdir".
func check(matrix, events io.Reader, o Options) (Report, error) {
	priority, failOnSkip := o.Priority, o.FailOnSkip
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
	field := func(r []string, name string) string {
		if i, ok := col[name]; ok && i < len(r) {
			return strings.TrimSpace(r[i])
		}
		return ""
	}
	var rep Report
	for _, r := range rows[1:] {
		if r[col["priority"]] != priority {
			continue
		}
		id, status, goTests := r[col["id"]], field(r, "status"), field(r, "go_tests")
		rep.IDs++
		if _, ok := col["status"]; ok && status != "Built" {
			rep.Open = append(rep.Open, fmt.Sprintf("OPEN    %s %s", id, status))
			if o.FailOnOpen {
				rep.Problems = append(rep.Problems, fmt.Sprintf("NOT BUILT %s %s", id, status))
			}
		}
		refs := frontendRef.FindAllStringSubmatch(field(r, "frontend_evidence"), -1)
		for _, ref := range refs {
			if o.Frontend == "" {
				rep.FrontendUnchecked++
				continue
			}
			rep.FrontendChecked++
			b, err := os.ReadFile(filepath.Join(o.Frontend, filepath.FromSlash(ref[1])))
			switch {
			case err != nil:
				rep.Problems = append(rep.Problems, fmt.Sprintf("FRONTEND %s %s (file not found)", id, ref[1]))
			case !strings.Contains(string(b), ref[2]):
				rep.Problems = append(rep.Problems, fmt.Sprintf("FRONTEND %s %s has no test %q", id, ref[1], ref[2]))
			}
		}
		if status == "Built" && goTests == "" && len(refs) == 0 {
			rep.Problems = append(rep.Problems, fmt.Sprintf("NO EVIDENCE %s is Built but names no Go test or frontend test", id))
		}
		if goTests == "" {
			continue
		}
		for _, t := range strings.Split(goTests, ";") {
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
