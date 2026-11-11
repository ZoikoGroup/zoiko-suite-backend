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
		"com.zoikosuite.accounting.journal.created": {"b.go"},
	}
	r, err := Reconcile(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conforming) != 1 || len(r.Uncatalogued) != 1 || r.Uncatalogued[0] != "com.zoikosuite.accounting.journal.created" {
		t.Fatalf("bad reconciliation: %+v", r)
	}
	if len(r.NeverEmitted) != 195 {
		t.Fatalf("expected 195 catalogued-but-not-emitted, got %d", len(r.NeverEmitted))
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
