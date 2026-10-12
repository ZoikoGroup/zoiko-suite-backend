package context_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/domain"
)

// S1-3 / R-1: ResolveTenantContext is a GOV-01 query, "correlation required".

func TestResolveWithoutCorrelationIsRefused(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	body, _ := json.Marshal(domain.ResolveRequest{BearerToken: "mock-token", LegalEntityID: "01HXXXENTITYID"})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", bytes.NewBuffer(body))
	req.Host = "tenant-a.zoiko.io"
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, h.sessions.storedCtx, "no session may be stored untraceable")
}

func TestResolveTakesTheHeaderCorrelation(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	body, _ := json.Marshal(domain.ResolveRequest{BearerToken: "mock-token", LegalEntityID: "01HXXXENTITYID"})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", bytes.NewBuffer(body))
	req.Host = "tenant-a.zoiko.io"
	req.Header.Set("X-Correlation-ID", "corr-from-header")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, h.sessions.storedCtx, 1)
	for _, sc := range h.sessions.storedCtx {
		assert.Equal(t, "corr-from-header", sc.CorrelationID)
	}
}
