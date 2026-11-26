package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/handler"
	"zoiko.io/data-ingestion-svc/internal/store"
)

// HTTP-layer tests for governed release of run-level quarantine: each route's permission, the strict request, and the
// status each store error maps to.

type runReleaseStore struct {
	store.Store
	calls   int
	err     error
	release bool
}

func (s *runReleaseStore) RequestRunRelease(_ context.Context, _, _, _, _ string, _ domain.RunReleaseRequest, _ domain.IdempotencyClaim) error {
	s.calls++
	return s.err
}

func (s *runReleaseStore) DecideRunRelease(_ context.Context, _, _, _, _ string, release bool, _ domain.RunReleaseDecision, _ domain.IdempotencyClaim) error {
	s.calls++
	s.release = release
	return s.err
}

func (s *runReleaseStore) RunQuarantineLedger(_ context.Context, _, _, _ string) ([]domain.RunReleaseEntry, domain.RunReleaseState, error) {
	s.calls++
	return []domain.RunReleaseEntry{{LedgerID: "l1", Kind: domain.RunReleaseRequested}}, domain.RunReleasePending, s.err
}

// denyAction answers GRANTED unless the call is for the named action.
func denyAction(t *testing.T, action string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		outcome := "GRANTED"
		if action != "" && strings.Contains(string(b), action) {
			outcome = "DENIED"
		}
		_, _ = w.Write([]byte(`{"decision_outcome":"` + outcome + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doRelease(t *testing.T, st store.Store, az *httptest.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := handler.NewRouter(handler.NewHandler(st, authz.NewClient(az.URL, zap.NewNop()), zap.NewNop()))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range map[string]string{
		"X-Tenant-Id": "11111111-1111-1111-1111-111111111111", "X-Principal-Id": "tester", "X-Correlation-ID": "c", "X-Purpose-Context": "audit",
		"X-Request-Id": "r", "X-Source-Channel": "api", "X-Timezone": "UTC", "X-Jurisdiction-Context": "GB",
		"Content-Type": "application/json", "Idempotency-Key": "k",
	} {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	rrBase    = "/v1/data-ingestion/runs/ing_x/quarantine-items/dqi_x"
	rrReqBody = `{"reason":"producer fixed it","correction_ref":"ticket-1"}`
	rrDecBody = `{"reason":"verified"}`
)

func TestRunReleaseAPI_EachRouteNeedsItsPermission(t *testing.T) {
	for _, rt := range []struct{ method, path, body, action string }{
		{"POST", rrBase + ":request-release", rrReqBody, "DATA_INGESTION_MANAGE"},
		{"POST", rrBase + ":approve-release", rrDecBody, "DATA_IMPORT_QUARANTINE_RELEASE"},
		{"POST", rrBase + ":refuse-release", rrDecBody, "DATA_IMPORT_QUARANTINE_RELEASE"},
		{"GET", rrBase + "/release", ``, "DATA_INGESTION_READ"},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			st := &runReleaseStore{}
			if rec := doRelease(t, st, denyAction(t, rt.action), rt.method, rt.path, rt.body); rec.Code != 403 || st.calls != 0 {
				t.Fatalf("without %s: %d, store calls %d", rt.action, rec.Code, st.calls)
			}
			// Asking is not deciding: denying the OTHER permission blocks nothing here.
			other := "DATA_IMPORT_QUARANTINE_RELEASE"
			if rt.action == other {
				other = "DATA_INGESTION_MANAGE"
			}
			if rec := doRelease(t, &runReleaseStore{}, denyAction(t, other), rt.method, rt.path, rt.body); rec.Code >= 400 {
				t.Fatalf("denying %s blocked a route that needs %s: %d %s", other, rt.action, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRunReleaseAPI_StrictBodiesAndDecisions(t *testing.T) {
	st := &runReleaseStore{}
	az := denyAction(t, "")
	for name, tc := range map[string]struct{ path, body string }{
		"request empty":         {rrBase + ":request-release", ``},
		"request unknown field": {rrBase + ":request-release", `{"reason":"x","correction_ref":"y","force":true}`},
		"decision empty":        {rrBase + ":approve-release", ``},
		"decision unknown":      {rrBase + ":refuse-release", `{"reason":"x","override":1}`},
		"trailing data":         {rrBase + ":approve-release", rrDecBody + `{}`},
	} {
		if rec := doRelease(t, st, az, "POST", tc.path, tc.body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed request reached the store %d times", st.calls)
	}
	if rec := doRelease(t, st, az, "POST", rrBase+":request-release", rrReqBody); rec.Code != 202 || !strings.Contains(rec.Body.String(), "RELEASE_PENDING") {
		t.Fatalf("a request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRelease(t, st, az, "POST", rrBase+":approve-release", rrDecBody); rec.Code != 200 || !st.release || !strings.Contains(rec.Body.String(), "RELEASE_APPROVED") {
		t.Fatalf("an approval: %d %s release=%v", rec.Code, rec.Body.String(), st.release)
	}
	if rec := doRelease(t, st, az, "POST", rrBase+":refuse-release", rrDecBody); rec.Code != 200 || st.release || !strings.Contains(rec.Body.String(), "QUARANTINED") {
		t.Fatalf("a refusal: %d %s release=%v", rec.Code, rec.Body.String(), st.release)
	}
}

func TestRunReleaseAPI_ErrorStatuses(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"invalid":          {domain.ErrRunReleaseInvalid, 400},
		"not independent":  {domain.ErrRunReleaseNotIndependent, 403},
		"already open":     {domain.ErrRunReleaseOpen, 409},
		"no open request":  {domain.ErrRunNoOpenRelease, 409},
		"already released": {domain.ErrRunReleaseAlreadyReleased, 409},
		"release required": {domain.ErrRunReleaseRequired, 409},
		"not approved":     {domain.ErrRunReleaseNotApproved, 409},
		"not a correction": {domain.ErrRunReleaseNotACorrection, 409},
		"unknown item":     {domain.ErrQuarantineItemNotFound, 404},
	} {
		rec := doRelease(t, &runReleaseStore{err: tc.err}, denyAction(t, ""), "POST", rrBase+":approve-release", rrDecBody)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
}
