package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// HTTP-layer tests for the DQC-01 contract registry: the permission each route needs, the strict
// request contract, and the status each store error maps to.

type contractStore struct {
	store.Store
	calls  int
	err    error
	gotVer int
	gotReq domain.PublishContractRequest
	gotKey string
}

func (s *contractStore) ok() *domain.ContractVersion {
	return &domain.ContractVersion{ContractID: "dct_x", AssetID: "dca_x", VersionNumber: 1, Status: domain.ContractDraft}
}

func (s *contractStore) CreateContract(_ context.Context, _ string, _ domain.CreateContractRequest, _ string, c domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	s.calls++
	s.gotKey = c.Key
	return s.ok(), s.err
}

func (s *contractStore) GetContractVersion(_ context.Context, _, _ string, n int) (*domain.ContractVersion, error) {
	s.calls++
	s.gotVer = n
	return s.ok(), s.err
}

func (s *contractStore) PublishContract(_ context.Context, _, _ string, n int, _ string, r domain.PublishContractRequest, _ domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	s.calls++
	s.gotVer, s.gotReq = n, r
	return s.ok(), s.err
}

func (s *contractStore) RetireContract(_ context.Context, _, _ string, _ int, _, _ string, _ domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	s.calls++
	return s.ok(), s.err
}

func (s *contractStore) CheckContractCompatibility(_ context.Context, _, _ string, _ domain.CompatibilityCheckRequest) (*domain.CompatibilityReport, error) {
	s.calls++
	return &domain.CompatibilityReport{Compatible: true}, s.err
}

// denyAction answers GRANTED unless the call is for the named action.
func denyAction(t *testing.T, action string) *authzStub {
	t.Helper()
	a := &authzStub{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		outcome := "GRANTED"
		if action != "" && strings.Contains(string(b), action) {
			outcome = "DENIED"
		}
		_, _ = w.Write([]byte(`{"decision_outcome":"` + outcome + `"}`))
	}))
	t.Cleanup(a.Close)
	return a
}

const goodCreate = `{"asset_key":"ap.invoice","asset_kind":"dataset","body":{"schema":[{"name":"a","type":"string"}],"rules":[{"rule_key":"r","dimension":"validity","severity":"BLOCKER"}],"compatibility":"BACKWARD"}}`

func TestContractAPI_EachRouteNeedsItsOwnPermission(t *testing.T) {
	type route struct{ method, path, body, action string }
	routes := []route{
		{"POST", "/v1/data-quality/dqc/contracts", goodCreate, "DQC_CONTRACT_MANAGE"},
		{"POST", "/v1/data-quality/dqc/contracts/dca_x/versions/1:submit", ``, "DQC_CONTRACT_MANAGE"},
		{"POST", "/v1/data-quality/dqc/contracts/dca_x/versions/1:publish", ``, "DQC_CONTRACT_PUBLISH"},
		{"POST", "/v1/data-quality/dqc/contracts/dca_x/versions/1:deprecate", `{"reason":"x"}`, "DQC_CONTRACT_PUBLISH"},
		{"POST", "/v1/data-quality/dqc/contracts/dca_x/versions/1:retire", `{"reason":"x"}`, "DQC_CONTRACT_PUBLISH"},
		{"GET", "/v1/data-quality/dqc/contracts/dca_x/versions/1", ``, "DQC_CONTRACT_READ"},
		{"GET", "/v1/data-quality/dqc/contracts/dca_x/versions", ``, "DQC_CONTRACT_READ"},
		{"POST", "/v1/data-quality/dqc/contracts/dca_x/compatibility-check", `{"against_version":1,"candidate":{}}`, "DQC_CONTRACT_READ"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// Denied for exactly this route's action: nothing reaches the store.
			st := &contractStore{}
			srv := newServer(t, st, denyAction(t, rt.action))
			if rec := do(t, srv, rt.method, rt.path, rt.body, nil); rec.Code != 403 || st.calls != 0 {
				t.Fatalf("without %s: status %d, store calls %d", rt.action, rec.Code, st.calls)
			}
			// Granted everything except a different action: the route works. This proves the denial
			// above was about THIS action and not about the route being broken.
			other := "DQC_CONTRACT_READ"
			if rt.action == other {
				other = "DQC_CONTRACT_PUBLISH"
			}
			st2 := &contractStore{}
			srv2 := newServer(t, st2, denyAction(t, other))
			if rec := do(t, srv2, rt.method, rt.path, rt.body, nil); rec.Code == 403 {
				t.Fatalf("denying %s blocked a route that needs %s: %s", other, rt.action, rec.Body.String())
			}
		})
	}
}

