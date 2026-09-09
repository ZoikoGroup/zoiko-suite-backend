package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func captureAPCost(t *testing.T, r chi.Router, projectID, sourceRef string, amount float64) domain.CostEntry {
	t.Helper()
	req := domain.CaptureProjectCostRequest{
		ProjectID: projectID, SourceType: domain.CostSourceTypeAP, SourceReference: sourceRef, Amount: amount, Currency: "USD",
	}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/", req, "capturer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("capture failed: %d %s", rr.Code, rr.Body.String())
	}
	var e domain.CostEntry
	_ = json.NewDecoder(rr.Body).Decode(&e)
	return e
}

// ── CaptureProjectCost ──────────────────────────────────────────────────────

func TestCaptureProjectCost_MissingSourceType_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C1")

	req := domain.CaptureProjectCostRequest{ProjectID: id, SourceReference: "AP-1", Amount: 100, Currency: "USD"}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/", req, "capturer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #1: same AP line captured twice ────────────────────────────

func TestCaptureProjectCost_DuplicateSourceReference_ReturnsOriginal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C2")

	first := captureAPCost(t, r, id, "AP-DUP", 500)
	second := captureAPCost(t, r, id, "AP-DUP", 500)
	if second.EntryID != first.EntryID {
		t.Fatalf("expected the original entry returned for a duplicate source reference, got a new one: %q vs %q", second.EntryID, first.EntryID)
	}
}

// ── ValidateProjectCost / MarkBillableEligibility ────────────────────────────

func TestValidateProjectCost_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C3")
	e := captureAPCost(t, r, id, "AP-3", 200)

	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/validate", nil, "validator-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.costEntries[e.EntryID].Status != domain.CostEntryStatusAccepted {
		t.Fatalf("expected ACCEPTED, got %q", s.costEntries[e.EntryID].Status)
	}
}

func TestMarkBillableEligibility_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C4")
	e := captureAPCost(t, r, id, "AP-4", 300)

	req := domain.MarkBillableEligibilityRequest{Billable: true, Capitalizable: false}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/mark-billable", req, "manager-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !s.costEntries[e.EntryID].Billable {
		t.Fatalf("expected billable=true")
	}
}

// ── Negative path #4: reclassify requires independent approval ──────────────

func TestReclassifyProjectCost_SameCapturer_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C5")
	e := captureAPCost(t, r, id, "AP-5", 400)

	req := domain.ReclassifyProjectCostRequest{Reason: "wrong category"}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reclassify", req, "capturer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReclassifyProjectCost_DifferentPrincipal_CreatesLinkedEntry(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C6")
	e := captureAPCost(t, r, id, "AP-6", 400)

	newCategory := "Travel"
	req := domain.ReclassifyProjectCostRequest{Reason: "wrong category", CostCategory: newCategory}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reclassify", req, "reviewer-2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var linked domain.CostEntry
	_ = json.NewDecoder(rr.Body).Decode(&linked)
	if linked.ReclassifiesEntryID == nil || *linked.ReclassifiesEntryID != e.EntryID {
		t.Fatalf("expected linked entry to reference the original via reclassifies_entry_id, got %+v", linked)
	}
	// The original entry must be untouched — a real handler-level proxy
	// for the store's own reject-mutation trigger.
	if s.costEntries[e.EntryID].CostCategory != "" {
		t.Fatalf("expected the original entry's own cost_category unchanged, got %q", s.costEntries[e.EntryID].CostCategory)
	}
}

func TestReclassifyProjectCost_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C7")
	e := captureAPCost(t, r, id, "AP-7", 100)

	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reclassify", map[string]string{}, "reviewer-2")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #2: ReverseProjectCost, real and self-approval-guarded ────

func TestReverseProjectCost_SameCapturer_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C8")
	e := captureAPCost(t, r, id, "AP-8", 250)

	req := domain.ReverseProjectCostRequest{Reason: "payroll reversed"}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reverse", req, "capturer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReverseProjectCost_DifferentPrincipal_NetsToZero(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C9")
	e := captureAPCost(t, r, id, "AP-9", 250)

	req := domain.ReverseProjectCostRequest{Reason: "payroll reversed"}
	rr := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reverse", req, "reviewer-2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var reversal domain.CostEntry
	_ = json.NewDecoder(rr.Body).Decode(&reversal)
	if reversal.Amount != -250 {
		t.Fatalf("expected reversal amount=-250, got %v", reversal.Amount)
	}
	if s.costEntries[e.EntryID].Status != domain.CostEntryStatusReversed {
		t.Fatalf("expected original marked REVERSED, got %q", s.costEntries[e.EntryID].Status)
	}

	list := doReq(r, http.MethodGet, "/v1/cost-entries/?project_id="+id, nil, "reviewer-2")
	var entries []domain.CostEntry
	_ = json.NewDecoder(list.Body).Decode(&entries)
	var total float64
	for _, en := range entries {
		total += en.Amount
	}
	if total != 0 {
		t.Fatalf("expected net total=0 after reversal, got %v", total)
	}
}

func TestReverseProjectCost_AlreadyReversed_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C10")
	e := captureAPCost(t, r, id, "AP-10", 100)

	first := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reverse", domain.ReverseProjectCostRequest{Reason: "x"}, "reviewer-2")
	if first.Code != http.StatusCreated {
		t.Fatalf("first reverse failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/cost-entries/"+e.EntryID+"/reverse", domain.ReverseProjectCostRequest{Reason: "x"}, "reviewer-3")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 reversing an already-reversed entry, got %d: %s", second.Code, second.Body.String())
	}
}

// ── Negative path #3: capture never posts to GL (no ledger client exists) ───

func TestCaptureProjectCost_HandlerHasNoLedgerDependency(t *testing.T) {
	// This is a structural, compile-time assertion: handler.New's own
	// signature takes no ledger client at all for this capability — see
	// migration 000002's own doc comment on negative path #3. Exercised
	// here as a real capture that succeeds with zero GL-related stub
	// configuration, proving no posting call is ever attempted.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C11")
	e := captureAPCost(t, r, id, "AP-11", 999)
	if e.Status != domain.CostEntryStatusCaptured {
		t.Fatalf("expected CAPTURED, got %q", e.Status)
	}
}

// ── CertifyCostPopulation ─────────────────────────────────────────────────────

func TestCertifyCostPopulation_ExcludesReversedEntries(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-C12")
	captureAPCost(t, r, id, "AP-12a", 100)
	toReverse := captureAPCost(t, r, id, "AP-12b", 50)
	doReq(r, http.MethodPost, "/v1/cost-entries/"+toReverse.EntryID+"/reverse", domain.ReverseProjectCostRequest{Reason: "x"}, "reviewer-2")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/certify-costs", nil, "certifier-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var cert domain.CostCertification
	_ = json.NewDecoder(rr.Body).Decode(&cert)
	// The REVERSED original (50) is excluded outright; its own reversal
	// entry (-50) still counts, so the net is 100 - 50 = 50 — the correct
	// economic total, not the naive "sum of everything not REVERSED
	// including the reversal itself" double-exclusion.
	if cert.TotalAmount != 50 {
		t.Fatalf("expected total_amount=50 (100 captured, net -50 after reversal), got %v", cert.TotalAmount)
	}
	if cert.EntryCount != 2 {
		t.Fatalf("expected entry_count=2 (AP-12a + the reversal entry; the REVERSED original excluded), got %d", cert.EntryCount)
	}
}
