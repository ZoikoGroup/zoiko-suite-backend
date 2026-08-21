package entity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/accounts-receivable-svc/internal/entity"
)

const (
	callerTenant = "11111111-1111-1111-1111-111111111111"
	otherTenant  = "99999999-9999-9999-9999-999999999999"
	entityID     = "22222222-2222-2222-2222-222222222222"
)

// fakeRegistry serves GET /v1/entities/{id}. It deliberately does NOT filter by the
// X-Tenant-Id header, because the real tenant-entity-registry-svc does not either —
// its GetEntity takes no tenant scope at all and serves any entity to anyone who
// asks. That is exactly why this client compares the tenant itself instead of
// trusting a 404, and a fake that filtered would hide the whole point.
func fakeRegistry(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyInTenant_EntityInCallerTenant_Accepted(t *testing.T) {
	srv := fakeRegistry(t, http.StatusOK, entity.LegalEntity{
		LegalEntityID: entityID, TenantID: callerTenant, EntityStatus: "ACTIVE",
	})

	if err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID); err != nil {
		t.Fatalf("expected an in-tenant ACTIVE entity to be accepted, got %v", err)
	}
}

// TestVerifyInTenant_EntityInAnotherTenant_Refused is the gap this package closes.
// The registry answers 200 with the entity — it does not scope the read — so
// without this comparison a caller could name any tenant's legal entity on a write.
func TestVerifyInTenant_EntityInAnotherTenant_Refused(t *testing.T) {
	srv := fakeRegistry(t, http.StatusOK, entity.LegalEntity{
		LegalEntityID: entityID, TenantID: otherTenant, EntityStatus: "ACTIVE",
	})

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrForeignTenant) {
		t.Fatalf("CROSS-TENANT ATTRIBUTION: expected ErrForeignTenant, got %v", err)
	}
}

func TestVerifyInTenant_UnknownEntity_Refused(t *testing.T) {
	srv := fakeRegistry(t, http.StatusNotFound, map[string]string{"error": "not_found"})

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestVerifyInTenant_InactiveStatuses_Refused — the allow-list is ACTIVE only. A
// dissolved or suspended entity plainly must not raise new receivables, and a
// DORMANT one is by definition not trading.
func TestVerifyInTenant_InactiveStatuses_Refused(t *testing.T) {
	for _, status := range []string{"DORMANT", "SUSPENDED", "DISSOLVED"} {
		t.Run(status, func(t *testing.T) {
			srv := fakeRegistry(t, http.StatusOK, entity.LegalEntity{
				LegalEntityID: entityID, TenantID: callerTenant, EntityStatus: status,
			})
			err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
			if !errors.Is(err, entity.ErrNotActive) {
				t.Fatalf("expected ErrNotActive for %s, got %v", status, err)
			}
		})
	}
}

// TestVerifyInTenant_UnknownStatus_Refused — the allow-list means a status added to
// the registry later stops invoicing until somebody decides it should not. A
// deny-list would have admitted it silently, which is the wrong default for an
// entity that might have been put beyond use.
func TestVerifyInTenant_UnknownStatus_Refused(t *testing.T) {
	srv := fakeRegistry(t, http.StatusOK, entity.LegalEntity{
		LegalEntityID: entityID, TenantID: callerTenant, EntityStatus: "LIQUIDATING",
	})

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrNotActive) {
		t.Fatalf("expected an unrecognised status to be refused, got %v", err)
	}
}

// TestVerifyInTenant_TenantCheckedBeforeStatus — an entity in another tenant must
// not be able to tell the caller anything about itself, including whether it is
// active. Both refuse, but the ordering decides which fact leaks.
func TestVerifyInTenant_TenantCheckedBeforeStatus(t *testing.T) {
	srv := fakeRegistry(t, http.StatusOK, entity.LegalEntity{
		LegalEntityID: entityID, TenantID: otherTenant, EntityStatus: "DISSOLVED",
	})

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrForeignTenant) {
		t.Fatalf("the tenant comparison must come first; got %v", err)
	}
}

func TestVerifyInTenant_RegistryUnreachable_FailsClosed(t *testing.T) {
	// A port nothing is listening on.
	err := entity.NewHTTPClient("http://127.0.0.1:1").VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable so the caller fails closed, got %v", err)
	}
}

func TestVerifyInTenant_RegistryErrors_FailClosed(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusForbidden} {
		srv := fakeRegistry(t, status, nil)
		err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
		if !errors.Is(err, entity.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable for status %d, got %v", status, err)
		}
	}
}

// TestVerifyInTenant_MalformedBody_FailsClosed — a body that cannot be parsed is
// NOT "the entity is fine". Reading an unparseable answer as acceptance is how a
// broken dependency becomes an unchecked write.
func TestVerifyInTenant_MalformedBody_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json"))
	}))
	defer srv.Close()

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for an unparseable body, got %v", err)
	}
}

// TestVerifyInTenant_EmptyTenantOnEntity_Refused — an entity whose tenant_id is
// absent from the response must not match a caller with no tenant either. The
// handler guarantees a non-empty tenant, but "" == "" would be the wrong answer if
// it ever did not.
func TestVerifyInTenant_EmptyTenantOnEntity_Refused(t *testing.T) {
	srv := fakeRegistry(t, http.StatusOK, map[string]any{
		"legal_entity_id": entityID, "entity_status": "ACTIVE",
	})

	err := entity.NewHTTPClient(srv.URL).VerifyInTenant(context.Background(), callerTenant, entityID)
	if !errors.Is(err, entity.ErrForeignTenant) {
		t.Fatalf("expected an entity with no tenant to be refused, got %v", err)
	}
}
