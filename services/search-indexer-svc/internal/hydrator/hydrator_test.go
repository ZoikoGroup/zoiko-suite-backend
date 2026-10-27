package hydrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
)

func callerCtx() context.Context {
	return envelope.WithEnvelope(context.Background(), envelope.Envelope{
		TenantID: "tenant-a", ActorSubjectID: "principal-1", LegalEntityID: "entity-1",
		RequestID: "req-1", CorrelationID: "corr-1", SourceChannel: "api",
		PurposeContext: "COMPLIANCE_REVIEW",
	})
}

// Rules 1–3 of the package comment, against a source that records what it got.
func TestHydrate_AsTheCallerAtTheConfiguredCollection(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_ = json.NewEncoder(w).Encode(map[string]any{"obligation_code": "GST"})
	}))
	defer srv.Close()

	h := New(map[string]string{"obligation": srv.URL + "/v1/obligations/"}, time.Second)
	obj, err := h.Hydrate(callerCtx(), "obligation", "ob-1", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, "GST", obj["obligation_code"])

	assert.Equal(t, "/v1/obligations/ob-1", got.URL.Path, "the plural collection, not a guessed /v1/{source_type}")
	assert.Equal(t, "principal-1", got.Header.Get("X-Principal-Id"), "the caller, never a service identity")
	assert.Empty(t, got.Header.Get("X-Workload-Id"), "no self-asserted workload")
	assert.Equal(t, "tenant-a", got.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "req-1", got.Header.Get("X-Request-Id"), "strict sources refuse a request without it")
	assert.Equal(t, "corr-1", got.Header.Get("X-Correlation-ID"))
	assert.Equal(t, "api", got.Header.Get("X-Source-Channel"))
	assert.Equal(t, "COMPLIANCE_REVIEW", got.Header.Get("X-Purpose-Context"))
	assert.Equal(t, "entity-1", got.Header.Get("X-Legal-Entity-Id"))
}

// NP-55: gone is a suppression, distinguishable from a failure.
func TestHydrate_NotFoundIsSourceGone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		h := New(map[string]string{"obligation": srv.URL}, time.Second)
		_, err := h.Hydrate(callerCtx(), "obligation", "ob-1", "tenant-a")
		assert.True(t, errors.Is(err, domain.ErrSourceGone), "status %d: %v", status, err)
		srv.Close()
	}
}

func TestHydrate_FailuresAreNotSourceGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	h := New(map[string]string{"obligation": srv.URL}, time.Second)
	_, err := h.Hydrate(callerCtx(), "obligation", "ob-1", "tenant-a")
	require.Error(t, err)
	assert.False(t, errors.Is(err, domain.ErrSourceGone))
}

func TestHydrate_RefusesWithoutACaller(t *testing.T) {
	h := New(map[string]string{"obligation": "http://unused"}, time.Second)
	_, err := h.Hydrate(context.Background(), "obligation", "ob-1", "tenant-a")
	assert.Error(t, err)
}

func TestHydrate_UnconfiguredSourceTypeFails(t *testing.T) {
	h := New(nil, time.Second)
	assert.False(t, h.Configured())
	_, err := h.Hydrate(callerCtx(), "obligation", "ob-1", "tenant-a")
	assert.Error(t, err)
}
