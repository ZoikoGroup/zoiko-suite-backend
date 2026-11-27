package store_test

import (
	"fmt"
	"testing"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

// ZS-SVC-Z-001 section 11: POST /dqc/quarantine/release, "optimistic concurrency + authority check +
// reason required" (docs/architecture/DQC_GAP_MATRIX.md, step F3).

const dqcRelease = "/dqc/quarantine/release"

// quarantinedWithRequest returns an API with one batch whose item 2 is quarantined and has an open release
// request, plus that request's ledger id as the quarantine view reports it.
func quarantinedWithRequest(t *testing.T) (*impAPI, string, int64) {
	t.Helper()
	a := newImpAPI(t)
	b := a.e.executing(a.e.profile(plainSpec()), csvOf(goodRows(3, entityOne)...), declared{})
	if q := a.tenant("POST", fmt.Sprintf("/imports/%s/items/2:quarantine", b.BatchID), approver, map[string]any{"reason": "disputed", "owner": "ap-team"}); q.Code != 200 {
		t.Fatalf("quarantine: %d %s", q.Code, q.Raw)
	}
	if done := a.e.drain(b); done.Status != imports.StatusCompletedWithExceptions {
		t.Fatal(done.Status)
	}
	return a, b.BatchID, a.requestRelease(t, b.BatchID, "credit note CN-1")
}

func (a *impAPI) requestRelease(t *testing.T, batchID, correction string) int64 {
	t.Helper()
	body := map[string]any{"reason": "dispute resolved", "correction_ref": correction}
	if r := a.tenant("POST", fmt.Sprintf("/imports/%s/items/2:request-release", batchID), uploader, body); r.Code != 202 {
		t.Fatalf("request release: %d %s", r.Code, r.Raw)
	}
	return a.openRequestID(t, batchID)
}

// openRequestID reads the latest RELEASE_REQUESTED entry from the quarantine view, as a client would.
func (a *impAPI) openRequestID(t *testing.T, batchID string) int64 {
	t.Helper()
	v := a.tenant("GET", fmt.Sprintf("/imports/%s/items/2/quarantine", batchID), uploader, nil)
	if v.Code != 200 {
		t.Fatalf("view: %d %s", v.Code, v.Raw)
	}
	ledger, _ := v.JSON["ledger"].([]any)
	var id int64
	for _, e := range ledger {
		m := e.(map[string]any)
		if m["kind"] == "RELEASE_REQUESTED" {
			id = int64(m["ledger_id"].(float64))
		}
	}
	if id == 0 {
		t.Fatalf("no release request in the view: %s", v.Raw)
	}
	return id
}

func decision(batchID, d, reason string, requestID int64) map[string]any {
	return map[string]any{"batch_id": batchID, "row_no": 2, "decision": d, "reason": reason, "request_ledger_id": requestID}
}

func TestDQCRelease_DecidesTheNamedRequest(t *testing.T) {
	a, batch, reqID := quarantinedWithRequest(t)
	r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "RELEASE", "verified against the AR ledger", reqID))
	if r.Code != 200 || str(r.JSON, "release") != "RELEASED" {
		t.Fatalf("release: %d %s", r.Code, r.Raw)
	}
	if done := a.e.drain(&imports.Batch{BatchID: batch}); done.Status != imports.StatusCompleted {
		t.Fatalf("after the release the item was not re-validated and imported: %s", done.Status)
	}
}

func TestDQCRelease_RefuseKeepsTheItemQuarantined(t *testing.T) {
	a, batch, reqID := quarantinedWithRequest(t)
	r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "REFUSE", "credit note not in the ledger", reqID))
	if r.Code != 200 || str(r.JSON, "release") != "REFUSED" {
		t.Fatalf("refuse: %d %s", r.Code, r.Raw)
	}
	v := a.tenant("GET", fmt.Sprintf("/imports/%s/quarantine", batch), uploader, nil)
	if str(v.JSON["summary"].(map[string]any), "state") != "PARTIAL" {
		t.Fatalf("after a refusal the state is not PARTIAL: %s", v.Raw)
	}
}

// The optimistic-concurrency rule: a decision made on a request that has since been replaced is refused.
func TestDQCRelease_StaleRequestIsRefused(t *testing.T) {
	a, batch, first := quarantinedWithRequest(t)
	// Someone else refuses the first request and a second, differently evidenced one is opened.
	if r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "REFUSE", "not enough", first)); r.Code != 200 {
		t.Fatalf("refuse first: %d %s", r.Code, r.Raw)
	}
	second := a.requestRelease(t, batch, "credit note CN-2 and the AR ledger extract")
	if second == first {
		t.Fatal("setup: the second request has the first's id")
	}
	// A decider who still holds the first request's id cannot apply their decision to the second.
	if r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "RELEASE", "approving what I read earlier", first)); r.Code != 409 {
		t.Fatalf("a stale decision: %d %s", r.Code, r.Raw)
	}
	if n := a.e.counts(batch)[imports.ItemQuarantined]; n != 1 {
		t.Fatalf("a refused stale decision changed the item: %d quarantined", n)
	}
	// The same decision naming the CURRENT request is accepted: it was staleness that was refused.
	if r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "RELEASE", "verified", second)); r.Code != 200 {
		t.Fatalf("a decision on the current request: %d %s", r.Code, r.Raw)
	}
}

