package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

func TestListAttempts_AuthorizedLikeTheRecord(t *testing.T) {
	s := newStubStore()
	s.byID["n-1"] = &domain.Notification{NotificationID: "n-1", TenantID: "tenant-abc", LegalEntityID: "le-us"}
	s.attempts = map[string][]domain.DeliveryAttempt{
		"n-1": {
			{AttemptNumber: 1, Outcome: domain.AttemptOutcomeRetrying, FailureReason: "421"},
			{AttemptNumber: 2, Outcome: domain.AttemptOutcomeAccepted, ProviderResponse: "250"},
		},
	}

	if rr := doReq(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, "/v1/notifications/missing/attempts", nil, "p-1"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %d", rr.Code)
	}

	denied := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	if rr := doReq(newRouter(s, &stubPublisher{}, denied), http.MethodGet, "/v1/notifications/n-1/attempts", nil, "p-1"); rr.Code != http.StatusForbidden {
		t.Fatalf("no NOTIFICATION_VIEW: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(denied.calls) != 1 || denied.calls[0] != "NOTIFICATION_VIEW" {
		t.Fatalf("authz calls = %v, want [NOTIFICATION_VIEW]", denied.calls)
	}

	rr := doReq(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, "/v1/notifications/n-1/attempts", nil, "p-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Attempts []domain.DeliveryAttempt `json:"attempts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Attempts) != 2 || body.Attempts[0].Outcome != domain.AttemptOutcomeRetrying {
		t.Fatalf("attempts = %+v", body.Attempts)
	}
}
