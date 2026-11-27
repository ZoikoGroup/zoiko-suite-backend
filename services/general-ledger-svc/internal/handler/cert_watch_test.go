package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certwatch"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// HTTP-layer tests for the certification watch: the permission and legal entity each route is authorized against, the strict
// inputs, and the status each store error maps to.

const (
	cwEntity = "aaaaaaaa-0000-0000-0000-00000000000a"
	cwFlag   = "bbbbbbbb-0000-0000-0000-00000000000b"
)

type fakeSweeper struct {
	calls     int
	principal string
	entity    string
	err       error
}

func (f *fakeSweeper) Sweep(_ context.Context, _, principal, _, entity string) (*certwatch.SweepResult, error) {
	f.calls++
	f.principal, f.entity = principal, entity
	return &certwatch.SweepResult{LegalEntityID: entity, Checked: 2, Withdrawn: 1, NewFlags: 3, Outcomes: []certwatch.Outcome{}}, f.err
}

type fakeFlagStore struct {
	certwatch.Store
	flag      *certwatch.Flag
	getErr    error
	ackErr    error
	listErr   error
	acks      int
	ackedBy   string
	listCalls int
	status    string
}

func (f *fakeFlagStore) GetFlag(context.Context, string, string) (*certwatch.Flag, error) {
	return f.flag, f.getErr
}

func (f *fakeFlagStore) Acknowledge(_ context.Context, _, id, by, text, remediation string) (*certwatch.Flag, error) {
	f.acks++
	f.ackedBy = by
	out := *f.flag
	out.Status, out.AcknowledgedBy, out.Acknowledgement, out.RemediationJournalID = certwatch.FlagAcknowledged, by, text, remediation
	return &out, f.ackErr
}

func (f *fakeFlagStore) ListFlags(_ context.Context, _, _, status string) ([]certwatch.Flag, error) {
	f.listCalls++
	f.status = status
	return []certwatch.Flag{{FlagID: cwFlag, Status: certwatch.FlagOpen}, {FlagID: "x", Status: certwatch.FlagAcknowledged}}, f.listErr
}

type cwRecordingAuthz struct {
	deny            string
	entities, acts []string
}

func (a *cwRecordingAuthz) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.entities, a.acts = append(a.entities, entity), append(a.acts, action)
	if action == a.deny {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func cwRouter(sw handler.CertificationSweeper, st certwatch.Store, az *cwRecordingAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(newStubStore(), &stubPublisher{}, az, &stubClose{}, zap.NewNop())
	if sw != nil {
		h = h.WithCertificationWatch(sw, st)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func cwFlagFixture() *certwatch.Flag {
	return &certwatch.Flag{FlagID: cwFlag, LegalEntityID: cwEntity, Status: certwatch.FlagOpen, SubjectType: "journal", SubjectID: "j1"}
}

func TestCertWatchAPI_EachRouteNeedsItsOwnPermissionOnTheRightEntity(t *testing.T) {
	for name, tc := range map[string]struct {
		method, path string
		body         any
		action       string
	}{
		"sweep":       {"POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": cwEntity}, "GL_CERTIFICATION_WATCH_SWEEP"},
		"view":        {"GET", "/v1/certification-flags?legal_entity_id=" + cwEntity, nil, "GL_CERTIFICATION_FLAG_VIEW"},
		"acknowledge": {"POST", "/v1/certification-flags/" + cwFlag + "/acknowledge", map[string]any{"acknowledgement": "reviewed"}, "GL_CERTIFICATION_FLAG_ACKNOWLEDGE"},
	} {
		sw, st := &fakeSweeper{}, &fakeFlagStore{flag: cwFlagFixture()}
		az := &cwRecordingAuthz{deny: tc.action}
		rec := doRequest(cwRouter(sw, st, az), tc.method, tc.path, tc.body, "principal-1")
		if rec.Code != http.StatusForbidden || sw.calls != 0 || st.acks != 0 || st.listCalls != 0 {
			t.Fatalf("%s without %s: %d (sweeps %d, acks %d, lists %d)", name, tc.action, rec.Code, sw.calls, st.acks, st.listCalls)
		}
		// Denying the OTHER actions blocks nothing here, and the check was made on the entity the request concerns.
		az2 := &cwRecordingAuthz{deny: "GL_JOURNAL_POST"}
		if rec := doRequest(cwRouter(&fakeSweeper{}, &fakeFlagStore{flag: cwFlagFixture()}, az2), tc.method, tc.path, tc.body, "principal-1"); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if len(az2.acts) != 1 || az2.acts[0] != tc.action || az2.entities[0] != cwEntity {
			t.Fatalf("%s authorized %v on %v, want %s on %s", name, az2.acts, az2.entities, tc.action, cwEntity)
		}
	}
}

func TestCertWatchAPI_SweepForwardsTheCallersIdentityAndReturnsTheAccount(t *testing.T) {
	sw := &fakeSweeper{}
	rec := doRequest(cwRouter(sw, &fakeFlagStore{}, &cwRecordingAuthz{}), "POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": cwEntity}, "alice")
	if rec.Code != http.StatusOK || sw.principal != "alice" || sw.entity != cwEntity ||
		!strings.Contains(rec.Body.String(), `"new_flags":3`) || !strings.Contains(rec.Body.String(), `"withdrawn":1`) {
		t.Fatalf("%d %s (principal %q entity %q)", rec.Code, rec.Body.String(), sw.principal, sw.entity)
	}
}

func TestCertWatchAPI_StrictInputsNeverReachTheStore(t *testing.T) {
	sw, st := &fakeSweeper{}, &fakeFlagStore{flag: cwFlagFixture()}
	r := cwRouter(sw, st, &cwRecordingAuthz{})
	for name, tc := range map[string]struct {
		method, path string
		body         any
	}{
		"sweep without an entity":     {"POST", "/v1/certification-watch/sweeps", map[string]any{}},
		"sweep entity not a uuid":     {"POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": "nope"}},
		"sweep unknown field":         {"POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": cwEntity, "everything": true}},
		"flags without an entity":     {"GET", "/v1/certification-flags", nil},
		"flags entity not a uuid":     {"GET", "/v1/certification-flags?legal_entity_id=x", nil},
		"flags unknown parameter":     {"GET", "/v1/certification-flags?legal_entity_id=" + cwEntity + "&owner=me", nil},
		"flags repeated parameter":    {"GET", "/v1/certification-flags?legal_entity_id=" + cwEntity + "&legal_entity_id=" + cwEntity, nil},
		"acknowledge unknown field":   {"POST", "/v1/certification-flags/" + cwFlag + "/acknowledge", map[string]any{"acknowledgement": "ok", "status": "RESOLVED"}},
		"acknowledge not json object": {"POST", "/v1/certification-flags/" + cwFlag + "/acknowledge", "just a string"},
	} {
		if rec := doRequest(r, tc.method, tc.path, tc.body, "principal-1"); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if sw.calls != 0 || st.acks != 0 || st.listCalls != 0 {
		t.Fatalf("a malformed request reached the store: sweeps %d, acks %d, lists %d", sw.calls, st.acks, st.listCalls)
	}
}

