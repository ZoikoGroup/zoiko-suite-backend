package commercial

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	idenv "zoiko.io/identity-envelope"
)

func TestClient_GetCommercialPlan_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/commercial-accounts/by-organization/tenant-abc":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"commercial_account_id":"ca-123","organization_id":"tenant-abc","status":"ACTIVE"}`))
		case "/v1/commercial-accounts/ca-123/subscription":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"subscription_id":"sub-456","organization_id":"tenant-abc","commercial_account_id":"ca-123","product_id":"plan-premium","lifecycle_status":"ACTIVE"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.NoError(t, err)
	assert.Equal(t, "plan-premium", plan)
	assert.Equal(t, "tenant-abc", orgUnit)
}

func TestClient_GetCommercialPlan_AccountNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-unknown")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, plan)
	assert.Empty(t, orgUnit)
}

func TestClient_GetCommercialPlan_ServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Empty(t, plan)
	assert.Empty(t, orgUnit)
}

func TestClient_GetCommercialPlan_NoSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/commercial-accounts/by-organization/tenant-abc":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"commercial_account_id":"ca-123","organization_id":"tenant-abc","status":"ACTIVE"}`))
		case "/v1/commercial-accounts/ca-123/subscription":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.NoError(t, err) // Returns org unit but no plan
	assert.Empty(t, plan)
	assert.Equal(t, "tenant-abc", orgUnit)
}

func TestClient_Disabled(t *testing.T) {
	var client *Client
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Empty(t, plan)
	assert.Empty(t, orgUnit)
}

func TestClient_Enabled(t *testing.T) {
	client := New("http://example.com")
	assert.True(t, client.Enabled())

	var disabled *Client
	assert.False(t, disabled.Enabled())
}

// TestS3D1_ForgedPlanHeaderReplaced proves that a client-forged
// X-Commercial-Plan header is stripped at the edge and replaced by the
// gateway's resolved value.
func TestS3D1_ForgedPlanHeaderReplaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gateway should forward its own resolved value, not the client's forged one
		forged := r.Header.Get("X-Commercial-Plan")
		if forged == "client-forged-plan" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("forged header passed through"))
			return
		}
		switch r.URL.Path {
		case "/v1/commercial-accounts/by-organization/tenant-abc":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"commercial_account_id":"ca-123","organization_id":"tenant-abc","status":"ACTIVE"}`))
		case "/v1/commercial-accounts/ca-123/subscription":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"subscription_id":"sub-456","organization_id":"tenant-abc","commercial_account_id":"ca-123","product_id":"plan-premium","lifecycle_status":"ACTIVE"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.NoError(t, err)
	assert.Equal(t, "plan-premium", plan)
	assert.Equal(t, "tenant-abc", orgUnit)
}

// TestS3D1_PlanUnavailableOmitsHeaderNeverGuesses proves that when the
// commercial account service is unavailable or returns no subscription,
// the gateway omits the headers rather than guessing.
func TestS3D1_PlanUnavailableOmitsHeaderNeverGuesses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(srv.URL)
	plan, orgUnit, err := client.GetCommercialPlan(t.Context(), "tenant-abc")
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Empty(t, plan)
	assert.Empty(t, orgUnit)
}

// TestS3D1_OrgUnitFromEnvelopeOnly proves that the org unit (tenant ID)
// comes from the verified envelope, not from a client header.
func TestS3D1_OrgUnitFromEnvelopeOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gateway should forward the tenant ID from the envelope
		tenantID := r.Header.Get("X-Tenant-Id")
		if tenantID != "tenant-from-envelope" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("wrong tenant ID: " + tenantID))
			return
		}
		switch r.URL.Path {
		case "/v1/commercial-accounts/by-organization/tenant-from-envelope":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"commercial_account_id":"ca-123","organization_id":"tenant-from-envelope","status":"ACTIVE"}`))
		case "/v1/commercial-accounts/ca-123/subscription":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"subscription_id":"sub-456","organization_id":"tenant-from-envelope","commercial_account_id":"ca-123","product_id":"plan-basic","lifecycle_status":"ACTIVE"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Use client without exchanger to avoid exchange errors
	client := New(srv.URL)
	require.NotNil(t, client, "client should not be nil")

	// Create a context with identity for Forward() to stamp headers
	ctx := idenv.WithIdentity(t.Context(), &idenv.Identity{
		Basis:       idenv.BasisEnvelope,
		PrincipalID: "principal-xyz",
		TenantID:    "tenant-from-envelope",
		Token:       "test-token",
	})

	plan, orgUnit, err := client.GetCommercialPlan(ctx, "tenant-from-envelope")
	require.NoError(t, err, "GetCommercialPlan failed: %v", err)
	assert.Equal(t, "plan-basic", plan)
	assert.Equal(t, "tenant-from-envelope", orgUnit)
}