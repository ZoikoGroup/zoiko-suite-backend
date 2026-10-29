package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/notification-svc/internal/domain"
)

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code. 412 and 428 are not produced here until If-Match lands (API-3).
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	send := map[string]any{
		"recipient_principal_id": "principal-2", "legal_entity_id": "le-us", "channel": "EMAIL",
		"subject": "Approval needed", "correlation_id": "corr-api2",
	}
	badChannel := map[string]any{
		"recipient_principal_id": "principal-2", "legal_entity_id": "le-us", "channel": "CARRIER_PIGEON",
		"subject": "Test", "correlation_id": "corr-api2-bad",
	}

	retired := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := createTestTemplate(t, retired, "owner-1")
	if rr := doReq(retired, http.MethodPost, "/v1/document-templates/"+templateID+"/retire", nil, "owner-1"); rr.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", rr.Code, rr.Body.String())
	}

	down := newStubStore()
	down.beginSubmissionErr = errors.New("db down")
	unavailable := sendBody("corr-api2-503", "", "emp-1")
	unavailable["channel"] = "EMAIL"

	for _, tc := range []struct {
		name   string
		rr     *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 unknown channel", doReq(newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}), http.MethodPost, "/v1/notifications/", badChannel, "principal-1"), 400, "unsupported_channel"},
		{"401 no verified caller", doReq(newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}), http.MethodPost, "/v1/notifications/", send, ""), 401, "identity_missing"},
		{"403 authorization denied", doReq(newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}), http.MethodPost, "/v1/notifications/", send, "principal-1"), 403, "forbidden"},
		{"404 unknown notification", doReq(newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}), http.MethodGet, "/v1/notifications/does-not-exist", nil, "principal-1"), 404, "notification_not_found"},
		{"409 version on a retired template", doReq(retired, http.MethodPost, "/v1/document-templates/"+templateID+"/versions", map[string]any{"locale": "en-US", "content": "hi"}, "owner-1"), 409, "template_retired"},
		{"503 submission mark failed", doReq(newRouterWith(down, &stubPublisher{}, &stubAuthZ{}, &stubDeliverer{delivered: true}, "tenant-abc"), http.MethodPost, "/v1/notifications/", unavailable, "p-1"), 503, "store_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rr.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.rr.Code, tc.status, tc.rr.Body.String())
			}
			if ct := tc.rr.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s", ct, apicontract.ContentTypeProblem)
			}
			var p map[string]any
			if err := json.Unmarshal(tc.rr.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", tc.rr.Body.String())
			}
			if p["code"] != tc.code || p["status"] != float64(tc.status) || !strings.HasPrefix(p["type"].(string), apicontract.ProblemBase) {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
		})
	}
}
