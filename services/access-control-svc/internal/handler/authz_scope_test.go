package handler_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// These lock in the fix for a defect that made this service unusable while
// looking like an outage.
//
// Every admin call to authorization-svc is provisioning — it is what turns a
// role definition recorded here into something actually enforced. Two of the
// three calls used to be made with an empty principal and an empty tenant
// (`c.post(ctx, path, body, "", "", correlationID)`), and authorization-svc's
// admin routes call requirePrincipal and requireTenant, so those calls could
// only ever answer 401. This service reported that as
// `authz_admin_unavailable`, which reads as "authorization-svc is down" rather
// than "we never populated the request".
//
// The signatures now take a clients.Scope, so an omission is a compile error.
// These tests assert the values actually arrive, because a struct with the
// right shape and empty fields would compile perfectly and fail identically.

func TestCreateRole_ForwardsVerifiedScopeToAuthzAdmin(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)

	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 1 {
		t.Fatalf("expected 1 admin call, got %d", len(admin.gotScopes))
	}

	got := admin.gotScopes[0]
	if got.PrincipalID != "admin-1" {
		t.Errorf("principal not forwarded: got %q, want admin-1", got.PrincipalID)
	}
	if got.TenantID == "" {
		t.Error("tenant not forwarded — authorization-svc answers 401 missing_tenant_scope for this")
	}
	// The entity the caller was authorized against. Provisioning under a
	// different one would enforce the role somewhere the CheckAllowed never
	// covered.
	if got.LegalEntityID != "le-us" {
		t.Errorf("legal entity not forwarded: got %q, want le-us", got.LegalEntityID)
	}
	if got.CorrelationID == "" {
		t.Error("correlation id not forwarded — the provisioning call would be untraceable to the request that caused it")
	}
}

func TestCreateBundle_ForwardsVerifiedScopeToAuthzAdmin(t *testing.T) {
	store := newStubStore()
	admin := &stubAuthzAdmin{}
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{}, admin)

	// The role has to exist for the bundle route to reach the admin client.
	correlationID := uuid.NewString()
	if rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(correlationID), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("seed role: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	// Only one role was seeded, so the single key is the id the route needs.
	var roleID string
	for id := range store.rolesByID {
		roleID = id
	}
	if roleID == "" {
		t.Fatal("stub store did not record a role id")
	}

	admin.gotScopes = nil // drop the CreateRole scope; assert on the bundle call

	body := map[string]any{
		"legal_entity_id":   "le-us",
		"bundle_code":       "PROCUREMENT_BUNDLE",
		"permitted_actions": []string{"PO_ISSUE"},
		"correlation_id":    uuid.NewString(),
	}
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+roleID+"/permission-bundles", body, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 1 {
		t.Fatalf("expected 1 admin call, got %d", len(admin.gotScopes))
	}

	// This is the call that used to pass "", "" — the regression this test exists for.
	got := admin.gotScopes[0]
	if got.PrincipalID != "admin-1" {
		t.Errorf("principal not forwarded: got %q", got.PrincipalID)
	}
	if got.TenantID == "" {
		t.Error("tenant not forwarded on the bundle call — this is the exact omission that made bundle provisioning always 401")
	}
	if got.LegalEntityID != "le-us" {
		t.Errorf("legal entity not forwarded: got %q, want le-us", got.LegalEntityID)
	}
}
