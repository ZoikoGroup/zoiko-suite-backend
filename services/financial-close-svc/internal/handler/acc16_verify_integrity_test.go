package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// ACC-16 VerifySnapshotIntegrity: "Hash/signature mismatch fails
// verification."

func verifySnapshot(t *testing.T, r chi.Router, method, id string) (*http.Response, domain.SnapshotIntegrityResult) {
	t.Helper()
	rr := doReq(r, method, "/v1/financial-snapshots/"+id+"/verify", nil, "auditor-1")
	var res domain.SnapshotIntegrityResult
	if rr.Code == http.StatusOK {
		if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
			t.Fatalf("decode verify response: %v", err)
		}
	}
	return rr.Result(), res
}

func TestVerifySnapshotIntegrity_FreshlySealed_Verified(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		resp, res := verifySnapshot(t, r, method, id)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200 got %d", method, resp.StatusCode)
		}
		if !res.Verified || res.SnapshotID != id || res.CheckedAt.IsZero() {
			t.Errorf("%s: expected verified=true for %s with a checked_at, got %+v", method, id, res)
		}
	}
	if len(s.integrityFailures) != 0 {
		t.Errorf("a passing verification must not record an integrity failure, got %+v", s.integrityFailures)
	}
}

func TestVerifySnapshotIntegrity_TamperedContent_FailsAndRecordsEvent(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)
	s.snapshots[id].Content = `{"trial_balance":{"1000-Cash":9999999}}`

	resp, res := verifySnapshot(t, r, http.MethodGet, id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a mismatch is a report, expected 200 got %d", resp.StatusCode)
	}
	if res.Verified {
		t.Fatal("expected verified=false for tampered content")
	}
	if len(s.integrityFailures) != 1 {
		t.Fatalf("expected exactly one SnapshotIntegrityFailed event, got %d", len(s.integrityFailures))
	}
	f := s.integrityFailures[0]
	if f.SnapshotID != id || f.LegalEntityID != "le-1" || f.HashMatched || f.SignatureMatched {
		t.Errorf("unexpected event payload: %+v", f)
	}
	if f.RecomputedHash == f.StoredContentHash {
		t.Errorf("recomputed hash should differ from the stored one for tampered content")
	}
	// Verification is a read: the snapshot must stay SEALED.
	if got := s.snapshots[id].Status; got != domain.SnapshotStatusSealed {
		t.Errorf("failed verification must not change status, got %q", got)
	}
}

func TestVerifySnapshotIntegrity_TamperedSignatureOnly_Fails(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)
	forged := "00" + (*s.snapshots[id].Signature)[2:]
	s.snapshots[id].Signature = &forged

	_, res := verifySnapshot(t, r, http.MethodGet, id)
	if res.Verified {
		t.Fatal("expected verified=false for a forged signature over untouched content")
	}
	if len(s.integrityFailures) != 1 || !s.integrityFailures[0].HashMatched || s.integrityFailures[0].SignatureMatched {
		t.Errorf("expected one event with hash matched but signature not, got %+v", s.integrityFailures)
	}
}

func TestVerifySnapshotIntegrity_FailureNotRecorded_Returns503(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	id := createSealedSnapshot(t, r, false)
	s.snapshots[id].Content = "tampered"
	s.recordIntegrityFailureErr = errors.New("outbox unavailable")

	resp, _ := verifySnapshot(t, r, http.MethodGet, id)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the failure event cannot be recorded, got %d", resp.StatusCode)
	}
}

func TestVerifySnapshotIntegrity_NeverSealed_Returns422(t *testing.T) {
	s := newStubStore()
	s.snapshots["snap-draft"] = &domain.FinancialSnapshot{SnapshotID: "snap-draft", LegalEntityID: "le-1", Status: domain.SnapshotStatusDraft}
	r := newRouter(s, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodGet, "/v1/financial-snapshots/snap-draft/verify", nil, "auditor-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.ErrorCode != "snapshot_not_sealed" {
		t.Errorf("expected error_code snapshot_not_sealed, got %q", body.ErrorCode)
	}
}

func TestVerifySnapshotIntegrity_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/financial-snapshots/does-not-exist/verify", nil, "auditor-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestVerifySnapshotIntegrity_AuthzDenied_Returns403WithVerifyAction(t *testing.T) {
	s := newStubStore()
	s.snapshots["snap-1"] = &domain.FinancialSnapshot{SnapshotID: "snap-1", LegalEntityID: "le-1", Status: domain.SnapshotStatusSealed}
	var seenAction string
	authz := &recordingAuthZ{onCheck: func(_, _, action string) error {
		seenAction = action
		return domain.ErrAuthorizationDenied
	}}
	h := handler.New(s, authz, &stubClients{}, testSigningKey, zap.NewNop())
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, h)

	rr := doReq(rt, http.MethodGet, "/v1/financial-snapshots/snap-1/verify", nil, "auditor-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if seenAction != "SNAPSHOT_VERIFY" {
		t.Errorf("expected SNAPSHOT_VERIFY, got %q", seenAction)
	}
}
