package handler_test

import (
	"context"
	"strings"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// HTTP-layer tests for the /dqc/* routes: the permission each needs, the strict request, and the status
// each outcome maps to (201 for a new record, 200 for the existing one, 409 for a conflict).

type facadeStore struct {
	store.Store
	calls int
	err   error
	dedup bool
	run   domain.RunDQRequest
	cert  string
}

func (s *facadeStore) RecordObservation(_ context.Context, _ string, req domain.RecordObservationRequest, _ string, _ domain.IdempotencyClaim) (*domain.DQObservation, error) {
	s.calls++
	return &domain.DQObservation{ObservationRunID: "dqo_x", AssetID: req.AssetID, Deduplicated: s.dedup}, s.err
}

func (s *facadeStore) GetObservation(_ context.Context, _, id string) (*domain.DQObservation, error) {
	s.calls++
	return &domain.DQObservation{ObservationRunID: id}, s.err
}

func (s *facadeStore) RunDQ(_ context.Context, _ string, req domain.RunDQRequest, _ string, _ domain.IdempotencyClaim) (*domain.DQRun, error) {
	s.calls++
	s.run = req
	return &domain.DQRun{RunID: "dqn_x", Deduplicated: s.dedup}, s.err
}

func (s *facadeStore) CertifyDQ(_ context.Context, _, runID, _ string, _ domain.CertifyDQRequest, _ domain.IdempotencyClaim) (*domain.DQCertification, error) {
	s.calls++
	s.cert = runID
	return &domain.DQCertification{CertificationID: "dqc_x", RunID: runID}, s.err
}

func (s *facadeStore) RaiseIssue(_ context.Context, _ string, req domain.RaiseIssueRequest, _ string, _ domain.IdempotencyClaim) (*domain.DQIssue, error) {
	s.calls++
	return &domain.DQIssue{IssueID: "dqi_x", Deduplicated: s.dedup}, s.err
}

const (
	obsBody   = `{"asset_id":"dca_x","window_start":"2026-11-01T00:00:00Z","window_end":"2026-11-02T00:00:00Z","source_watermark":"wm-1","source_system":"erp","row_count":10,"evidence_hash":"` + hash64 + `"}`
	valBody   = `{"contract_asset_id":"dca_x","contract_version":2,"population_ref":"p","population_row_count":1,"population_content_hash":"` + hash64 + `","rule_outcomes":[{"rule_key":"r","pass_count":1,"fail_count":0,"failure_magnitude_sum":"0"}]}`
	certBody  = `{"run_id":"dqn_x"}`
	excBody   = `{"run_id":"dqn_x","result_id":"dqr_x","description":"wrong"}`
	dqcPrefix = "/v1/data-quality/dqc"
)

func TestFacadeAPI_EachRouteNeedsItsPermission(t *testing.T) {
	routes := []struct{ method, path, body, action string }{
		{"POST", dqcPrefix + "/observations", obsBody, "DATA_QUALITY_MANAGE"},
		{"GET", dqcPrefix + "/observations/dqo_x", ``, "DATA_QUALITY_READ"},
		{"POST", dqcPrefix + "/validate", valBody, "DATA_QUALITY_MANAGE"},
		{"POST", dqcPrefix + "/certifications", certBody, "DATA_QUALITY_MANAGE"},
		{"POST", dqcPrefix + "/exceptions", excBody, "DATA_QUALITY_MANAGE"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			st := &facadeStore{}
			if rec := do(t, newServer(t, st, denyAction(t, rt.action)), rt.method, rt.path, rt.body, nil); rec.Code != 403 || st.calls != 0 {
				t.Fatalf("without %s: %d, store calls %d", rt.action, rec.Code, st.calls)
			}
			other := "DQC_CONTRACT_PUBLISH"
			if rec := do(t, newServer(t, &facadeStore{}, denyAction(t, other)), rt.method, rt.path, rt.body, nil); rec.Code >= 400 {
				t.Fatalf("denying %s blocked a route that needs %s: %d %s", other, rt.action, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFacadeAPI_StrictBodies(t *testing.T) {
	st := &facadeStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, tc := range map[string]struct{ path, body string }{
		"observation unknown field": {"/observations", strings.Replace(obsBody, `"row_count":10`, `"row_count":10,"extra":1`, 1)},
		"observation empty":         {"/observations", ``},
		"validate unknown field":    {"/validate", strings.Replace(valBody, `"population_ref":"p"`, `"population_ref":"p","extra":1`, 1)},
		"certification no run":      {"/certifications", `{}`},
		"certification unknown":     {"/certifications", `{"run_id":"dqn_x","force":true}`},
		"exception unknown field":   {"/exceptions", `{"run_id":"a","result_id":"b","description":"c","severity":"INFO"}`},
	} {
		if rec := do(t, srv, "POST", dqcPrefix+tc.path, tc.body, nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Validation is judged against an exact contract: a rule-set version, a missing version, or both are refused.
	for name, body := range map[string]string{
		"rule set only": strings.Replace(valBody, `"contract_asset_id":"dca_x","contract_version":2`, `"ruleset_version_id":"dqv_x"`, 1),
		"no version":    strings.Replace(valBody, `"contract_version":2`, `"contract_version":0`, 1),
		"both":          strings.Replace(valBody, `"contract_version":2`, `"contract_version":2,"ruleset_version_id":"dqv_x"`, 1),
		"neither":       strings.Replace(valBody, `"contract_asset_id":"dca_x","contract_version":2,`, ``, 1),
	} {
		if rec := do(t, srv, "POST", dqcPrefix+"/validate", body, nil); rec.Code != 400 {
			t.Fatalf("validate %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed request reached the store %d times", st.calls)
	}
	if rec := do(t, srv, "POST", dqcPrefix+"/validate", valBody, nil); rec.Code != 201 || st.run.ContractVersion != 2 || st.run.ContractAssetID != "dca_x" {
		t.Fatalf("a valid validate: %d %+v", rec.Code, st.run)
	}
	if rec := do(t, srv, "POST", dqcPrefix+"/certifications", certBody, nil); rec.Code != 201 || st.cert != "dqn_x" {
		t.Fatalf("a valid certification: %d run=%q", rec.Code, st.cert)
	}
}

// 201 for something new, 200 for the record that already exists.
func TestFacadeAPI_ExistingRecordIs200(t *testing.T) {
	for name, tc := range map[string]struct{ path, body string }{
		"observation": {"/observations", obsBody}, "validate": {"/validate", valBody}, "exception": {"/exceptions", excBody},
	} {
		if rec := do(t, newServer(t, &facadeStore{}, denyAction(t, "")), "POST", dqcPrefix+tc.path, tc.body, nil); rec.Code != 201 {
			t.Fatalf("%s new: %d", name, rec.Code)
		}
		rec := do(t, newServer(t, &facadeStore{dedup: true}, denyAction(t, "")), "POST", dqcPrefix+tc.path, tc.body, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deduplicated":true`) {
			t.Fatalf("%s existing: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// The service's own routes behave the same way.
	for name, tc := range map[string]struct{ path, body string }{
		"runs": {"/v1/data-quality/runs", valBody}, "issues": {"/v1/data-quality/issues", excBody},
	} {
		if rec := do(t, newServer(t, &facadeStore{dedup: true}, denyAction(t, "")), "POST", tc.path, tc.body, nil); rec.Code != 200 {
			t.Fatalf("%s existing: %d", name, rec.Code)
		}
	}
}

func TestFacadeAPI_ErrorStatuses(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		err        error
		want       int
	}{
		"observation conflict":        {"/observations", obsBody, domain.ErrObservationConflict, 409},
		"observation unknown asset":   {"/observations", obsBody, domain.ErrContractNotFound, 404},
		"reperform of same evidence":  {"/validate", valBody, domain.ErrEvidenceAlreadyRecorded, 409},
		"validate unpublished":        {"/validate", valBody, domain.ErrContractNotPublished, 409},
		"exception on a clean run":    {"/exceptions", excBody, domain.ErrRunNotInExceptionsOpen, 409},
		"certification stale":         {"/certifications", certBody, domain.ErrEvidenceStale, 409},
		"certification not found run": {"/certifications", certBody, domain.ErrRunNotFound, 404},
	} {
		rec := do(t, newServer(t, &facadeStore{err: tc.err}, denyAction(t, "")), "POST", dqcPrefix+tc.path, tc.body, nil)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if rec := do(t, newServer(t, &facadeStore{err: domain.ErrObservationNotFound}, denyAction(t, "")), "GET", dqcPrefix+"/observations/dqo_x", ``, nil); rec.Code != 404 {
		t.Fatalf("an unknown observation: %d", rec.Code)
	}
}

// "Conflict returns existing certification or 409": the 409 names the certification that holds the basis.
func TestFacadeAPI_CertificationConflictNamesTheExistingCertification(t *testing.T) {
	st := &facadeStore{err: &domain.BasisExistsError{ExistingID: "dqc_existing"}}
	for _, path := range []string{dqcPrefix + "/certifications", "/v1/data-quality/runs/dqn_x:certify"} {
		body := certBody
		if strings.Contains(path, ":certify") {
			body = ``
		}
		rec := do(t, newServer(t, st, denyAction(t, "")), "POST", path, body, nil)
		b := rec.Body.String()
		if rec.Code != 409 || !strings.Contains(b, `"existing_certification_id":"dqc_existing"`) || !strings.Contains(b, `"code":"DQC-015"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, b)
		}
	}
}
