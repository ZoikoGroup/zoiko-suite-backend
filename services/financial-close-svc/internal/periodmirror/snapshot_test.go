package periodmirror

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
)

func TestSnapshotRef_DocumentedFormat(t *testing.T) {
	got := SnapshotRef("fp-1", "HARD_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: false, BlockingIssues: []string{"b", "a"}})
	// The exact canonical document documented in migration 000015 / SnapshotRef.
	canon := `{"v":1,"fiscal_period_id":"fp-1","command":"HARD_CLOSE","evidence_document_id":"doc-1","readiness":{"is_ready":false,"blocking_issues":["a","b"]}}`
	sum := sha256.Sum256([]byte(canon))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hash is not sha256 of the documented canonical JSON\n got %s\nwant %s", got, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("not lowercase sha256 hex: %s", got)
	}
	// nil readiness encodes as null; empty and nil issues both encode as [].
	canonNil := `{"v":1,"fiscal_period_id":"fp-1","command":"AUTHORIZE_REOPEN","evidence_document_id":"","readiness":null}`
	sum = sha256.Sum256([]byte(canonNil))
	if got := SnapshotRef("fp-1", "AUTHORIZE_REOPEN", "", nil); got != hex.EncodeToString(sum[:]) {
		t.Fatal("nil readiness must hash as null")
	}
}

func TestSnapshotRef_Deterministic(t *testing.T) {
	r := &ReadinessSnapshot{IsReady: true}
	a := SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", r)
	for i := 0; i < 50; i++ {
		if SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true}) != a {
			t.Fatal("same inputs must give the same hash")
		}
	}
	// nil and empty issue slices are the same readiness.
	if SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true, BlockingIssues: []string{}}) != a {
		t.Fatal("nil vs empty blocking issues must hash the same")
	}
	// issue ORDER must not matter; the input slice must not be mutated.
	in := []string{"z", "a"}
	x := SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{BlockingIssues: in})
	y := SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{BlockingIssues: []string{"a", "z"}})
	if x != y || in[0] != "z" {
		t.Fatal("order-insensitive and non-mutating expected")
	}
}

func TestSnapshotRef_DifferentInputsDifferentHash(t *testing.T) {
	base := SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true})
	seen := map[string]string{"base": base}
	for name, h := range map[string]string{
		"readiness flag": SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: false}),
		"blocking issue": SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true, BlockingIssues: []string{"unposted_journals_exist: 1"}}),
		"other issue":    SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true, BlockingIssues: []string{"unposted_journals_exist: 2"}}),
		"command":        SnapshotRef("fp-1", "HARD_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true}),
		"evidence doc":   SnapshotRef("fp-1", "SOFT_CLOSE", "doc-2", &ReadinessSnapshot{IsReady: true}),
		"period":         SnapshotRef("fp-2", "SOFT_CLOSE", "doc-1", &ReadinessSnapshot{IsReady: true}),
		"no readiness":   SnapshotRef("fp-1", "SOFT_CLOSE", "doc-1", nil),
	} {
		for prev, ph := range seen {
			if h == ph {
				t.Errorf("%s collides with %s", name, prev)
			}
		}
		seen[name] = h
	}
}