func TestCertWatchAPI_ListAcknowledgeAndErrorStatuses(t *testing.T) {
	st := &fakeFlagStore{flag: cwFlagFixture()}
	rec := doRequest(cwRouter(&fakeSweeper{}, st, &cwRecordingAuthz{}), "GET", "/v1/certification-flags?legal_entity_id="+cwEntity+"&status=OPEN", nil, "p")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":2`) || !strings.Contains(rec.Body.String(), `"open":1`) || st.status != "OPEN" {
		t.Fatalf("list: %d %s (status filter %q)", rec.Code, rec.Body.String(), st.status)
	}
	rec = doRequest(cwRouter(&fakeSweeper{}, st, &cwRecordingAuthz{}), "POST", "/v1/certification-flags/"+cwFlag+"/acknowledge",
		map[string]any{"acknowledgement": "reversed in March", "remediation_journal_id": "jrn-9"}, "controller-7")
	if rec.Code != http.StatusOK || st.ackedBy != "controller-7" || !strings.Contains(rec.Body.String(), `"status":"ACKNOWLEDGED"`) ||
		!strings.Contains(rec.Body.String(), `"remediation_journal_id":"jrn-9"`) {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	for name, tc := range map[string]struct {
		get, ack error
		want     int
	}{
		"unknown flag":         {certwatch.ErrFlagNotFound, nil, http.StatusNotFound},
		"already acknowledged": {nil, certwatch.ErrFlagAlreadyAcknowledged, http.StatusConflict},
		"invalid":              {nil, certwatch.ErrInvalid, http.StatusBadRequest},
		"store down":           {nil, context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		st := &fakeFlagStore{flag: cwFlagFixture(), getErr: tc.get, ackErr: tc.ack}
		if tc.get != nil {
			st.flag = nil
		}
		if rec := doRequest(cwRouter(&fakeSweeper{}, st, &cwRecordingAuthz{}), "POST", "/v1/certification-flags/"+cwFlag+"/acknowledge",
			map[string]any{"acknowledgement": "ok"}, "p"); rec.Code != tc.want {
			t.Fatalf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// Authorization is checked against the flagged entity BEFORE the flag's state is reported on: someone not permitted cannot tell
// whether a flag is already acknowledged.
func TestCertWatchAPI_AcknowledgeAuthorizesBeforeReportingTheFlagsState(t *testing.T) {
	st := &fakeFlagStore{flag: cwFlagFixture(), ackErr: certwatch.ErrFlagAlreadyAcknowledged}
	az := &cwRecordingAuthz{deny: "GL_CERTIFICATION_FLAG_ACKNOWLEDGE"}
	rec := doRequest(cwRouter(&fakeSweeper{}, st, az), "POST", "/v1/certification-flags/"+cwFlag+"/acknowledge", map[string]any{"acknowledgement": "ok"}, "p")
	if rec.Code != http.StatusForbidden || st.acks != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCertWatchAPI_UnwiredIsUnavailableNotSilent(t *testing.T) {
	r := cwRouter(nil, nil, &cwRecordingAuthz{})
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/certification-watch/sweeps", map[string]any{"legal_entity_id": cwEntity}},
		{"GET", "/v1/certification-flags?legal_entity_id=" + cwEntity, nil},
		{"POST", "/v1/certification-flags/" + cwFlag + "/acknowledge", map[string]any{"acknowledgement": "ok"}},
	} {
		if rec := doRequest(r, tc.method, tc.path, tc.body, "p"); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: %d", tc.method, tc.path, rec.Code)
		}
	}
}
