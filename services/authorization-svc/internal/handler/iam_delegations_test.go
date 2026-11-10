package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// ZS-IAM-001 §21 names POST /v1/iam/delegations and
// POST /v1/iam/delegations/{id}:revoke; no service served them (tracker
// D2-5.F19). They are aliases of /v1/admin/delegated-authorities: the same
// handler, the same checks, the same answer.

const iamTenant = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func iamDo(t *testing.T, s *stubStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", iamTenant)
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

func TestIAMDelegations_CreateParity(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"own authority":     {`{"delegator_principal_id":"admin-1","delegate_principal_id":"p-2","scope_type":"FULL","effective_from":"2026-01-01T00:00:00Z"}`, http.StatusCreated},
		"someone else's":    {`{"delegator_principal_id":"someone-else","delegate_principal_id":"p-2","scope_type":"FULL","effective_from":"2026-01-01T00:00:00Z"}`, http.StatusForbidden},
		"malformed request": {`{`, http.StatusBadRequest},
	}
	for name, c := range cases {
		admin := iamDo(t, &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}, http.MethodPost, "/v1/admin/delegated-authorities", c.body)
		alias := iamDo(t, &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}, http.MethodPost, "/v1/iam/delegations", c.body)
		if admin.Code != c.want || alias.Code != c.want {
			t.Errorf("%s: admin=%d alias=%d, want %d (alias body %s)", name, admin.Code, alias.Code, c.want, alias.Body.String())
		}
		if admin.Body.String() != alias.Body.String() {
			t.Errorf("%s: bodies differ:\nadmin %s\nalias %s", name, admin.Body.String(), alias.Body.String())
		}
	}
}

func TestIAMDelegations_ListParity(t *testing.T) {
	admin := iamDo(t, &stubStore{}, http.MethodGet, "/v1/admin/delegated-authorities", "")
	alias := iamDo(t, &stubStore{}, http.MethodGet, "/v1/iam/delegations", "")
	if admin.Code != alias.Code || admin.Body.String() != alias.Body.String() {
		t.Fatalf("list differs: admin %d %s / alias %d %s", admin.Code, admin.Body.String(), alias.Code, alias.Body.String())
	}
}

// The colon form is the one §21 documents; the slash form is served too,
// because some HTTP tooling percent-encodes a colon in a path segment (the
// same pair access-control-svc serves for /v1/iam/access-assignments).
func TestIAMDelegations_RevokeParity(t *testing.T) {
	for _, path := range []string{
		"/v1/admin/delegated-authorities/d-1/revoke",
		"/v1/iam/delegations/d-1:revoke",
		"/v1/iam/delegations/d-1/revoke",
	} {
		notMine := iamDo(t, &stubStore{findDelegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1", DelegatorPrincipalID: "someone-else"}},
			http.MethodPost, path, `{"reason_code":"LEAVER"}`)
		if notMine.Code != http.StatusForbidden {
			t.Errorf("%s: revoking someone else's delegation: got %d, want 403: %s", path, notMine.Code, notMine.Body.String())
		}
		done := iamDo(t, &stubStore{
			findDelegation:      &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1", DelegatorPrincipalID: "admin-1"},
			revokeDelegationErr: domain.ErrInvalidTransition,
		}, http.MethodPost, path, `{"reason_code":"LEAVER"}`)
		if done.Code != http.StatusConflict {
			t.Errorf("%s: revoking an ended delegation: got %d, want 409: %s", path, done.Code, done.Body.String())
		}
	}
}
