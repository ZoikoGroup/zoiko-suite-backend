package handler_test

import (
	"context"
	"strings"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// HTTP-layer tests for reliance on a certification and its blast radius: each route's permission, the strict body,
// 201 versus 200, and the status each store error maps to.

type relianceStore struct {
	store.Store
	calls   int
	created bool
	err     error
}

func (s *relianceStore) RecordReliance(_ context.Context, _, runID, actor string, req domain.RecordRelianceRequest) (*domain.CertificationReliance, bool, error) {
	s.calls++
	return &domain.CertificationReliance{RelianceID: "dqr_x", RunID: runID, RecordedBy: actor, RelyingService: req.RelyingService}, s.created, s.err
}

func (s *relianceStore) GetBlastRadius(_ context.Context, _, runID string) (*domain.BlastRadius, error) {
	s.calls++
	return &domain.BlastRadius{RunID: runID, Status: domain.CertificationRevoked, Reliances: []domain.CertificationReliance{}, ByService: map[string]int{}}, s.err
}

const (
	relyPath   = "/v1/data-quality/runs/dqn_x/certification/reliances"
	radiusPath = "/v1/data-quality/runs/dqn_x/certification/blast-radius"
	relyBody   = `{"relying_service":"gl-svc","relying_object_type":"journal-batch","relying_object_id":"jb_1","purpose":"posted on the certified invoices"}`
)

func TestRelianceAPI_EachRouteNeedsItsOwnPermission(t *testing.T) {
	for _, rt := range []struct{ method, path, body, action string }{
		{"POST", relyPath, relyBody, "DQC_RELIANCE_RECORD"},
		{"GET", radiusPath, ``, "DATA_QUALITY_READ"},
	} {
		st := &relianceStore{}
		if rec := do(t, newServer(t, st, denyAction(t, rt.action)), rt.method, rt.path, rt.body, nil); rec.Code != 403 || st.calls != 0 {
			t.Fatalf("%s without %s: %d, store calls %d", rt.path, rt.action, rec.Code, st.calls)
		}
		// Recording reliance is not the power to revoke, certify or manage runs.
		for _, other := range []string{"DQC_CERTIFICATION_REVOKE", "DATA_QUALITY_MANAGE", "DQC_RISK_APPROVE"} {
			if rec := do(t, newServer(t, &relianceStore{}, denyAction(t, other)), rt.method, rt.path, rt.body, nil); rec.Code >= 400 {
				t.Fatalf("denying %s blocked %s: %d %s", other, rt.path, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestRelianceAPI_CreatedVersusExistingAndTheRecordingPrincipal(t *testing.T) {
	st := &relianceStore{created: true}
	rec := do(t, newServer(t, st, denyAction(t, "")), "POST", relyPath, relyBody, nil)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"relying_service":"gl-svc"`) {
		t.Fatalf("new reliance: %d %s", rec.Code, rec.Body.String())
	}
	st.created = false
	if rec := do(t, newServer(t, st, denyAction(t, "")), "POST", relyPath, relyBody, nil); rec.Code != 200 {
		t.Fatalf("existing reliance: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRelianceAPI_StrictBodiesAndErrorStatuses(t *testing.T) {
	st := &relianceStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, body := range map[string]string{
		"empty":         ``,
		"unknown field": strings.Replace(relyBody, `"purpose"`, `"recorded_by":"someone-else","purpose"`, 1),
	} {
		if rec := do(t, srv, "POST", relyPath, body, nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed body reached the store %d times", st.calls)
	}
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"inactive": {domain.ErrRelianceInactive, 409},
		"invalid":  {domain.ErrRelianceInvalid, 400},
		"no cert":  {domain.ErrCertificationNotFound, 404},
	} {
		if rec := do(t, newServer(t, &relianceStore{err: tc.err}, denyAction(t, "")), "POST", relyPath, relyBody, nil); rec.Code != tc.code {
			t.Fatalf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), tc.code)
		}
	}
	if rec := do(t, newServer(t, &relianceStore{err: domain.ErrCertificationNotFound}, denyAction(t, "")), "GET", radiusPath, ``, nil); rec.Code != 404 {
		t.Fatalf("blast radius of an unknown run: %d", rec.Code)
	}
	rec := do(t, newServer(t, &relianceStore{}, denyAction(t, "")), "GET", radiusPath, ``, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reliances":[]`) || !strings.Contains(rec.Body.String(), `"status":"REVOKED"`) {
		t.Fatalf("blast radius: %d %s", rec.Code, rec.Body.String())
	}
}

const standingPath = "/v1/data-quality/certification-reliances?relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=jb_1"

func (s *relianceStore) GetRelianceStanding(_ context.Context, _ string, subj domain.RelianceSubject) (*domain.RelianceStanding, error) {
	s.calls++
	return &domain.RelianceStanding{Subject: subj, Withdrawn: 1, Certifications: []domain.ReliedCertification{}}, s.err
}

func TestRelianceAPI_StandingNeedsReadAndAcceptsOnlyItsThreeParameters(t *testing.T) {
	st := &relianceStore{}
	if rec := do(t, newServer(t, st, denyAction(t, "DATA_QUALITY_READ")), "GET", standingPath, ``, nil); rec.Code != 403 || st.calls != 0 {
		t.Fatalf("without read: %d, store calls %d", rec.Code, st.calls)
	}
	// Recording reliance is not required to ask what an object rests on.
	if rec := do(t, newServer(t, &relianceStore{}, denyAction(t, "DQC_RELIANCE_RECORD")), "GET", standingPath, ``, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"withdrawn":1`) {
		t.Fatalf("standing: %d %s", rec.Code, rec.Body.String())
	}
	st = &relianceStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, p := range map[string]string{
		"misspelt filter":     standingPath + "&relying_servce=x",
		"repeated parameter":  standingPath + "&relying_service=other",
		"missing object id":   "/v1/data-quality/certification-reliances?relying_service=gl-svc&relying_object_type=journal-batch",
		"malformed object id": "/v1/data-quality/certification-reliances?relying_service=gl-svc&relying_object_type=journal-batch&relying_object_id=a%20b",
	} {
		if rec := do(t, srv, "GET", p, ``, nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed query reached the store %d times", st.calls)
	}
}
