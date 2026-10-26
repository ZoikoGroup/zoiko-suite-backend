package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// End to end through the REAL transfer client: a decision that needs a transfer
// evaluation must reach privacy-transfer-svc with the full mutation contract
// (tenant, principal, correlation id, idempotency key). The fake downstream
// enforces the same order of header checks as privacy-transfer-svc's pre-flight
// (tenant 401, principal 401, correlation 400, idempotency key 400) and answers
// 400 exactly as the real service did when this client omitted the last two.

type strictTransfer struct {
	mu       sync.Mutex
	calls    []http.Header
	bodies   []map[string]any
	status   int // 0 = enforce the contract and answer AUTHORIZED
	rejected int
}

func (s *strictTransfer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refuse := func(code int, msg string) {
		s.rejected++
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	h := r.Header
	switch {
	case strings.TrimSpace(h.Get("X-Tenant-Id")) == "":
		refuse(http.StatusUnauthorized, "X-Tenant-Id header is required")
		return
	case strings.TrimSpace(h.Get("X-Principal-Id")) == "":
		refuse(http.StatusUnauthorized, "X-Principal-Id header is required")
		return
	case strings.TrimSpace(h.Get("X-Correlation-ID")) == "":
		refuse(http.StatusBadRequest, "X-Correlation-ID header is required")
		return
	case strings.TrimSpace(h.Get("Idempotency-Key")) == "":
		refuse(http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	s.calls = append(s.calls, h.Clone())
	s.bodies = append(s.bodies, b)
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(`{"error":"downstream failure"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"decision_id": "td-real-1", "result": "AUTHORIZED", "reason_codes": []string{},
		"transfer_mechanism_id": b["transfer_mechanism_id"], "relationship_id": b["relationship_id"],
	})
}

func exportRequest() domain.EvaluateDecisionRequest {
	return domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport, SubjectContext: validSubjectContext(),
		TransferCheck: &domain.TransferCheckRequest{
			RelationshipID: "rel-001", TransferMechanismID: "mech-001", DestinationJurisdiction: "US", AssessmentCheck: true,
		},
	}
}

func newRealTransferRouter(st *stubStore, downstream *strictTransfer) (http.Handler, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(downstream.handler))
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()
	h := handler.New(st, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferregistry.NewClient(srv.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r, srv
}

func TestTransferCall_CarriesTheFullMutationContract(t *testing.T) {
	down := &strictTransfer{}
	r, srv := newRealTransferRouter(newStubStore(), down)
	defer srv.Close()

	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", exportRequest(), testTenant, map[string]string{
		"X-Correlation-ID": "corr-xyz", "Idempotency-Key": "decision-key-1", "X-Principal-Id": "principal-42",
	})
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != http.StatusOK || d.Result != domain.ResultPermit {
		t.Fatalf("a transfer-dependent decision must succeed once the contract is met: %d %s %v", w.Code, d.Result, d.ReasonCodes)
	}
	if down.rejected != 0 || len(down.calls) != 1 {
		t.Fatalf("transfer must accept the call (rejected=%d, accepted=%d)", down.rejected, len(down.calls))
	}
	h := down.calls[0]
	for header, want := range map[string]string{
		"X-Tenant-Id": testTenant, "X-Principal-Id": "principal-42",
		"X-Correlation-Id": "corr-xyz", "Idempotency-Key": "decision-key-1:transfer",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if down.bodies[0]["relationship_id"] != "rel-001" || down.bodies[0]["transfer_mechanism_id"] != "mech-001" ||
		down.bodies[0]["destination_jurisdiction"] != "US" || down.bodies[0]["assessment_required"] != true {
		t.Errorf("transfer body = %v", down.bodies[0])
	}
	if d.TransferDecisionID == nil || *d.TransferDecisionID != "td-real-1" {
		t.Errorf("transfer_decision_id not recorded: %v", d.TransferDecisionID)
	}
}

// A retry of the same decision request — here after the first attempt's commit
// failed, so the decision is re-evaluated — repeats the SAME transfer
// idempotency key (so privacy-transfer-svc replays instead of recording twice),
// and still ends in exactly one stored decision.
func TestTransferCall_RetryReusesTheTransferIdempotencyKey(t *testing.T) {
	down := &strictTransfer{}
	st := newStubStore()
	r, srv := newRealTransferRouter(st, down)
	defer srv.Close()
	hdr := map[string]string{"Idempotency-Key": "retry-key", "X-Correlation-ID": "corr-1"}

	st.failCommit = true
	if w := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", exportRequest(), testTenant, hdr); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first attempt: want 503, got %d", w.Code)
	}
	st.failCommit = false
	hdr["X-Correlation-ID"] = "corr-2" // the retry carries its own correlation id
	if w := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", exportRequest(), testTenant, hdr); w.Code != http.StatusOK {
		t.Fatalf("retry: want 200, got %d", w.Code)
	}
	if len(down.calls) != 2 {
		t.Fatalf("both attempts evaluate the transfer, got %d calls", len(down.calls))
	}
	if a, b := down.calls[0].Get("Idempotency-Key"), down.calls[1].Get("Idempotency-Key"); a != b || a != "retry-key:transfer" {
		t.Fatalf("a retry must reuse the transfer key, got %q and %q", a, b)
	}
	if len(st.decisions) != 1 {
		t.Fatalf("exactly one decision must be stored, got %d", len(st.decisions))
	}
}

// A failed downstream call never turns into a successful transfer-dependent
// decision: it fails closed to INDETERMINATE, whatever the failure.
func TestTransferCall_DownstreamFailureFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		down := &strictTransfer{status: status}
		r, srv := newRealTransferRouter(newStubStore(), down)
		w := doRequest(r, http.MethodPost, "/privacy/decisions", exportRequest(), testTenant)
		srv.Close()
		var d domain.PrivacyDecision
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		if d.Result != domain.ResultIndeterminate || d.TransferDecisionID != nil {
			t.Errorf("downstream %d: want INDETERMINATE with no transfer decision, got %s %v", status, d.Result, d.TransferDecisionID)
		}
	}
	// Downstream unreachable.
	down := &strictTransfer{}
	r, srv := newRealTransferRouter(newStubStore(), down)
	srv.Close()
	w := doRequest(r, http.MethodPost, "/privacy/decisions", exportRequest(), testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultIndeterminate {
		t.Errorf("unreachable downstream: want INDETERMINATE, got %s", d.Result)
	}
}

// The fake enforces the contract the way the real service does: a client that
// omitted the correlation id and the key (the pre-fix behaviour) is refused 400.
func TestTransferCall_FakeDownstreamRefusesTheOldClientBehaviour(t *testing.T) {
	down := &strictTransfer{}
	srv := httptest.NewServer(http.HandlerFunc(down.handler))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/privacy/transfer-decisions", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || down.rejected != 1 {
		t.Fatalf("the old header set must be refused with 400, got %d", resp.StatusCode)
	}
}