func TestContractAPI_StrictBodiesAndVersions(t *testing.T) {
	st := &contractStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, tc := range map[string]struct{ path, body string }{
		"unknown field on create":  {"/v1/data-quality/dqc/contracts", `{"asset_key":"a.b","asset_kind":"dataset","body":{},"surprise":1}`},
		"empty create body":        {"/v1/data-quality/dqc/contracts", ``},
		"trailing data":            {"/v1/data-quality/dqc/contracts", goodCreate + `{}`},
		"unknown field on publish": {"/v1/data-quality/dqc/contracts/dca_x/versions/1:publish", `{"acknowledge":true}`},
		"retire without a body":    {"/v1/data-quality/dqc/contracts/dca_x/versions/1:retire", ``},
		"version zero":             {"/v1/data-quality/dqc/contracts/dca_x/versions/0:publish", ``},
		"version not a number":     {"/v1/data-quality/dqc/contracts/dca_x/versions/latest:publish", ``},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(t, srv, "POST", tc.path, tc.body, nil); rec.Code != 400 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if st.calls != 0 {
		t.Fatalf("a malformed request reached the store %d times", st.calls)
	}
	// There is no "latest" read: the word is not a version.
	if rec := do(t, srv, "GET", "/v1/data-quality/dqc/contracts/dca_x/versions/latest", ``, nil); rec.Code != 400 || st.calls != 0 {
		t.Fatalf("GET .../versions/latest: %d (store calls %d)", rec.Code, st.calls)
	}
	// An empty publish body is fine (acknowledge_breaking defaults to false) and the version is passed through.
	if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts/dca_x/versions/7:publish", ``, nil); rec.Code != 200 || st.gotVer != 7 || st.gotReq.AcknowledgeBreaking {
		t.Fatalf("publish: %d ver=%d req=%+v", rec.Code, st.gotVer, st.gotReq)
	}
	if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts/dca_x/versions/7:publish", `{"acknowledge_breaking":true}`, nil); rec.Code != 200 || !st.gotReq.AcknowledgeBreaking {
		t.Fatalf("acknowledged publish: %d %+v", rec.Code, st.gotReq)
	}
}

func TestContractAPI_ErrorStatuses(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"unknown contract":        {domain.ErrContractNotFound, 404},
		"unknown version":         {domain.ErrContractVersionUnknown, 404},
		"bad transition":          {domain.ErrContractTransition, 409},
		"open draft":              {domain.ErrContractDraftOpen, 409},
		"not acknowledged":        {domain.ErrBreakingNotAcknowledged, 409},
		"publisher not separate":  {domain.ErrContractNotIndependent, 403},
		"invalid contract":        {domain.ErrContractInvalid, 400},
		"idempotency key reused":  {domain.ErrIdempotencyKeyReused, 409},
		"database immutable stop": {domain.ErrImmutableViolation, 409},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, &contractStore{err: tc.err}, denyAction(t, ""))
			if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts/dca_x/versions/1:publish", ``, nil); rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// DQC-003 carries the breaking-change evidence in the body, not just a status.
func TestContractAPI_IncompatibleChangeReturnsEvidence(t *testing.T) {
	inc := &store.IncompatibleError{Report: domain.CompatibilityReport{
		Mode: domain.CompatBackward, AgainstVersion: 2,
		Breaking: []domain.BreakingChange{{Field: "amount", Kind: domain.ErrKindFieldRemoved}},
	}}
	srv := newServer(t, &contractStore{err: inc}, denyAction(t, ""))
	rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts/dca_x/versions/3:publish", ``, nil)
	body := rec.Body.String()
	if rec.Code != 409 || !strings.Contains(body, `"code":"DQC-003"`) || !strings.Contains(body, "REQUIRED_FIELD_REMOVED") || !strings.Contains(body, `"against_version":2`) {
		t.Fatalf("status %d body %s", rec.Code, body)
	}
}

func TestContractAPI_NoTenantOrPrincipalNeverReachesTheStore(t *testing.T) {
	st := &contractStore{}
	srv := newServer(t, st, denyAction(t, ""))
	if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts", goodCreate, map[string]string{"X-Principal-Id": ""}); rec.Code != 401 {
		t.Fatalf("no principal: %d", rec.Code)
	}
	if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts", goodCreate, map[string]string{"X-Tenant-Id": ""}); rec.Code < 400 {
		t.Fatalf("no tenant: %d", rec.Code)
	}
	if rec := do(t, srv, "POST", "/v1/data-quality/dqc/contracts", goodCreate, map[string]string{"Idempotency-Key": ""}); rec.Code < 400 {
		t.Fatalf("no idempotency key: %d", rec.Code)
	}
	if st.calls != 0 {
		t.Fatalf("the store was reached %d times", st.calls)
	}
}

// A run request naming a contract reaches the store with the contract fields, and the error for a contract
// that is not PUBLISHED is a 409, not a 400 or a 500.
type runStore struct {
	store.Store
	req domain.RunDQRequest
	err error
}

