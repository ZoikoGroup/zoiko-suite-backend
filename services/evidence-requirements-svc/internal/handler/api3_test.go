package handler_test

// ER-4 (API-3): every mutation of an existing aggregate is conditional on the
// version the caller last read. Three cases per mutation: no If-Match is 428
// and the store is not touched; a stale one is 412 with the current version;
// the current one succeeds and answers the new ETag.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

const pbcRequestID = "dddddddd-0000-4000-8000-000000000001"

// stubPBC is a PBCStore whose request sits at version; a command with a
// non-zero expected version other than that is stale.
type stubPBC struct {
	version  int64
	expected []int64 // the expected version each command was called with
	calls    int
	listed   domain.ListEvidenceRequestsFilter
	requests []*domain.EvidenceRequest
	assignee string // the request's assigned_to_principal_id
	writes   int    // view, note and acknowledge calls that reached the store
}

func (s *stubPBC) request() *domain.EvidenceRequest {
	return &domain.EvidenceRequest{RequestID: pbcRequestID, TenantID: testTenant, LegalEntityID: testEntity,
		Status: domain.EvidenceRequestSent, RecordVersion: s.version, DueAt: time.Now().Add(time.Hour),
		AssignedToPrincipalID: s.assignee}
}

func (s *stubPBC) command(expected int64) (*domain.EvidenceRequest, error) {
	s.calls++
	s.expected = append(s.expected, expected)
	if expected != 0 && expected != s.version {
		return nil, &domain.StaleVersionError{Current: s.version}
	}
	s.version++
	return s.request(), nil
}

func (s *stubPBC) CreateEvidenceRequest(context.Context, string, domain.CreateEvidenceRequestParams) (*domain.EvidenceRequest, bool, error) {
	return s.request(), true, nil
}
func (s *stubPBC) GetEvidenceRequest(context.Context, string, string) (*domain.EvidenceRequest, error) {
	return s.request(), nil
}
func (s *stubPBC) GetResponseRequest(context.Context, string, string) (*domain.EvidenceRequest, error) {
	return s.request(), nil
}
func (s *stubPBC) ListEvidenceRequestsPage(_ context.Context, _ string, f domain.ListEvidenceRequestsFilter, offset, limit int) ([]*domain.EvidenceRequest, error) {
	s.listed = f
	return window(s.requests, offset, limit), nil
}
func (s *stubPBC) SendEvidenceRequest(_ context.Context, p domain.SendEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}
func (s *stubPBC) ReassignEvidenceRequest(_ context.Context, p domain.ReassignEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}
func (s *stubPBC) ExtendDueDate(_ context.Context, p domain.ExtendDueDateParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}
func (s *stubPBC) ViewEvidenceRequest(context.Context, domain.ViewEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	s.writes++
	s.version++
	return s.request(), nil
}
func (s *stubPBC) SubmitResponse(context.Context, domain.SubmitResponseParams) (*domain.EvidenceRequestResponse, bool, error) {
	s.version++
	return &domain.EvidenceRequestResponse{ResponseID: "resp-1", RequestID: pbcRequestID, Version: 1}, true, nil
}
func (s *stubPBC) RecordScanResult(context.Context, domain.ScanResultParams) (*domain.EvidenceRequestResponse, error) {
	return &domain.EvidenceRequestResponse{ResponseID: "resp-1", MalwareScanStatus: domain.MalwareScanClean}, nil
}
func (s *stubPBC) AcknowledgeReceipt(context.Context, domain.AcknowledgeReceiptParams) (*domain.EvidenceRequestReceipt, error) {
	s.writes++
	return &domain.EvidenceRequestReceipt{ReceiptID: "rcpt-1"}, nil
}
func (s *stubPBC) AddRequestNote(context.Context, domain.AddRequestNoteParams) (*domain.EvidenceRequestNote, error) {
	s.writes++
	return &domain.EvidenceRequestNote{NoteID: "note-1"}, nil
}
func (s *stubPBC) RequestClarification(_ context.Context, p domain.RequestClarificationParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}
func (s *stubPBC) MarkSatisfied(_ context.Context, p domain.MarkSatisfiedParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}
func (s *stubPBC) CloseEvidenceRequest(_ context.Context, p domain.CloseEvidenceRequestParams) (*domain.EvidenceRequest, error) {
	return s.command(p.ExpectedVersion)
}

