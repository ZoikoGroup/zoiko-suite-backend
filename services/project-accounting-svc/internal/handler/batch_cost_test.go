package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"zoiko.io/project-accounting-svc/internal/domain"
	"zoiko.io/project-accounting-svc/internal/handler"
)

func ingest(t *testing.T, s *stubStore, authz *stubAuthZ, projectID string, lines any) (int, handler.CostIngestResponse) {
	t.Helper()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, authz, ledger)
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/costs/ingest", lines, "capturer-1")
	var resp handler.CostIngestResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if ledger.postCalls != 0 || ledger.reverseCalls != 0 {
		t.Fatalf("batch ingestion must never post to the GL")
	}
	return rr.Code, resp
}

func line(ref string, amount float64) domain.CaptureProjectCostRequest {
	return domain.CaptureProjectCostRequest{SourceType: domain.CostSourceTypeAP, SourceReference: ref, Amount: amount, Currency: "USD"}
}

func TestIngestProjectCosts_PartialSuccess_PerLineResults(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "ING-1")

	lines := []domain.CaptureProjectCostRequest{
		line("AP-1", 100),
		line("AP-2", 200),
		line("AP-1", 100), // duplicate of line 0 in the same batch
		{SourceType: "BOGUS", SourceReference: "AP-3", Amount: 1, Currency: "USD"},
		{ProjectID: "some-other-project", SourceType: domain.CostSourceTypeAP, SourceReference: "AP-4", Amount: 1, Currency: "USD"},
	}
	code, resp := ingest(t, s, &stubAuthZ{}, id, lines)
	if code != http.StatusOK {
		t.Fatalf("expected 200 even with rejected lines, got %d", code)
	}
	if resp.Total != 5 || resp.Captured != 2 || resp.Duplicates != 1 || resp.Rejected != 2 {
		t.Fatalf("unexpected counts: %+v", resp)
	}
	want := []string{handler.IngestStatusCaptured, handler.IngestStatusCaptured, handler.IngestStatusDuplicate, handler.IngestStatusRejected, handler.IngestStatusRejected}
	for i, w := range want {
		if resp.Results[i].Index != i || resp.Results[i].Status != w {
			t.Fatalf("line %d: expected %s, got %+v", i, w, resp.Results[i])
		}
	}
	if resp.Results[2].EntryID == "" || resp.Results[2].EntryID != resp.Results[0].EntryID {
		t.Fatalf("duplicate must point at the original entry, got %+v vs %+v", resp.Results[2], resp.Results[0])
	}
	if resp.Results[3].Reason == "" || resp.Results[4].Reason == "" {
		t.Fatalf("rejected lines must carry a reason")
	}
	if len(s.costEntries) != 2 {
		t.Fatalf("expected exactly 2 stored entries, got %d", len(s.costEntries))
	}
}

func TestIngestProjectCosts_ReplayedBatch_AllDuplicates(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "ING-2")
	lines := []domain.CaptureProjectCostRequest{line("AP-1", 1), line("AP-2", 2)}
	ingest(t, s, &stubAuthZ{}, id, lines)
	_, resp := ingest(t, s, &stubAuthZ{}, id, lines)
	if resp.Duplicates != 2 || resp.Captured != 0 {
		t.Fatalf("replay must be all DUPLICATE, got %+v", resp)
	}
}

func TestIngestProjectCosts_InactiveProject_AllRejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "ING-DRAFT")
	code, resp := ingest(t, s, &stubAuthZ{}, p.ProjectID, []domain.CaptureProjectCostRequest{line("AP-1", 1)})
	if code != http.StatusOK || resp.Rejected != 1 || len(s.costEntries) != 0 {
		t.Fatalf("expected the line rejected on a non-ACTIVE project, got %d %+v", code, resp)
	}
}

func TestIngestProjectCosts_AuthzDenied_LinesRejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "ING-AUTHZ")
	code, resp := ingest(t, s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, id, []domain.CaptureProjectCostRequest{line("AP-1", 1)})
	if code != http.StatusOK || resp.Rejected != 1 || len(s.costEntries) != 0 {
		t.Fatalf("expected rejected line and nothing stored, got %d %+v", code, resp)
	}
}

func TestIngestProjectCosts_BatchLimits(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "ING-LIM")

	build := func(n int) []domain.CaptureProjectCostRequest {
		out := make([]domain.CaptureProjectCostRequest, n)
		for i := range out {
			out[i] = line(fmt.Sprintf("AP-%d", i), 1)
		}
		return out
	}
	if code, _ := ingest(t, s, &stubAuthZ{}, id, build(501)); code != http.StatusBadRequest {
		t.Fatalf("501 lines: expected 400, got %d", code)
	}
	if len(s.costEntries) != 0 {
		t.Fatalf("an oversized batch must store nothing")
	}
	if code, resp := ingest(t, s, &stubAuthZ{}, id, build(500)); code != http.StatusOK || resp.Captured != 500 {
		t.Fatalf("500 lines: expected 200/500 captured, got %d %+v", code, resp)
	}
	if code, _ := ingest(t, s, &stubAuthZ{}, id, []domain.CaptureProjectCostRequest{}); code != http.StatusBadRequest {
		t.Fatalf("empty batch: expected 400, got %d", code)
	}
}

func TestIngestProjectCosts_NoPrincipal_Returns401(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "ING-ID")
	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/costs/ingest", []domain.CaptureProjectCostRequest{line("AP-1", 1)}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}