func TestDQCRelease_StrictRequest(t *testing.T) {
	a, batch, reqID := quarantinedWithRequest(t)
	for name, body := range map[string]map[string]any{
		"no request named": decision(batch, "RELEASE", "x", 0),
		"unknown decision": decision(batch, "MAYBE", "x", reqID),
		"no decision":      decision(batch, "", "x", reqID),
		"no reason":        decision(batch, "RELEASE", "", reqID),
		"no batch":         decision("", "RELEASE", "x", reqID),
		"row zero":         {"batch_id": batch, "row_no": 0, "decision": "RELEASE", "reason": "x", "request_ledger_id": reqID},
		"unknown field":    {"batch_id": batch, "row_no": 2, "decision": "RELEASE", "reason": "x", "request_ledger_id": reqID, "force": true},
	} {
		if r := a.tenant("POST", dqcRelease, releaseController, body); r.Code != 400 {
			t.Fatalf("%s: %d %s", name, r.Code, r.Raw)
		}
	}
	if n := a.e.counts(batch)[imports.ItemQuarantined]; n != 1 {
		t.Fatalf("a malformed request changed the item: %d quarantined", n)
	}
	// An id that is no request of this item is stale, not an error of form.
	if r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "RELEASE", "x", reqID+9999)); r.Code != 409 {
		t.Fatalf("an unknown request id: %d %s", r.Code, r.Raw)
	}
	// Proof that the refusals were about their defect: the well-formed request works.
	if r := a.tenant("POST", dqcRelease, releaseController, decision(batch, "RELEASE", "x", reqID)); r.Code != 200 {
		t.Fatalf("a well-formed decision: %d %s", r.Code, r.Raw)
	}
}

func TestDQCRelease_AuthorityAndIndependenceAreUnchanged(t *testing.T) {
	a, batch, reqID := quarantinedWithRequest(t)
	// The requester cannot decide their own request (NP-16), by this route either.
	if r := a.tenant("POST", dqcRelease, uploader, decision(batch, "RELEASE", "my own request", reqID)); r.Code != 403 {
		t.Fatalf("the requester approved their own request: %d %s", r.Code, r.Raw)
	}
	// Deciding needs the release permission, not merely the permission to work items.
	a.authz.deny["item-manager|DATA_IMPORT_QUARANTINE_RELEASE"] = true
	if r := a.tenant("POST", dqcRelease, "item-manager", decision(batch, "RELEASE", "x", reqID)); r.Code != 403 {
		t.Fatalf("a decision without the release permission: %d %s", r.Code, r.Raw)
	}
	// Denying a different permission does not block it.
	a.authz.deny["other-controller|DATA_IMPORT_ITEM_MANAGE"] = true
	if r := a.tenant("POST", dqcRelease, "other-controller", decision(batch, "REFUSE", "x", reqID)); r.Code != 200 {
		t.Fatalf("denying ITEM_MANAGE blocked a release decision: %d %s", r.Code, r.Raw)
	}
	// Another tenant cannot see the batch at all.
	if r := a.do("POST", dqcRelease, "99999999-9999-9999-9999-999999999999", releaseController, decision(batch, "RELEASE", "x", reqID)); r.Code != 404 {
		t.Fatalf("another tenant: %d %s", r.Code, r.Raw)
	}
}

// The store enforces it too, under the item's row lock: not merely the HTTP layer.
func TestDQCRelease_StoreRefusesAStaleRequest(t *testing.T) {
	a, batch, reqID := quarantinedWithRequest(t)
	err := a.e.svc.DecideReleaseOfRequest(ictx, a.e.actor(releaseController), batch, 2, "x", true, reqID+1)
	if err == nil || err.Error() != imports.ErrStaleRelease.Error() {
		t.Fatalf("want ErrStaleRelease, got %v", err)
	}
	if err := a.e.svc.DecideReleaseOfRequest(ictx, a.e.actor(releaseController), batch, 2, "x", true, 0); err == nil {
		t.Fatal("a decision naming no request was accepted by the service")
	}
	if err := a.e.svc.DecideReleaseOfRequest(ictx, a.e.actor(releaseController), batch, 2, "x", true, reqID); err != nil {
		t.Fatalf("a decision on the current request: %v", err)
	}
}
