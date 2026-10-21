// Command intcert runs the tests the INT certification ledger cites and writes a release certificate: the exact
// source revision, the migrations in force, the ledger's digest, one result per cited test, and a verdict.
//
// It certifies nothing by itself. The verdict is CERTIFIED only if every cited test ran and passed (a skipped test
// proves nothing and counts as a failure), no entry is partial, and every recorded deviation has an owner's
// acceptance in the ledger.
//
//	cd services/_certification
//	TEST_DATABASE_URL=postgres://... go run ./cmd/intcert -out certificate.json
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cert "zoiko.io/certification"
)

type testResult struct {
	Ref    string `json:"ref"`
	Result string `json:"result"` // pass | fail | skip | missing
}

type entryResult struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Status   string       `json:"ledger_status"`
	Proven   bool         `json:"proven"`
	Note     string       `json:"note,omitempty"`
	Evidence []testResult `json:"evidence"`
}

type certificate struct {
	GeneratedAt string            `json:"generated_at"`
	GitCommit   string            `json:"git_commit"`
	GitDirty    bool              `json:"git_dirty"`
	LedgerSHA   string            `json:"ledger_sha256"`
	Migrations  map[string]string `json:"migrations_sha256"`
	Entries     []entryResult     `json:"entries"`
	Summary     map[string]int    `json:"summary"`
	Open        []string          `json:"open"`
	Verdict     string            `json:"verdict"`
}

func main() {
	services := flag.String("services", "..", "path to the services directory")
	out := flag.String("out", "certificate.json", "where to write the certificate")
	reportOnly := flag.Bool("report-only", false, "exit 0 even when not certified")
	flag.Parse()

	if p := cert.Problems(*services); len(p) > 0 {
		for _, s := range p {
			fmt.Fprintln(os.Stderr, "ledger:", s)
		}
		os.Exit(2)
	}
	results := runAll(*services)

	c := certificate{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Migrations: map[string]string{}, Summary: map[string]int{}}
	c.GitCommit, c.GitDirty = gitState(*services)
	c.LedgerSHA = ledgerDigest()
	for svc := range serviceSet() {
		c.Migrations[svc] = migrationDigest(filepath.Join(*services, svc))
	}
	for _, e := range cert.All() {
		er := entryResult{ID: e.ID, Title: e.Title, Status: e.Status, Note: e.Note, Proven: len(e.Evidence) > 0}
		for _, raw := range e.Evidence {
			r := results[raw]
			if r == "" {
				r = "missing"
			}
			er.Evidence = append(er.Evidence, testResult{Ref: raw, Result: r})
			if r != "pass" {
				er.Proven = false
			}
		}
		c.Entries = append(c.Entries, er)
		c.Summary[e.Status]++
		if !er.Proven {
			c.Summary["UNPROVEN"]++
		}
	}
	for _, e := range cert.Open() {
		c.Open = append(c.Open, e.ID+" ("+e.Status+"): "+e.Note)
	}
	var unproven []string
	for _, e := range c.Entries {
		if !e.Proven {
			unproven = append(unproven, e.ID+": a cited test did not pass")
		}
	}
	c.Open = append(c.Open, unproven...)
	switch {
	case len(c.Open) == 0 && !c.GitDirty:
		c.Verdict = "CERTIFIED"
	case len(c.Open) == 0:
		c.Verdict = "NOT CERTIFIED: the working tree is not the committed revision"
	default:
		c.Verdict = fmt.Sprintf("NOT CERTIFIED: %d open item(s)", len(c.Open))
	}

	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Printf("%s\n  controls+scenarios: %v\n  written to %s\n", c.Verdict, c.Summary, *out)
	for _, o := range c.Open {
		fmt.Println("  open:", o)
	}
	if c.Verdict != "CERTIFIED" && !*reportOnly {
		os.Exit(1)
	}
}

func serviceSet() map[string]bool {
	out := map[string]bool{}
	for _, e := range cert.All() {
		for _, raw := range e.Evidence {
			if r, err := cert.Parse(raw); err == nil {
				out[r.Service] = true
			}
		}
	}
	return out
}

// runAll runs each cited package once with every test it is cited for.
func runAll(services string) map[string]string {
	type pkg struct{ svc, dir string }
	tests := map[pkg]map[string]bool{}
	refs := map[string][]pkg{}
	for _, e := range cert.All() {
		for _, raw := range e.Evidence {
			r, _ := cert.Parse(raw)
			k := pkg{r.Service, r.Dir}
			if tests[k] == nil {
				tests[k] = map[string]bool{}
			}
			tests[k][r.Test] = true
			refs[raw] = append(refs[raw], k)
		}
	}
	got := map[pkg]map[string]string{}
	keys := make([]pkg, 0, len(tests))
	for k := range tests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].svc+keys[i].dir < keys[j].svc+keys[j].dir })
	for _, k := range keys {
		var names []string
		for n := range tests[k] {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Printf("running %s/%s (%d tests)\n", k.svc, k.dir, len(names))
		cmd := exec.Command("go", "test", "-json", "-tags=integration", "-count=1", "-run", "^("+strings.Join(names, "|")+")$", "./"+k.dir)
		cmd.Dir = filepath.Join(services, k.svc)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		_ = cmd.Run() // a failing package is reported per test below
		got[k] = parse(buf.Bytes())
	}
	out := map[string]string{}
	for raw, ks := range refs {
		r, _ := cert.Parse(raw)
		out[raw] = got[ks[0]][r.Test]
	}
	return out
}

func parse(b []byte) map[string]string {
	res := map[string]string{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var ev struct {
			Action, Test string
		}
		if json.Unmarshal(line, &ev) != nil || ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			res[ev.Test] = ev.Action
		}
	}
	return res
}

func gitState(services string) (commit string, dirty bool) {
	run := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = services
		o, _ := c.Output()
		return strings.TrimSpace(string(o))
	}
	commit = run("rev-parse", "HEAD")
	dirty = run("status", "--porcelain", "--", ".") != ""
	return
}

func ledgerDigest() string {
	b, _ := json.Marshal(cert.All())
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func migrationDigest(dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "deployments", "migrations", "*.sql"))
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, _ := os.ReadFile(f)
		h.Write([]byte(filepath.Base(f)))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
