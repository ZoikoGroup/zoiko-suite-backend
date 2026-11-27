package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// HTTP-layer tests for reliance on a certification and its blast radius: authorization against the run's own legal
// entity with the right action, the strict body, 201 versus 200, and the status each store error maps to.

type relianceStore struct {
	*fakeStore
	calls   int
	created bool
	err     error
	actor   string
}

func (s *relianceStore) RecordReliance(_ context.Context, _, runID, actor string, req domain.RecordRelianceRequest) (*domain.CertificationReliance, bool, error) {
	s.calls++
	s.actor = actor
	return &domain.CertificationReliance{RelianceID: "rrl_x", RunID: runID, RelyingService: req.RelyingService}, s.created, s.err
}

func (s *relianceStore) GetBlastRadius(_ context.Context, _, runID string) (*domain.BlastRadius, error) {
	s.calls++
	return &domain.BlastRadius{RunID: runID, CertificationState: domain.CertRevoked, Reliances: []domain.CertificationReliance{}, ByService: map[string]int{}}, s.err
}

const (
	relyPath   = "/v1/reconciliation/runs/rcr_1/certification/reliances"
	radiusPath = "/v1/reconciliation/runs/rcr_1/certification/blast-radius"
	relyBody   = `{"relying_service":"gl-svc","relying_object_type":"journal-batch","relying_object_id":"jb_1","purpose":"posted on the certified run"}`
)

func relianceServer(st *relianceStore, az *recordingAuthz) *Handler {
	st.fakeStore = &fakeStore{run: &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}}
	return NewHandler(st, az, failingFetcher{}, zap.NewNop())
}

func TestRelianceAPI_EachRouteNeedsItsOwnPermissionOnTheRunsEntity(t *testing.T) {
	for _, rt := range []struct{ method, path, body, action string }{
		{"POST", relyPath, relyBody, ActionRelianceRecord},
		{"GET", radiusPath, ``, ActionReconciliationRead},
	} {
		st := &relianceStore{}
		az := &recordingAuthz{deny: map[string]bool{rt.action: true}}
		if w := serve(relianceServer(st, az), rt.method, rt.path, rt.body); w.Code != http.StatusForbidden || st.calls != 0 {
			t.Fatalf("%s without %s: %d, store calls %d", rt.path, rt.action, w.Code, st.calls)
		}
		// Recording reliance is not the power to revoke, certify or manage runs.
		for _, other := range []string{ActionReconciliationRevoke, ActionReconciliationCertify, ActionReconciliationManage} {
			st := &relianceStore{}
			az := &recordingAuthz{deny: map[string]bool{other: true}}
			if w := serve(relianceServer(st, az), rt.method, rt.path, rt.body); w.Code >= 400 {
				t.Fatalf("denying %s blocked %s: %d %s", other, rt.path, w.Code, w.Body)
			}
			if len(az.entities) != 1 || az.entities[0] != entityA || az.actions[0] != rt.action {
				t.Fatalf("%s authorized %v for %v, want %s on the run's entity", rt.path, az.actions, az.entities, rt.action)
			}
		}
	}
}

func TestRelianceAPI_CreatedVersusExistingAndTheRecordingPrincipal(t *testing.T) {
	st := &relianceStore{created: true}
	w := serve(relianceServer(st, &recordingAuthz{}), "POST", relyPath, relyBody)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"relying_service":"gl-svc"`) || st.actor != "alice" {
		t.Fatalf("new reliance: %d %s (actor %q)", w.Code, w.Body, st.actor)
	}
	st = &relianceStore{created: false}
	if w := serve(relianceServer(st, &recordingAuthz{}), "POST", relyPath, relyBody); w.Code != http.StatusOK {
		t.Fatalf("existing reliance: %d %s", w.Code, w.Body)
	}
}

func TestRelianceAPI_StrictBodiesAndErrorStatuses(t *testing.T) {
	st := &relianceStore{}
	h := relianceServer(st, &recordingAuthz{})
	for name, body := range map[string]string{
		"empty":         ``,
		"unknown field": strings.Replace(relyBody, `"purpose"`, `"recorded_by":"someone-else","purpose"`, 1),
	} {
		if w := serve(h, "POST", relyPath, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed body reached the store %d times", st.calls)
	}
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"inactive": {domain.ErrRelianceInactive, http.StatusConflict},
		"invalid":  {domain.ErrRelianceInvalid, http.StatusBadRequest},
		"no run":   {domain.ErrRunNotFound, http.StatusNotFound},
	} {
		st := &relianceStore{err: tc.err}
		if w := serve(relianceServer(st, &recordingAuthz{}), "POST", relyPath, relyBody); w.Code != tc.code {
			t.Fatalf("%s: %d %s, want %d", name, w.Code, w.Body, tc.code)
		}
	}
	w := serve(relianceServer(&relianceStore{}, &recordingAuthz{}), "GET", radiusPath, ``)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reliances":[]`) || !strings.Contains(w.Body.String(), `"certification_state":"REVOKED"`) {
		t.Fatalf("blast radius: %d %s", w.Code, w.Body)
	}
}

const standingQuery = "?legal_entity_id=" + entityA + "&relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=jb_1"

func (s *relianceStore) GetRelianceStanding(_ context.Context, _, entity string, subj domain.RelianceSubject) (*domain.RelianceStanding, error) {
	s.calls++
	s.actor = entity
	return &domain.RelianceStanding{LegalEntityID: entity, Subject: subj, Withdrawn: 1, Certifications: []domain.ReliedCertification{}}, s.err
}

func TestRelianceAPI_StandingIsAuthorizedOnTheNamedEntityAndAcceptsOnlyItsParameters(t *testing.T) {
	const path = "/v1/reconciliation/certification-reliances"
	st := &relianceStore{}
	az := &recordingAuthz{deny: map[string]bool{ActionReconciliationRead: true}}
	if w := serve(relianceServer(st, az), "GET", path+standingQuery, ""); w.Code != http.StatusForbidden || st.calls != 0 {
		t.Fatalf("without read: %d, store calls %d", w.Code, st.calls)
	}
	st = &relianceStore{}
	az = &recordingAuthz{}
	w := serve(relianceServer(st, az), "GET", path+standingQuery, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"withdrawn":1`) || st.actor != entityA ||
		len(az.entities) != 1 || az.entities[0] != entityA || az.actions[0] != ActionReconciliationRead {
		t.Fatalf("standing: %d %s (entity %q, authorized %v on %v)", w.Code, w.Body, st.actor, az.actions, az.entities)
	}
	st = &relianceStore{}
	h := relianceServer(st, &recordingAuthz{})
	for name, q := range map[string]string{
		"misspelt filter":     standingQuery + "&relying_servce=x",
		"repeated parameter":  standingQuery + "&relying_service=other",
		"no entity":           "?relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=jb_1",
		"entity not a UUID":   "?legal_entity_id=nope&relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=jb_1",
		"missing object id":   "?legal_entity_id=" + entityA + "&relying_service=gl-svc&relying_object_type=journal-batch",
		"malformed object id": "?legal_entity_id=" + entityA + "&relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=a%20b",
	} {
		if w := serve(h, "GET", path+q, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed query reached the store %d times", st.calls)
	}
}
