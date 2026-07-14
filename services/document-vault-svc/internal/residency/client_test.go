package residency_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/residency"
)

func TestCheckRegion_Match_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant_id":"t1","region_code":"eu","region_name":"EU"}`))
	}))
	defer srv.Close()

	v := residency.NewHTTPValidator(srv.URL, zap.NewNop())
	err := v.CheckRegion(context.Background(), "t1", "eu")
	require.NoError(t, err)
}

func TestCheckRegion_Mismatch_ReturnsErrMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant_id":"t1","region_code":"us","region_name":"US"}`))
	}))
	defer srv.Close()

	v := residency.NewHTTPValidator(srv.URL, zap.NewNop())
	err := v.CheckRegion(context.Background(), "t1", "eu")
	assert.ErrorIs(t, err, residency.ErrMismatch)
}

func TestCheckRegion_NonOKStatus_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	v := residency.NewHTTPValidator(srv.URL, zap.NewNop())
	err := v.CheckRegion(context.Background(), "t1", "eu")
	assert.ErrorIs(t, err, residency.ErrServiceUnavailable)
}

func TestCheckRegion_Unreachable_FailsClosed(t *testing.T) {
	v := residency.NewHTTPValidator("http://127.0.0.1:1", zap.NewNop())
	err := v.CheckRegion(context.Background(), "t1", "eu")
	assert.ErrorIs(t, err, residency.ErrServiceUnavailable)
}
