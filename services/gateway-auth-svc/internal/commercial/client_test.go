package commercial

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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