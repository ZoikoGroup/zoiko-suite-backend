package certification

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Ref is a parsed evidence reference.
type Ref struct {
	Raw     string
	Service string // module directory under services/, e.g. "identity-context-svc"
	Dir     string // package directory relative to the module, e.g. "internal/store"
	File    string // absolute-ish path under services/
	Test    string
}

// Parse resolves "<prefix>:<path>::<TestName>" against the prefix table.
func Parse(raw string) (Ref, error) {
	prefix, rest, ok := strings.Cut(raw, ":")
	if !ok {
		return Ref{}, fmt.Errorf("%q: no prefix", raw)
	}
	base, known := Prefixes[prefix]
	if !known {
		return Ref{}, fmt.Errorf("%q: unknown prefix %q", raw, prefix)
	}
	path, test, ok := strings.Cut(rest, "::")
	if !ok || !strings.HasPrefix(test, "Test") || !strings.HasSuffix(path, "_test.go") {
		return Ref{}, fmt.Errorf("%q: want <prefix>:<dir>/<file>_test.go::TestName", raw)
	}
	full := filepath.ToSlash(filepath.Join(base, path))
	parts := strings.SplitN(full, "/", 2)
	module := parts[0]
	// the eventing module root is eventing/, the others keep their code under internal/
	dir := filepath.ToSlash(filepath.Dir(parts[1]))
	return Ref{Raw: raw, Service: module, Dir: dir, File: full, Test: test}, nil
}

// All returns every entry, controls first.
func All() []Entry { return append(append([]Entry{}, Controls...), Scenarios...) }

// Acceptance records a named owner's sign-off of a DEVIATION. An entry with a deviation and no acceptance is open.
type Acceptance struct{ By, On, Reference string }

// Acceptances holds the decisions taken so far. They were taken by the product owner in the Wave 9c session; a formal
// sign-off by the named control owner can replace By/Reference without changing the ledger's shape.
var Acceptances = map[string]Acceptance{
	"INT-NP-042":   {By: "yash@zoikogroup.com", On: "2026-10-19", Reference: "Wave 9c session decision: SAML federation deferred, OIDC only"},
	"INT-NP-047":   {By: "yash@zoikogroup.com", On: "2026-10-19", Reference: "Wave 9c session decision: hash-only provider evidence"},
	"INT-CTRL-026": {By: "yash@zoikogroup.com", On: "2026-10-19", Reference: "Wave 9c session decision: bank payment files out of scope, API only"},
}

// Problems checks the ledger against the source tree under servicesDir. It proves the ledger is well formed and that
// every cited test exists; it does not run them (see Run).
func Problems(servicesDir string) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	seen := map[string]bool{}
	expect := func(prefix string, n int, list []Entry) {
		have := map[string]bool{}
		for _, e := range list {
			if have[e.ID] {
				add("%s appears twice", e.ID)
			}
			have[e.ID] = true
		}
		for i := 1; i <= n; i++ {
			id := fmt.Sprintf("%s-%03d", prefix, i)
			if !have[id] {
				add("%s is missing from the ledger", id)
			}
		}
		if len(list) != n {
			add("%s: the ledger holds %d entries, the specification defines %d", prefix, len(list), n)
		}
	}
	expect("INT-CTRL", 36, Controls)
	expect("INT-NP", 48, Scenarios)

	cache := map[string]string{}
	for _, e := range All() {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		switch e.Status {
		case Covered:
			if len(e.Evidence) == 0 {
				add("%s is COVERED with no evidence", e.ID)
			}
		case Partial, Deviation, OutOfScope:
			if strings.TrimSpace(e.Note) == "" {
				add("%s is %s with no note saying what is missing", e.ID, e.Status)
			}
		default:
			add("%s has unknown status %q", e.ID, e.Status)
		}
		for _, raw := range e.Evidence {
			r, err := Parse(raw)
			if err != nil {
				add("%s: %v", e.ID, err)
				continue
			}
			src, ok := cache[r.File]
			if !ok {
				b, err := os.ReadFile(filepath.Join(servicesDir, filepath.FromSlash(r.File)))
				if err != nil {
					add("%s: %s does not exist", e.ID, r.File)
					cache[r.File] = "\x00missing"
					continue
				}
				src = string(b)
				cache[r.File] = src
			}
			if src == "\x00missing" {
				add("%s: %s does not exist", e.ID, r.File)
				continue
			}
			if !regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(r.Test) + `\(`).MatchString(src) {
				add("%s: %s has no %s", e.ID, r.File, r.Test)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Open lists the entries that stand between this ledger and certification: anything partial, and any deviation
// that no owner has accepted.
func Open() []Entry {
	var out []Entry
	for _, e := range All() {
		if e.Status == Partial {
			out = append(out, e)
		}
		if e.Status == Deviation || e.Status == OutOfScope {
			if _, ok := Acceptances[e.ID]; !ok {
				out = append(out, e)
			}
		}
	}
	return out
}
