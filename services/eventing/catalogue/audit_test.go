package catalogue

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const baselineFile = "testdata/uncatalogued_baseline.txt"

func servicesRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "eventing", "catalogue")); err != nil {
		t.Skip("services directory not found next to eventing")
	}
	return root
}

func readBaseline(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(baselineFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	return out
}

// The ratchet: a service may not start emitting a canonical event type that is
// neither in the section 19 catalogue nor already known. A new type is added
// to catalogue.json (with its owner and consumers) - or, if it is a genuine
// pre-existing extension, deliberately to the baseline in review.
func TestNoNewUncataloguedEventTypes(t *testing.T) {
	e, err := ScanServices(servicesRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Reconcile(e)
	if err != nil {
		t.Fatal(err)
	}
	base := readBaseline(t)
	for _, u := range rec.Uncatalogued {
		if !base[u] {
			t.Errorf("%s (in %s) is emitted but is neither in catalogue.json nor in %s.\n"+
				"Add it to the catalogue with an owner and consumers (ZS-EVENT-001 section 19/23), "+
				"or use a catalogued type.", u, strings.Join(e[u], ", "), baselineFile)
		}
	}
}

// The baseline may only shrink: an entry that is now catalogued or no longer
// emitted must be removed, so the exemption list never hides progress.
func TestBaselineHasNoStaleEntries(t *testing.T) {
	e, err := ScanServices(servicesRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := Reconcile(e)
	unc := map[string]bool{}
	for _, u := range rec.Uncatalogued {
		unc[u] = true
	}
	var stale []string
	for b := range readBaseline(t) {
		if !unc[b] {
			stale = append(stale, b)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("remove from %s (now catalogued or no longer emitted): %v", baselineFile, stale)
	}
}

func TestReconcileClassifiesTypes(t *testing.T) {
	e := Emitted{
		"com.zoikosuite.accounting.journal.posted":  {"a.go"},
		"com.zoikosuite.accounting.journal.edited": {"b.go"},
	}
	r, err := Reconcile(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conforming) != 1 || len(r.Uncatalogued) != 1 || r.Uncatalogued[0] != "com.zoikosuite.accounting.journal.edited" {
		t.Fatalf("bad reconciliation: %+v", r)
	}
	if all, _ := All(); len(r.NeverEmitted) != len(all)-1 {
		t.Fatalf("expected every catalogued type but the one emitted (%d) to be never-emitted, got %d", len(all)-1, len(r.NeverEmitted))
	}
}

// Prints the adoption picture; run with -v.
func TestPrintAdoptionReport(t *testing.T) {
	e, err := ScanServices(servicesRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Reconcile(e)
	t.Logf("emitted canonical types: %d | in catalogue: %d | uncatalogued: %d | catalogued but never emitted: %d",
		len(e), len(r.Conforming), len(r.Uncatalogued), len(r.NeverEmitted))
}

func TestPrintDynamicPrefixes(t *testing.T) {
	p, err := ScanPrefixes(servicesRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d runtime-assembled type prefixes (verbs not visible to a source scan):", len(keys))
	for _, k := range keys {
		t.Logf("  %s  <- %s", k, strings.Join(p[k], ", "))
	}
}

// schemaFiles maps each canonical type to the services whose contracts/events
// directory publishes a schema for it.
func schemaFiles(t *testing.T) map[string][]string {
	t.Helper()
	root := servicesRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "*", "contracts", "events", "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range files {
		typ := "com.zoikosuite." + strings.TrimSuffix(filepath.Base(f), ".schema.json")
		svc := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f))))
		out[typ] = append(out[typ], svc)
	}
	return out
}

// Every published payload schema is for a catalogued type: a schema cannot exist
// for an event the catalogue (and so the owner, consumers and AsyncAPI) does not know.
func TestNoSchemaWithoutACatalogueEntry(t *testing.T) {
	for typ, svcs := range schemaFiles(t) {
		if _, ok := Lookup(typ); !ok {
			t.Errorf("%s has a schema in %v but is not in the catalogue", typ, svcs)
		}
	}
}

// knownSchemaConflicts are types two services publish different schemas for. Each is
// a violation of single ownership (E-02): financial-close-svc and accounting-period-svc
// both emit these period facts with incompatible payloads (about 30 fields keyed by
// period_key versus 7 keyed by fiscal_period_id). Resolve by deciding the owner, then
// delete the loser's schema and its emitter. The list may only shrink.
var knownSchemaConflicts = map[string]bool{
	"com.zoikosuite.accounting.period.reopened":    true,
	"com.zoikosuite.accounting.period.soft-closed": true,
}

func TestOneServicePublishesEachSchema(t *testing.T) {
	seen := map[string]bool{}
	for typ, svcs := range schemaFiles(t) {
		if len(svcs) > 1 {
			seen[typ] = true
			if !knownSchemaConflicts[typ] {
				t.Errorf("%s has schemas in more than one service (%v): one producer must own it (E-02)", typ, svcs)
			}
		}
	}
	for typ := range knownSchemaConflicts {
		if !seen[typ] {
			t.Errorf("%s is no longer in conflict: remove it from knownSchemaConflicts", typ)
		}
	}
}
