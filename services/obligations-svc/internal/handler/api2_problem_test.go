package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/obligations-svc/internal/domain"
)

// API-2 (ZS-API-001 §12): every refusal is application/problem+json with a
// stable code, across 400, 401, 403, 404, 409, 412, 428 and 503.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	id := existing().ObligationID
	path := "/v1/obligations/" + id
	ifMatch := map[string]string{"If-Match": `"3"`}

	anonymous := httptest.NewRecorder()
	completionRouter(&stubStore{findObligation: existing()}, &stubRegistry{}, &stubAuthz{}).
		ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, path, nil))

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 invalid status value", do(t, completionRouter(&stubStore{findObligation: existing()}, &stubRegistry{}, &stubAuthz{}),
			http.MethodPost, path+"/status", `{"obligation_status":"FOO"}`, nil), 400, "invalid_field"},
		{"401 no verified caller", anonymous, 401, "identity_missing"},
		{"403 authorization denied", do(t, completionRouter(&stubStore{findObligation: existing()}, &stubRegistry{}, &stubAuthz{err: domain.ErrAuthorizationDenied}),
			http.MethodGet, path, "", nil), 403, "forbidden"},
		{"404 unknown obligation", do(t, completionRouter(&stubStore{findErr: domain.ErrObligationNotFound}, &stubRegistry{}, &stubAuthz{}),
			http.MethodGet, path, "", nil), 404, "OBLIGATION_NOT_FOUND"},
		{"409 invalid transition", do(t, completionRouter(&stubStore{findObligation: existing(), updateErr: domain.ErrInvalidTransition}, &stubRegistry{}, &stubAuthz{}),
			http.MethodPost, path+"/status", `{"obligation_status":"CLOSED"}`, ifMatch), 409, "INVALID_TRANSITION"},
		{"412 stale If-Match", do(t, completionRouter(&stubStore{findObligation: existing(), updateErr: domain.ErrVersionMismatch}, &stubRegistry{}, &stubAuthz{}),
			http.MethodPatch, path, `{"severity_level":"LOW"}`, map[string]string{"If-Match": `"2"`}), 412, "VERSION_MISMATCH"},
		{"428 missing If-Match", do(t, completionRouter(&stubStore{findObligation: existing()}, &stubRegistry{}, &stubAuthz{}),
			http.MethodPatch, path, `{"severity_level":"LOW"}`, nil), 428, "precondition_required"},
		{"503 store unavailable", do(t, completionRouter(&stubStore{findErr: domain.ErrStoreUnavailable}, &stubRegistry{}, &stubAuthz{}),
			http.MethodGet, path, "", nil), 503, "STORE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.w.Code, tc.status, tc.w.Body.String())
			}
			p := problem(t, tc.w)
			if p["code"] != tc.code || p["status"] != float64(tc.status) {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
		})
	}
}