func pbcRouter(pbc handler.PBCStore, az handler.AuthZClient) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(&stubStore{}, az, &stubDocs{}, zap.NewNop()).WithCursors(testCursors)
	handler.RegisterPBCRoutes(r, h, pbc)
	return r
}

type pbcCommand struct {
	path string
	body any
}

var pbcCommands = map[string]pbcCommand{
	"send":           {"/send", nil},
	"reassign":       {"/reassign", map[string]any{"new_assignee_principal_id": "someone-else"}},
	"extend":         {"/extend-due-date", map[string]any{"new_due_at": time.Now().Add(48 * time.Hour).UTC()}},
	"clarify":        {"/clarify", map[string]any{"reason": "the scan is unreadable"}},
	"mark-satisfied": {"/mark-satisfied", nil},
	"close":          {"/close", nil},
}

// checkPBCCommandAPI3 runs the three API-3 cases against one PBC command.
func checkPBCCommandAPI3(t *testing.T, name string) {
	t.Helper()
	cmd := pbcCommands[name]
	path := "/v1/pbc/requests/" + pbcRequestID + cmd.path
	headers := func(ifMatch string) map[string]string {
		h := map[string]string{"X-Correlation-ID": "corr-" + name}
		if ifMatch != "" {
			h["If-Match"] = ifMatch
		}
		return h
	}

	t.Run("missing If-Match is 428", func(t *testing.T) {
		st := &stubPBC{version: 3}
		rec := doWith(t, pbcRouter(st, &recordingAuthz{}), http.MethodPost, path, cmd.body, testTenant, testPrincipal, headers(""))
		assert.Equal(t, http.StatusPreconditionRequired, rec.Code, rec.Body.String())
		assert.Equal(t, "precondition_required", problemOf(t, rec)["code"])
		assert.Zero(t, st.calls, "the store must not be touched without a precondition")
	})
	t.Run("stale If-Match is 412", func(t *testing.T) {
		st := &stubPBC{version: 3}
		rec := doWith(t, pbcRouter(st, &recordingAuthz{}), http.MethodPost, path, cmd.body, testTenant, testPrincipal, headers(`"v2"`))
		assert.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
		p := problemOf(t, rec)
		assert.Equal(t, "precondition_failed", p["code"])
		assert.EqualValues(t, 3, p["current_version"])
		assert.Equal(t, `"v3"`, rec.Header().Get("ETag"))
		assert.Equal(t, []int64{2}, st.expected)
	})
	t.Run("current If-Match succeeds with the new ETag", func(t *testing.T) {
		st := &stubPBC{version: 3}
		rec := doWith(t, pbcRouter(st, &recordingAuthz{}), http.MethodPost, path, cmd.body, testTenant, testPrincipal, headers(`"v3"`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, `"v4"`, rec.Header().Get("ETag"))
		var got domain.EvidenceRequest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.EqualValues(t, 4, got.RecordVersion)
	})
}

func TestAPI3_PBCSendNeedsIfMatch(t *testing.T)          { checkPBCCommandAPI3(t, "send") }
func TestAPI3_PBCReassignNeedsIfMatch(t *testing.T)      { checkPBCCommandAPI3(t, "reassign") }
func TestAPI3_PBCExtendDueDateNeedsIfMatch(t *testing.T) { checkPBCCommandAPI3(t, "extend") }
func TestAPI3_PBCClarifyNeedsIfMatch(t *testing.T)       { checkPBCCommandAPI3(t, "clarify") }
func TestAPI3_PBCMarkSatisfiedNeedsIfMatch(t *testing.T) { checkPBCCommandAPI3(t, "mark-satisfied") }
func TestAPI3_PBCCloseStaleIs412(t *testing.T)           { checkPBCCommandAPI3(t, "close") }

func TestAPI3_EndDateNeedsIfMatch(t *testing.T) {
	existing := &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", TenantID: testTenant,
		EffectiveFrom: time.Now().Add(-time.Hour), RecordVersion: 3}
	path := "/v1/admin/evidence-requirements/req-1/end-date"
	body := domain.EndDateRequirementRequest{Reason: "superseded"}

	t.Run("missing If-Match is 428", func(t *testing.T) {
		st := &stubStore{getRequirement: existing, endDateVersion: -1}
		rec := do(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, path, body, testTenant, testPrincipal)
		assert.Equal(t, http.StatusPreconditionRequired, rec.Code, rec.Body.String())
		assert.EqualValues(t, -1, st.endDateVersion, "the store must not be touched without a precondition")
	})
	t.Run("malformed If-Match is 400", func(t *testing.T) {
		st := &stubStore{getRequirement: existing}
		rec := doWith(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, path, body, testTenant, testPrincipal,
			map[string]string{"If-Match": "yesterday"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
	t.Run("stale If-Match is 412", func(t *testing.T) {
		st := &stubStore{getRequirement: existing, endDateErr: &domain.StaleVersionError{Current: 3}}
		rec := doWith(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, path, body, testTenant, testPrincipal,
			map[string]string{"If-Match": `"v2"`})
		assert.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
		assert.Equal(t, "precondition_failed", problemOf(t, rec)["code"])
		assert.Equal(t, `"v3"`, rec.Header().Get("ETag"))
		assert.EqualValues(t, 2, st.endDateVersion)
	})
	t.Run("current If-Match succeeds with the new ETag", func(t *testing.T) {
		st := &stubStore{getRequirement: existing, endDated: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", RecordVersion: 4}}
		rec := doWith(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, path, body, testTenant, testPrincipal,
			map[string]string{"If-Match": `"v3"`})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, `"v4"`, rec.Header().Get("ETag"))
		assert.EqualValues(t, 3, st.endDateVersion)
	})
	t.Run("If-Match * is unconditional", func(t *testing.T) {
		st := &stubStore{getRequirement: existing, endDated: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", RecordVersion: 4}}
		rec := doWith(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, path, body, testTenant, testPrincipal,
			map[string]string{"If-Match": "*"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.EqualValues(t, 0, st.endDateVersion)
	})
}

// A read serves the ETag the next mutation must send.
func TestAPI3_ReadsServeTheETag(t *testing.T) {
	st := &stubStore{getRequirement: &domain.EvidenceRequirement{EvidenceRequirementID: "req-1", TenantID: testTenant, RecordVersion: 7}}
	rec := do(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodGet, "/v1/evidence-requirements/req-1", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"v7"`, rec.Header().Get("ETag"))

	pbc := &stubPBC{version: 5}
	rec = do(t, pbcRouter(pbc, &recordingAuthz{}), http.MethodGet, "/v1/pbc/requests/"+pbcRequestID, nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"v5"`, rec.Header().Get("ETag"))
}

// Submit, view, note, acknowledge and scan are the contributor's and the
// scanner's own actions on an open request, not edits of it: they take no
// If-Match. Submit and view move the request's status, so they bump its
// version and a manager holding the older ETag is refused.
func TestAPI3_AppendingActionsTakeNoIfMatch(t *testing.T) {
	// The caller is the assignee, so it may record the view (D-ER-29).
	pbc := &stubPBC{version: 1, assignee: testPrincipal}
	h := pbcRouter(pbc, &recordingAuthz{})
	for _, c := range []struct {
		path string
		body any
		want int
	}{
		{"/v1/pbc/requests/" + pbcRequestID + "/view", nil, http.StatusOK},
		{"/v1/pbc/requests/" + pbcRequestID + "/responses", map[string]any{"correlation_id": "resp-corr"}, http.StatusCreated},
		{"/v1/pbc/requests/" + pbcRequestID + "/notes", map[string]any{"body": "chasing", "visibility": "SHARED"}, http.StatusCreated},
		{"/v1/pbc/responses/resp-1/acknowledge", nil, http.StatusCreated},
		{"/v1/pbc/responses/resp-1/scan-result", map[string]any{"result": "CLEAN"}, http.StatusOK},
	} {
		rec := do(t, h, http.MethodPost, c.path, c.body, testTenant, testPrincipal)
		assert.Equal(t, c.want, rec.Code, "%s: %s", c.path, rec.Body.String())
	}
	assert.EqualValues(t, 3, pbc.version, "view and submit each bump the version")

	rec := doWith(t, h, http.MethodPost, "/v1/pbc/requests/"+pbcRequestID+"/close", nil, testTenant, testPrincipal,
		map[string]string{"If-Match": `"v1"`})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code, "a manager's ETag from before the response is stale")
}