func (s *runStore) RunDQ(_ context.Context, tenant string, req domain.RunDQRequest, _ string, _ domain.IdempotencyClaim) (*domain.DQRun, error) {
	s.req = req
	if s.err != nil {
		return nil, s.err
	}
	id := "dct_x"
	return &domain.DQRun{RunID: "dqn_x", TenantID: tenant, ContractID: &id}, nil
}

const contractRunBody = `{"contract_asset_id":"dca_x","contract_version":3,"population_ref":"p","population_row_count":1,"population_content_hash":"` + hash64 + `","rule_outcomes":[{"rule_key":"r","pass_count":1,"fail_count":0,"failure_magnitude_sum":"0"}]}`

func TestRunAPI_ContractFieldsReachTheStore(t *testing.T) {
	st := &runStore{}
	srv := newServer(t, st, denyAction(t, ""))
	rec := do(t, srv, "POST", "/v1/data-quality/runs", contractRunBody, nil)
	if rec.Code != 201 || st.req.ContractAssetID != "dca_x" || st.req.ContractVersion != 3 || st.req.RuleSetVersionID != "" {
		t.Fatalf("status %d req %+v body %s", rec.Code, st.req, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"contract_id":"dct_x"`) {
		t.Fatalf("the run response does not name its contract: %s", rec.Body.String())
	}
}

func TestRunAPI_ContractErrorStatuses(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not published": {domain.ErrContractNotPublished, 409},
		"unknown":       {domain.ErrContractVersionUnknown, 404},
		"no such asset": {domain.ErrContractNotFound, 404},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, &runStore{err: tc.err}, denyAction(t, ""))
			if rec := do(t, srv, "POST", "/v1/data-quality/runs", contractRunBody, nil); rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Revoking a certification is its own permission and a strict request.
type revokeStore struct {
	store.Store
	calls int
	req   domain.RevokeCertificationRequest
	err   error
}

func (s *revokeStore) RevokeCertification(_ context.Context, _, _, _ string, req domain.RevokeCertificationRequest, _ domain.IdempotencyClaim) (*domain.CertificationRevocation, error) {
	s.calls++
	s.req = req
	if s.err != nil {
		return nil, s.err
	}
	return &domain.CertificationRevocation{RevocationID: "dqx_x", ReasonCode: req.ReasonCode}, nil
}

const revokePath = "/v1/data-quality/runs/dqn_x/certification:revoke"
const revokeBody = `{"reason_code":"FALSE_PASS","reason":"control defect","evidence_refs":["incident-4471"]}`

func TestRevokeAPI_NeedsItsOwnPermission(t *testing.T) {
	st := &revokeStore{}
	if rec := do(t, newServer(t, st, denyAction(t, "DQC_CERTIFICATION_REVOKE")), "POST", revokePath, revokeBody, nil); rec.Code != 403 || st.calls != 0 {
		t.Fatalf("without the permission: %d, store calls %d", rec.Code, st.calls)
	}
	// Denying every OTHER action does not block it: the denial above was about this permission.
	st2 := &revokeStore{}
	for _, other := range []string{"DATA_QUALITY_MANAGE", "DQC_CONTRACT_PUBLISH"} {
		if rec := do(t, newServer(t, st2, denyAction(t, other)), "POST", revokePath, revokeBody, nil); rec.Code != 201 {
			t.Fatalf("denying %s blocked revocation: %d %s", other, rec.Code, rec.Body.String())
		}
	}
}

func TestRevokeAPI_StrictBodyAndStatuses(t *testing.T) {
	st := &revokeStore{}
	srv := newServer(t, st, denyAction(t, ""))
	for name, body := range map[string]string{
		"empty": ``, "unknown field": `{"reason_code":"FALSE_PASS","reason":"x","notify":true}`, "trailing": revokeBody + `{}`,
	} {
		if rec := do(t, srv, "POST", revokePath, body, nil); rec.Code != 400 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
	if st.calls != 0 {
		t.Fatalf("a malformed request reached the store %d times", st.calls)
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"already revoked":          {domain.ErrCertificationAlreadyRevoked, 409},
		"revoker is the certifier": {domain.ErrRevokerIsCertifier, 403},
		"invalid":                  {domain.ErrRevocationInvalid, 400},
		"no certificate":           {domain.ErrCertificationNotFound, 404},
	} {
		rec := do(t, newServer(t, &revokeStore{err: tc.err}, denyAction(t, "")), "POST", revokePath, revokeBody, nil)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d", name, rec.Code, tc.want)
		}
	}
	if rec := do(t, srv, "POST", revokePath, revokeBody, nil); rec.Code != 201 || st.req.ReasonCode != domain.RevokeFalsePass {
		t.Fatalf("valid: %d %+v", rec.Code, st.req)
	}
}
