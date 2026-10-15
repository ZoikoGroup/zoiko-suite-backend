package clients_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/clients"
	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

var req = service.ProvenanceRequest{TenantID: "t", LegalEntityID: "e1", PeriodKey: "FY2026-P03",
	Command: domain.CmdHardClose, Acc14WorkflowRef: "WF-1", ControlSnapshotRef: "SNAP-1"}

const good = `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e1","command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"SNAP-1"}`

func code(t *testing.T, err error) domain.Code {
	t.Helper()
	require.Error(t, err)
	de, ok := domain.AsError(err)
	require.True(t, ok, "typed error expected, got %v", err)
	return de.Code
}

func serve(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestProvenance_ApprovedMatchingWorkflowPasses(t *testing.T) {
	var gotPath, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant = r.URL.Path, r.Header.Get("X-Tenant-Id")
		_, _ = w.Write([]byte(good))
	}))
	defer srv.Close()
	require.NoError(t, clients.NewHTTPProvenance(srv.URL).Verify(context.Background(), req))
	assert.Equal(t, "/v1/close/workflow-refs/WF-1", gotPath)
	assert.Equal(t, "t", gotTenant)
}

func TestProvenance_EverythingElseFailsClosed(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   domain.Code
	}{
		"endpoint not built yet (404)":    {404, `{"code":"NOT_FOUND"}`, domain.CodeSourceUnverified},
		"forbidden":                       {403, ``, domain.CodeSourceUnverified},
		"server error":                    {500, ``, domain.CodeDependencyUnavailable},
		"bad gateway":                     {502, ``, domain.CodeDependencyUnavailable},
		"throttled":                       {429, ``, domain.CodeDependencyUnavailable},
		"not JSON":                        {200, `<html>`, domain.CodeSourceUnverified},
		"pending approval":                {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e1","command":"HARD_CLOSE","status":"PENDING","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"rejected":                        {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e1","command":"HARD_CLOSE","status":"REJECTED","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"approved for another period":     {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P04","legal_entity_id":"e1","command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"approved for another entity":     {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e2","command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"approved for another command":    {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e1","command":"AUTHORIZE_REOPEN","status":"APPROVED","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"different control snapshot":      {200, `{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"e1","command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"OTHER"}`, domain.CodeSourceUnverified},
		"answer for another workflow ref": {200, `{"workflow_ref":"WF-2","period_key":"FY2026-P03","legal_entity_id":"e1","command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"SNAP-1"}`, domain.CodeSourceUnverified},
		"empty object":                    {200, `{}`, domain.CodeSourceUnverified},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serve(c.status, c.body)
			defer srv.Close()
			assert.Equal(t, c.want, code(t, clients.NewHTTPProvenance(srv.URL).Verify(context.Background(), req)))
		})
	}
}

func TestProvenance_UnreachableIsDependencyUnavailable(t *testing.T) {
	srv := serve(200, good)
	url := srv.URL
	srv.Close() // nothing listens any more
	assert.Equal(t, domain.CodeDependencyUnavailable, code(t, clients.NewHTTPProvenance(url).Verify(context.Background(), req)))
}

func TestCalendar_PreviewAndResolve(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/fiscal-calendar-versions/v1/periods-preview":
			_, _ = w.Write([]byte(`{"calendar_id":"c1","version_id":"v1","version_no":3,"legal_entity_id":"e1","fiscal_year":2026,
				"periods":[{"period_key":"FY2026-P01","period_no":1,"start_date":"2026-01-01","end_date":"2026-01-31","kind":"NORMAL"}]}`))
		case "/v1/fiscal-calendars:resolve":
			_, _ = w.Write([]byte(`{"calendar_id":"c1","version_id":"v1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := clients.NewHTTPCalendar(srv.URL)
	pv, err := c.PeriodsPreview(context.Background(), "t", "v1", 2026)
	require.NoError(t, err)
	assert.Equal(t, "c1", pv.CalendarID)
	require.Len(t, pv.Periods, 1)
	assert.Equal(t, "2026-01-31", pv.Periods[0].EndDate)
	ref, err := c.Resolve(context.Background(), "t", "e1", "BOOK-1", "2026-01-01")
	require.NoError(t, err)
	assert.Equal(t, "v1", ref.VersionID)
	assert.Equal(t, "/v1/fiscal-calendar-versions/v1/periods-preview?fiscal_year=2026", paths[0])
	assert.Contains(t, paths[1], "/v1/fiscal-calendars:resolve?")
	assert.Contains(t, paths[1], "legal_entity_id=e1")
	assert.Contains(t, paths[1], "scope=BOOK-1")
	assert.Contains(t, paths[1], "date=2026-01-01")

	_, err = c.PeriodsPreview(context.Background(), "t", "missing", 2026)
	assert.Equal(t, domain.CodeNotFound, code(t, err))
}

func TestCalendar_FailureModes(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   domain.Code
	}{
		"500":      {500, ``, domain.CodeDependencyUnavailable},
		"400":      {400, ``, domain.CodeSourceUnverified},
		"bad json": {200, `nope`, domain.CodeSourceUnverified},
	} {
		t.Run(name, func(t *testing.T) {
			srv := serve(c.status, c.body)
			defer srv.Close()
			_, err := clients.NewHTTPCalendar(srv.URL).PeriodsPreview(context.Background(), "t", "v1", 2026)
			assert.Equal(t, c.want, code(t, err))
		})
	}
	srv := serve(200, `{"calendar_id":"","version_id":""}`)
	defer srv.Close()
	_, err := clients.NewHTTPCalendar(srv.URL).Resolve(context.Background(), "t", "e", "", "2026-01-01")
	assert.Equal(t, domain.CodeSourceUnverified, code(t, err), "an empty resolve answer is not a calendar")

	dead := serve(200, `{}`)
	url := dead.URL
	dead.Close()
	_, err = clients.NewHTTPCalendar(url).Resolve(context.Background(), "t", "e", "", "2026-01-01")
	assert.Equal(t, domain.CodeDependencyUnavailable, code(t, err))
}
