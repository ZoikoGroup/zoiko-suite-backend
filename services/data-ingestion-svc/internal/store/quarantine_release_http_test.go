package store_test

import (
	"fmt"
	"testing"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

// ZS-SVC-Z-001 NP-16 / INV-12 over HTTP (docs/architecture/DQC_GAP_MATRIX.md, step E).

// A FAILED item that is retried keeps its original delivery key: its first attempt may have been
// committed by the owning domain, which is exactly what the key protects against. Only a release
// from quarantine, which follows a REFUSAL, delivers under a key of its own.
func TestRetry_OfATransientFailureKeepsTheOriginalDeliveryKey(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	e.dom.failFirst["INV-5"] = struct {
		n    int
		code int
	}{99, 503} // the domain keeps failing INV-5 until the item is FAILED
	done := e.drain(e.executing(p, csvOf(goodRows(5, entityOne)...), declared{}))
	failed, err := e.svc.Items(ictx, e.tenant, done.BatchID, imports.ItemFailed, 0, 10)
	iMust(t, err)
	if len(failed) != 1 {
		t.Fatalf("setup: want one FAILED item, got %+v", failed)
	}
	iMust(t, e.svc.RetryItem(ictx, e.actor(approver), done.BatchID, failed[0].RowNo, "the domain is back"))
	it, err := e.st.GetItem(ictx, e.tenant, done.BatchID, failed[0].RowNo)
	iMust(t, err)
	if it.ReleaseGeneration != 0 || imports.DeliveryKey(*it) != it.IdempotencyKey {
		t.Fatalf("a transient-failure retry changed the delivery key: generation %d, key %q vs %q",
			it.ReleaseGeneration, imports.DeliveryKey(*it), it.IdempotencyKey)
	}
}

func TestImportAPI_ReleaseHasItsOwnPermissionAndAStrictContract(t *testing.T) {
	a := newImpAPI(t)
	b := a.e.executing(a.e.profile(plainSpec()), csvOf(goodRows(3, entityOne)...), declared{})
	path := func(suffix string) string { return fmt.Sprintf("/imports/%s%s", b.BatchID, suffix) }
	if q := a.tenant("POST", path("/items/2:quarantine"), approver, map[string]any{"reason": "disputed", "owner": "ap-team"}); q.Code != 200 {
		t.Fatalf("quarantine: %d %s", q.Code, q.Raw)
	}
	if done := a.e.drain(b); done.Status != imports.StatusCompletedWithExceptions {
		t.Fatal(done.Status)
	}
	body := map[string]any{"reason": "dispute resolved", "correction_ref": "credit-note CN-2041"}

	// The authorization client caches a decision for a few seconds per (principal, action), so each
	// denial below belongs to a principal of its own rather than being toggled on a shared one.

	// Asking for a release needs the permission to work items...
	a.authz.deny["viewer-only|DATA_IMPORT_ITEM_MANAGE"] = true
	if r := a.tenant("POST", path("/items/2:request-release"), "viewer-only", body); r.Code != 403 {
		t.Fatalf("a request without ITEM_MANAGE: %d %s", r.Code, r.Raw)
	}

	// ...a strict body: an unknown field is refused, not ignored, and a missing correction is refused...
	if r := a.tenant("POST", path("/items/2:request-release"), uploader, map[string]any{"reason": "x", "correction_ref": "y", "extra": 1}); r.Code != 400 {
		t.Fatalf("an unknown field: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("POST", path("/items/2:request-release"), uploader, map[string]any{"reason": "no correction named"}); r.Code != 400 {
		t.Fatalf("a request that names no correction: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("POST", path("/items/zero:request-release"), uploader, body); r.Code != 400 {
		t.Fatalf("a non-numeric row: %d", r.Code)
	}
	if r := a.tenant("POST", path("/items/2:request-release"), uploader, body); r.Code != 202 {
		t.Fatalf("a valid request: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("POST", path("/items/2:request-release"), uploader, body); r.Code != 409 {
		t.Fatalf("a second open request: %d %s", r.Code, r.Raw)
	}

	// ...but DECIDING needs its own permission. ITEM_MANAGE, which is all the requester needs, is not enough.
	a.authz.deny["item-manager|DATA_IMPORT_QUARANTINE_RELEASE"] = true
	if r := a.tenant("POST", path("/items/2:approve-release"), "item-manager", map[string]any{"reason": "ok"}); r.Code != 403 {
		t.Fatalf("approving without the release permission: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("POST", path("/items/2:refuse-release"), "item-manager", map[string]any{"reason": "no"}); r.Code != 403 {
		t.Fatalf("refusing without the release permission: %d %s", r.Code, r.Raw)
	}
	if n := a.e.counts(b.BatchID)[imports.ItemQuarantined]; n != 1 {
		t.Fatalf("a refused decision changed the item: %d quarantined", n)
	}
	// A decision needs a reason.
	if r := a.tenant("POST", path("/items/2:approve-release"), releaseController, map[string]any{"reason": ""}); r.Code != 400 {
		t.Fatalf("a decision with no reason: %d %s", r.Code, r.Raw)
	}

	// The quarantine view needs the read permission, and reports the state and the ledger.
	a.authz.deny["blind|DATA_IMPORT_READ"] = true
	if r := a.tenant("GET", path("/quarantine"), "blind", nil); r.Code != 403 {
		t.Fatalf("the quarantine view without READ: %d", r.Code)
	}
	v := a.tenant("GET", path("/quarantine"), uploader, nil)
	if v.Code != 200 {
		t.Fatalf("quarantine view: %d %s", v.Code, v.Raw)
	}
	sum, _ := v.JSON["summary"].(map[string]any)
	if str(sum, "state") != "RELEASE_PENDING" {
		t.Fatalf("with an open request the state is RELEASE_PENDING: %s", v.Raw)
	}
	if ledger, _ := v.JSON["ledger"].([]any); len(ledger) != 2 {
		t.Fatalf("ledger entries: %s", v.Raw)
	}
	iv := a.tenant("GET", path("/items/2/quarantine"), uploader, nil)
	if iv.Code != 200 {
		t.Fatalf("item quarantine view: %d %s", iv.Code, iv.Raw)
	}

	// Finally a refusal, which closes the request and leaves the item held.
	if r := a.tenant("POST", path("/items/2:refuse-release"), releaseController, map[string]any{"reason": "credit note not in the AR ledger"}); r.Code != 200 {
		t.Fatalf("refuse: %d %s", r.Code, r.Raw)
	}
	if str(a.tenant("GET", path("/quarantine"), uploader, nil).JSON["summary"].(map[string]any), "state") != "PARTIAL" {
		t.Fatal("after a refusal the state returns to PARTIAL")
	}

	// Another tenant cannot see this batch's quarantine at all.
	if r := a.do("GET", path("/quarantine"), "99999999-9999-9999-9999-999999999999", uploader, nil); r.Code != 404 {
		t.Fatalf("another tenant read the quarantine view: %d %s", r.Code, r.Raw)
	}
}
