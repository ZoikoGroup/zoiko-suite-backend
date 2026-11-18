package aggregator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	svcenvelope "zoiko.io/evidence-manifest-svc/internal/envelope"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// D2-2.F07: a range read follows next_cursor to the end (a manifest that
// snapshotted page one would LOOK complete), forwards the signed-in user, and
// carries the decision log's full read envelope with a purpose.
func TestGovernanceClient_RangeFollowsEveryPageWithTheFullEnvelope(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"records":[{"decision_id":"gd-1"},{"decision_id":"gd-2"}],"total_count":3,"next_cursor":"c-2"}`))
		case "c-2":
			_, _ = w.Write([]byte(`{"records":[{"decision_id":"gd-3"}],"total_count":3,"next_cursor":""}`))
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")
	ctx = svcmiddleware.WithPrincipal(ctx, "user-auditor")
	ctx = svcenvelope.WithEnvelope(ctx, svcenvelope.Envelope{RequestID: "req-1", CorrelationID: "corr-1", SourceChannel: "web"})
	recs, err := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop()).ListByEntityAndDateRange(ctx, "le-1", nil, nil)
	require.NoError(t, err)
	require.Len(t, recs, 3)
	assert.Equal(t, []string{"gd-1", "gd-2", "gd-3"}, []string{recs[0].SourceRecordID, recs[1].SourceRecordID, recs[2].SourceRecordID})
	assert.JSONEq(t, `{"decision_id":"gd-3"}`, string(recs[2].RawJSON))

	require.Len(t, seen, 2, "both pages read")
	for _, r := range seen {
		assert.Equal(t, "le-1", r.URL.Query().Get("entity"))
		assert.Equal(t, "200", r.URL.Query().Get("limit"))
		assert.Equal(t, "tenant-a", r.Header.Get("X-Tenant-Id"))
		assert.Equal(t, "user-auditor", r.Header.Get("X-Principal-Id"), "the signed-in user, not a service principal")
		assert.Equal(t, "req-1", r.Header.Get("X-Request-Id"))
		assert.Equal(t, "corr-1", r.Header.Get("X-Correlation-ID"))
		assert.Equal(t, "web", r.Header.Get("X-Source-Channel"))
		assert.Equal(t, aggregator.GovernancePurpose, r.Header.Get("X-Purpose-Context"))
	}
}

// A refusal or a malformed page fails the manifest: never a partial snapshot.
func TestGovernanceClient_RefusalsAndOldShapeFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"401 identity refused":    {http.StatusUnauthorized, `{}`},
		"403 read not granted":    {http.StatusForbidden, `{}`},
		"bare array (old shape)":  {http.StatusOK, `[{"decision_id":"gd-1"}]`},
		"record without an id":    {http.StatusOK, `{"records":[{"outcome":"GRANTED"}],"next_cursor":""}`},
		"second page refused 503": {http.StatusOK, `{"records":[{"decision_id":"gd-1"}],"next_cursor":"boom"}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "boom" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop()).ListByEntityAndDateRange(context.Background(), "le-1", nil, nil)
		assert.Error(t, err, name)
		srv.Close()
	}
}

// GetByID carries the same read envelope.
func TestGovernanceClient_GetByIDCarriesThePurpose(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = w.Write([]byte(`{"decision_id":"gd-1"}`))
	}))
	defer srv.Close()
	ctx := svcmiddleware.WithPrincipal(svcmiddleware.WithTenant(context.Background(), "tenant-a"), "user-1")
	_, err := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop()).GetByID(ctx, "gd-1")
	require.NoError(t, err)
	assert.Equal(t, aggregator.GovernancePurpose, got.Header.Get("X-Purpose-Context"))
	assert.Equal(t, "system", got.Header.Get("X-Source-Channel"))
	assert.NotEmpty(t, got.Header.Get("X-Request-Id"))
	assert.NotEmpty(t, got.Header.Get("X-Correlation-ID"))
	assert.Equal(t, "user-1", got.Header.Get("X-Principal-Id"))
}
