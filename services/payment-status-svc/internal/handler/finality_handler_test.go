package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/handler"
)

func confirmRequest() domain.ConfirmSettlementRequest {
	return domain.ConfirmSettlementRequest{
		ProviderReference: "provider-settlement-1",
		BankReference:     "bank-settlement-1",
		Reason:            "bank settlement report reconciled",
		EffectiveDate:     "2026-10-25",
		EvidenceReference: "evidence://bank/settlement/2026-10-25/1",
		EvidenceHash:      strings.Repeat("a", 64),
	}
}

func rejectionRequest() domain.RecordRejectionRequest {
	return domain.RecordRejectionRequest{
		ProviderReference: "provider-rejection-1",
		BankReference:     "bank-rejection-1",
		Reason:            "provider rejected the instruction",
		EffectiveDate:     "2026-10-25T12:00:00Z",
		EvidenceReference: "evidence://provider/rejection/1",
		EvidenceHash:      strings.Repeat("b", 64),
	}
}

type finalityResponse struct {
	Payment  domain.PaymentExecutionState `json:"payment"`
	Evidence *domain.FinalityEvidence     `json:"evidence"`
	Replayed bool                         `json:"replayed"`
}

func decodeFinalityResponse(t *testing.T, w *httptest.ResponseRecorder) finalityResponse {
	t.Helper()
	var resp finalityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode finality response: %v", err)
	}
	return resp
}

func TestConfirmSettlement_RequiresIdempotencyKey(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequestAs(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without Idempotency-Key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmSettlement_RequiresEvidence(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)
	req := confirmRequest()
	req.EvidenceReference = ""

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", req, testTenant, "principal-reviewer", "idem-evidence-1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without evidence_reference, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmSettlement_Success(t *testing.T) {
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), az)
	p := recordPayment(t, r)
	az.actions = nil

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-confirm-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming settlement, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeFinalityResponse(t, w)
	if resp.Payment.Status != domain.StatusSettled {
		t.Fatalf("expected SETTLED, got %s", resp.Payment.Status)
	}
	if resp.Payment.FinalitySource != domain.FinalitySourceManualConfirmation {
		t.Fatalf("expected manual confirmation finality source, got %s", resp.Payment.FinalitySource)
	}
	if resp.Evidence == nil || resp.Evidence.EvidenceReference != confirmRequest().EvidenceReference {
		t.Fatalf("expected the command's evidence in the response, got %+v", resp.Evidence)
	}
	if resp.Replayed {
		t.Fatalf("first command must not be a replay")
	}
	if len(az.actions) != 1 || az.actions[0] != handler.PaymentFinalityConfirm {
		t.Fatalf("expected PAYMENT_FINALITY_CONFIRM, got %v", az.actions)
	}
}

func TestConfirmSettlement_DuplicateSameKey_Replays(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	p := recordPayment(t, r)

	first := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-confirm-2")
	if first.Code != http.StatusOK {
		t.Fatalf("first confirm: %d %s", first.Code, first.Body.String())
	}
	second := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-confirm-2")
	if second.Code != http.StatusOK {
		t.Fatalf("replay confirm: %d %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("expected Idempotent-Replay: true")
	}
	resp := decodeFinalityResponse(t, second)
	if !resp.Replayed || resp.Payment.Status != domain.StatusSettled {
		t.Fatalf("expected replay of the SETTLED result, got %+v", resp)
	}
	if len(st.events[p.PaymentID]) != 1 {
		t.Fatalf("a replay must not append a second status event, got %d", len(st.events[p.PaymentID]))
	}
	if len(st.finalityEvidence[p.PaymentID]) != 1 {
		t.Fatalf("a replay must not append a second evidence row, got %d", len(st.finalityEvidence[p.PaymentID]))
	}
}

func TestConfirmSettlement_AlreadyTerminalWithNewKey_Refused(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)

	if w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-terminal-1"); w.Code != http.StatusOK {
		t.Fatalf("first confirm: %d %s", w.Code, w.Body.String())
	}
	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-terminal-2")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a second finality decision with a new key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmSettlement_SameKeyDifferentRequest_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)

	if w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-conflict-1"); w.Code != http.StatusOK {
		t.Fatalf("first confirm: %d %s", w.Code, w.Body.String())
	}
	changed := confirmRequest()
	changed.Reason = "different reason with the same key"
	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", changed, testTenant, "principal-reviewer", "idem-conflict-1")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for key reuse with a different request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmSettlement_SamePrincipalAsCreator_Forbidden(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	p := recordPayment(t, r) // principal-operator

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-operator", "idem-sod-1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator confirms their own settlement, got %d: %s", w.Code, w.Body.String())
	}
	if st.payments[p.PaymentID].Status != domain.StatusPrepared {
		t.Fatalf("a forbidden command must not change status, got %s", st.payments[p.PaymentID].Status)
	}
}

func TestConfirmSettlement_DeniedPermission(t *testing.T) {
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), az)
	p := recordPayment(t, r)
	az.deny = true

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-denied-1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when PAYMENT_FINALITY_CONFIRM is denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmSettlement_OpenConflict_Refused(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	p := recordPayment(t, r)
	st.payments[p.PaymentID].HasOpenConflict = true

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-conflict-open-1")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while an evidence conflict is open, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordRejection_RequiresEvidence(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)
	req := rejectionRequest()
	req.EvidenceReference = ""

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/record-rejection", req, testTenant, "principal-reviewer", "idem-reject-evidence-1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without rejection evidence, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordRejection_Success(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/record-rejection", rejectionRequest(), testTenant, "principal-reviewer", "idem-reject-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 recording rejection, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeFinalityResponse(t, w)
	if resp.Payment.Status != domain.StatusRejected || resp.Payment.FinalitySource != domain.FinalitySourceManualRejection {
		t.Fatalf("expected REJECTED with manual rejection source, got %+v", resp.Payment)
	}
	if resp.Evidence == nil || resp.Evidence.Command != domain.CommandRecordRejection {
		t.Fatalf("expected rejection evidence, got %+v", resp.Evidence)
	}
	if len(st.events[p.PaymentID]) != 1 || st.events[p.PaymentID][0].EventType != domain.EventPaymentRejected {
		t.Fatalf("expected one PAYMENT_REJECTED status event, got %+v", st.events[p.PaymentID])
	}
}

func TestGetFinalityEvidence_ReturnsCommandEvidence(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	p := recordPayment(t, r)
	if w := doRequestWithIdempotency(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/confirm-settlement", confirmRequest(), testTenant, "principal-reviewer", "idem-evidence-read-1"); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}

	w := doRequestAs(r, http.MethodGet, "/bnk07/payments/"+p.PaymentID+"/finality-evidence", nil, testTenant, "principal-reader")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 reading evidence, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Evidence []domain.FinalityEvidence `json:"evidence"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if len(resp.Evidence) != 1 || resp.Evidence[0].Command != domain.CommandConfirmSettlement {
		t.Fatalf("expected one confirmation evidence row, got %+v", resp.Evidence)
	}
}
