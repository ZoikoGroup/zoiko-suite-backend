package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/financial-close-svc/internal/domain"
)

// recognizeOnePeriod runs a real RunAccrualRecognition call and returns the
// resulting RecognitionInstance, so a reversal test starts from an actual
// recognized period rather than a hand-built fixture.
func recognizeOnePeriod(t *testing.T, r chi.Router, id, fiscalPeriod string) domain.RecognitionInstance {
	t.Helper()
	req := domain.RunAccrualRecognitionRequest{FiscalPeriod: fiscalPeriod}
	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", req, "preparer-1")
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("recognize %s failed: %d %s", fiscalPeriod, rr.Code, rr.Body.String())
	}
	var inst domain.RecognitionInstance
	if err := json.NewDecoder(rr.Body).Decode(&inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestReverseAccrualRecognition_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	recognizeOnePeriod(t, r, id, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{}, "reverser-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReverseAccrualRecognition_UnrecognizedPeriod_Returns404(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	// Never called /recognize for 2026-01.

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "correction"}, "reverser-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.reverseJournalCalls != 0 {
		t.Fatalf("expected no GL reversal call for a period never recognized, got %d", cl.reverseJournalCalls)
	}
}

func TestReverseAccrualRecognition_HappyPath_CallsRealGLReversal(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{reversingJournalID: "reversing-journal-1"}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	inst := recognizeOnePeriod(t, r, id, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "estimate corrected"}, "reverser-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var rev domain.RecognitionReversal
	if err := json.NewDecoder(rr.Body).Decode(&rev); err != nil {
		t.Fatal(err)
	}
	if rev.ReversingJournalID != "reversing-journal-1" {
		t.Fatalf("expected the real reversing journal id, got %q", rev.ReversingJournalID)
	}
	if rev.RecognitionInstanceID != inst.RecognitionInstanceID {
		t.Fatalf("expected the reversal to reference the recognized instance, got %q want %q", rev.RecognitionInstanceID, inst.RecognitionInstanceID)
	}
	if cl.reverseJournalCalls != 1 {
		t.Fatalf("expected exactly 1 GL reversal call, got %d", cl.reverseJournalCalls)
	}
}

// TestReverseAccrualRecognition_Replay_IsIdempotentNoDuplicateReversal is
// the spec's own negative path, "Auto-reversal duplicates": a replayed or
// retried reverse call must return the SAME reversal, never issue a
// second GL reversal call.
func TestReverseAccrualRecognition_Replay_IsIdempotentNoDuplicateReversal(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{reversingJournalID: "reversing-journal-1"}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900.00, 3, "2026-01")
	recognizeOnePeriod(t, r, id, "2026-01")

	first := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "estimate corrected"}, "reverser-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first reverse failed: %d %s", first.Code, first.Body.String())
	}
	var firstRev domain.RecognitionReversal
	_ = json.NewDecoder(first.Body).Decode(&firstRev)

	second := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "estimate corrected"}, "reverser-1")
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d: %s", second.Code, second.Body.String())
	}
	var secondRev domain.RecognitionReversal
	_ = json.NewDecoder(second.Body).Decode(&secondRev)

	if secondRev.RecognitionReversalID != firstRev.RecognitionReversalID {
		t.Fatalf("expected the SAME reversal on replay, got a different id: %q vs %q", secondRev.RecognitionReversalID, firstRev.RecognitionReversalID)
	}
	if cl.reverseJournalCalls != 1 {
		t.Fatalf("expected exactly 1 GL reversal call across both requests (no duplicate accounting consequence), got %d", cl.reverseJournalCalls)
	}
}

func TestReverseAccrualRecognition_AuthorizationDenied_NeverCallsGL(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	// Build the schedule and recognize a period through an ALLOWING
	// router, sharing the same store — the denial under test is specific
	// to the reverse call itself, not to every prior setup step.
	setupRouter := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, setupRouter, 900.00, 3, "2026-01")
	recognizeOnePeriod(t, setupRouter, id, "2026-01")

	denyingRouter := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, cl)
	rr := doReq(denyingRouter, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "x"}, "reverser-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.reverseJournalCalls != 0 {
		t.Fatalf("expected no GL call when authorization is denied, got %d", cl.reverseJournalCalls)
	}
}
