// Command skipcheck fails when a test was skipped without a recorded reason
// (Doc 07 §10: no test is disabled without a waiver). It reads `go test
// -json` output on stdin and compares every skipped top-level test with
// testdata/skip-allowlist.txt (one test name per line, then the reason).
//
//	go test -json ./... | go run ./cmd/skipcheck -allowlist testdata/skip-allowlist.txt
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

func main() {
	allowPath := flag.String("allowlist", "testdata/skip-allowlist.txt", "allowed skips, one test name per line with its reason")
	flag.Parse()
	allowed, err := readAllowlist(*allowPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skipcheck:", err)
		os.Exit(2)
	}
	bad, failed, err := check(os.Stdin, allowed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skipcheck:", err)
		os.Exit(2)
	}
	for _, b := range bad {
		fmt.Println("skipped without an allowlist entry:", b)
	}
	if len(bad) > 0 || failed {
		os.Exit(1)
	}
	fmt.Println("skipcheck: every skipped test is allowlisted")
}

// check returns skipped top-level tests that are not allowed, and whether
// any test failed (so the job also fails on failures it was piped).
func check(r io.Reader, allowed map[string]bool) ([]string, bool, error) {
	seen := map[string]bool{}
	failed := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Test == "" {
			continue
		}
		top := strings.SplitN(e.Test, "/", 2)[0]
		switch e.Action {
		case "skip":
			if !allowed[top] {
				seen[e.Package+" "+top] = true
			}
		case "fail":
			failed = true
		}
	}
	bad := make([]string, 0, len(seen))
	for k := range seen {
		bad = append(bad, k)
	}
	sort.Strings(bad)
	return bad, failed, sc.Err()
}

func readAllowlist(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s: %q has no reason", path, fields[0])
		}
		out[fields[0]] = true
	}
	return out, nil
}
