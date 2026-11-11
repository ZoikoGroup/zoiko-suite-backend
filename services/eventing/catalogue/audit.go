package catalogue

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// typeLiteral matches a complete canonical event type written as a Go string
// literal. A type assembled from a prefix constant plus a suffix is not seen:
// the audit is a floor on what services emit, not a ceiling.
var typeLiteral = regexp.MustCompile(`"(com\.zoikosuite\.[a-z0-9-]+\.[a-z0-9-]+(?:\.[a-z0-9-]+)+)"`)

// Emitted maps a canonical event type found in non-test Go source to the
// files (relative to root, slash separated) that mention it.
type Emitted map[string][]string

// ScanServices walks root (the services directory) and returns every complete
// canonical event type literal in non-test Go files. The eventing module's own
// catalogue directory is skipped, since its fixtures are not producers.
func ScanServices(root string) (Emitted, error) {
	out := Emitted{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git":
				return filepath.SkipDir
			case "catalogue":
				if filepath.Base(filepath.Dir(path)) == "eventing" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		seen := map[string]bool{}
		for _, m := range typeLiteral.FindAllSubmatch(b, -1) {
			t := string(m[1])
			if !seen[t] {
				seen[t] = true
				out[t] = append(out[t], rel)
			}
		}
		return nil
	})
	return out, err
}

// Reconciliation compares what services emit with the §19 catalogue.
type Reconciliation struct {
	// Conforming are emitted types that are catalogued.
	Conforming []string
	// Uncatalogued are emitted types the catalogue does not list. Each is
	// either a real fact the catalogue must gain (with an owner) or a
	// non-conforming name to be renamed.
	Uncatalogued []string
	// NeverEmitted are catalogued types no service source mentions yet: the
	// implementation backlog (or a type assembled at runtime).
	NeverEmitted []string
}

// Reconcile compares an Emitted set with the catalogue.
func Reconcile(e Emitted) (Reconciliation, error) {
	all, err := All()
	if err != nil {
		return Reconciliation{}, err
	}
	var r Reconciliation
	cat := map[string]bool{}
	for _, c := range all {
		cat[c.Type] = true
		if _, ok := e[c.Type]; !ok {
			r.NeverEmitted = append(r.NeverEmitted, c.Type)
		}
	}
	for t := range e {
		if cat[t] {
			r.Conforming = append(r.Conforming, t)
		} else {
			r.Uncatalogued = append(r.Uncatalogued, t)
		}
	}
	sort.Strings(r.Conforming)
	sort.Strings(r.Uncatalogued)
	sort.Strings(r.NeverEmitted)
	return r, nil
}

// prefixLiteral matches the first half of a type built at runtime, e.g.
// "com.zoikosuite.accounting.journal." + verb.
var prefixLiteral = regexp.MustCompile(`"(com\.zoikosuite\.[a-z0-9-]+\.[a-z0-9-]+\.)"`)

// ScanPrefixes returns the dynamic type prefixes (domain.aggregate.) found in
// non-test Go files, with the files that use them. These producers assemble
// the final type at runtime, so the audit cannot see their verbs; it can
// only show which aggregate families they emit into.
func ScanPrefixes(root string) (Emitted, error) {
	out := Emitted{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		seen := map[string]bool{}
		for _, m := range prefixLiteral.FindAllSubmatch(b, -1) {
			p := string(m[1])
			if !seen[p] {
				seen[p] = true
				out[p] = append(out[p], rel)
			}
		}
		return nil
	})
	return out, err
}
