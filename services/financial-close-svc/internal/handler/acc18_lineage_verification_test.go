package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

// ── GetLineageAsOf ────────────────────────────────────────────────────────────

func TestGetLineageAsOf_MissingAsOf_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/lineage/journals/journal-1/source/as-of", nil, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetLineageAsOf_ExcludesEdgesRecordedAfterWatermark(t *testing.T) {
	s := newStubStore()
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s.lineageEdges = []domain.LineageEdge{
		{FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1", RecordedAt: early},
		{FromType: "allocation_run", FromID: "run-2", ToType: "journal", ToID: "journal-1", RecordedAt: late},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	asOf := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	rr := doReq(r, http.MethodGet, "/v1/lineage/journals/journal-1/source/as-of?as_of="+asOf, nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var edges []domain.LineageEdge
	_ = json.NewDecoder(rr.Body).Decode(&edges)
	if len(edges) != 1 || edges[0].FromID != "run-1" {
		t.Fatalf("expected only the edge recorded before the watermark, got %+v", edges)
	}
}

// ── VerifyTracePath ──────────────────────────────────────────────────────────

func TestVerifyTracePath_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/lineage/verify-path", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestVerifyTracePath_EdgeExists_ReturnsVerifiedTrueAndPersists(t *testing.T) {
	s := newStubStore()
	s.lineageEdges = []domain.LineageEdge{
		{FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1"},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	req := domain.VerifyTracePathRequest{LegalEntityID: "le-1", FromType: "allocation_run", FromID: "run-1", ToID: "journal-1"}
	rr := doReq(r, http.MethodPost, "/v1/lineage/verify-path", req, "auditor-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var result domain.TracePathVerification
	_ = json.NewDecoder(rr.Body).Decode(&result)
	if !result.Verified {
		t.Fatal("expected Verified=true")
	}
	if len(s.tracePathVerifications) != 1 {
		t.Fatalf("expected the verification result to be persisted, got %d rows", len(s.tracePathVerifications))
	}
}

func TestVerifyTracePath_EdgeMissing_ReturnsVerifiedFalse(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	req := domain.VerifyTracePathRequest{LegalEntityID: "le-1", FromType: "allocation_run", FromID: "run-1", ToID: "journal-1"}
	rr := doReq(r, http.MethodPost, "/v1/lineage/verify-path", req, "auditor-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (a completed check, not an error), got %d: %s", rr.Code, rr.Body.String())
	}
	var result domain.TracePathVerification
	_ = json.NewDecoder(rr.Body).Decode(&result)
	if result.Verified {
		t.Fatal("expected Verified=false for a missing edge")
	}
	if len(s.tracePathVerifications) != 1 || s.tracePathVerifications[0].Verified {
		t.Fatalf("expected the FAILED verification to still be persisted as evidence, got %+v", s.tracePathVerifications)
	}
}

// ── QuarantineBrokenLineage ──────────────────────────────────────────────────

func TestQuarantineBrokenLineage_MissingReason_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	req := domain.QuarantineBrokenLineageRequest{LegalEntityID: "le-1", FromType: "allocation_run", FromID: "run-1", ToID: "journal-1"}
	rr := doReq(r, http.MethodPost, "/v1/lineage/quarantine", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestQuarantineBrokenLineage_RemovesGapFromCompletenessReport is the real
// point of this command: a gap that's been deliberately accepted as known
// must stop appearing in VerifyLineageCompleteness's own Gaps list, while
// still being visibly counted (QuarantinedCount) — never a silent drop.
func TestQuarantineBrokenLineage_RemovesGapFromCompletenessReport(t *testing.T) {
	s := newStubStore()
	s.postedJournalRefs = []domain.PostedJournalRef{
		{FromType: "allocation_run", FromID: "run-1", JournalID: "journal-1"},
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	// Before quarantine: a real gap.
	before := doReq(r, http.MethodGet, "/v1/lineage/verify?legal_entity_id=le-1", nil, "preparer-1")
	var beforeReport domain.LineageCompletenessReport
	_ = json.NewDecoder(before.Body).Decode(&beforeReport)
	if beforeReport.Complete || len(beforeReport.Gaps) != 1 {
		t.Fatalf("expected 1 gap before quarantine, got %+v", beforeReport)
	}

	quarantineReq := domain.QuarantineBrokenLineageRequest{
		LegalEntityID: "le-1", FromType: "allocation_run", FromID: "run-1", ToID: "journal-1",
		Reason: "predates lineage recording — accepted as known",
	}
	qrr := doReq(r, http.MethodPost, "/v1/lineage/quarantine", quarantineReq, "preparer-1")
	if qrr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", qrr.Code, qrr.Body.String())
	}

	after := doReq(r, http.MethodGet, "/v1/lineage/verify?legal_entity_id=le-1", nil, "preparer-1")
	var afterReport domain.LineageCompletenessReport
	_ = json.NewDecoder(after.Body).Decode(&afterReport)
	if !afterReport.Complete {
		t.Fatalf("expected Complete=true after quarantine, got %+v", afterReport)
	}
	if len(afterReport.Gaps) != 0 {
		t.Fatalf("expected 0 gaps after quarantine, got %+v", afterReport.Gaps)
	}
	if afterReport.QuarantinedCount != 1 {
		t.Fatalf("expected QuarantinedCount=1 (never silently dropped), got %d", afterReport.QuarantinedCount)
	}
}
