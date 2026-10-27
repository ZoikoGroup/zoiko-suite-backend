package aggregator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	"zoiko.io/evidence-manifest-svc/internal/domain"
)

func TestGovernanceDecisionClient_ListByEntityAndDateRange_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "entity-1", r.URL.Query().Get("entity"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"decision_id":"gd-1","outcome":"GRANTED"},{"decision_id":"gd-2","outcome":"DENIED"}]`))
	}))
	defer srv.Close()

	c := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop())
	recs, err := c.ListByEntityAndDateRange(context.Background(), "entity-1", nil, nil)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	assert.Equal(t, domain.SourceGovernanceDecision, recs[0].SourceType)
	assert.Equal(t, "gd-1", recs[0].SourceRecordID)
	assert.Equal(t, "gd-2", recs[1].SourceRecordID)
	assert.JSONEq(t, `{"decision_id":"gd-1","outcome":"GRANTED"}`, string(recs[0].RawJSON))
}

func TestGovernanceDecisionClient_ListByEntityAndDateRange_Unreachable_FailsClosed(t *testing.T) {
	c := aggregator.NewGovernanceDecisionClient("http://127.0.0.1:1", zap.NewNop())
	_, err := c.ListByEntityAndDateRange(context.Background(), "entity-1", nil, nil)
	assert.ErrorIs(t, err, aggregator.ErrSourceUnavailable)
}

func TestWorkflowHistoryClient_ListByInstanceID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/workflows/wf-1/history", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"event_id":"ev-1","event_type":"STAGE_ADVANCED"},{"event_id":"ev-2","event_type":"COMPLETED"}]`))
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	recs, err := c.ListByInstanceID(context.Background(), "wf-1")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	assert.Equal(t, domain.SourceWorkflowHistory, recs[0].SourceType)
	assert.Equal(t, "ev-1", recs[0].SourceRecordID)
	assert.Equal(t, "ev-2", recs[1].SourceRecordID)
}

func TestWorkflowHistoryClient_ListByInstanceID_NotFound_IsEmptyNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	recs, err := c.ListByInstanceID(context.Background(), "wf-missing")
	require.NoError(t, err)
	assert.Empty(t, recs)
}

// TestWorkflowHistoryClient_ListByEntityAndDateRange_Success covers the
// method added to close the interface/implementation mismatch found this
// pass: handler.go's WorkflowHistorySource interface (and its real caller in
// collectRecords) required ListByEntityAndDateRange, but WorkflowHistoryClient
// never implemented it — the package did not even compile before this fix.
func TestWorkflowHistoryClient_ListByEntityAndDateRange_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/workflows/history", r.URL.Path)
		assert.Equal(t, "entity-1", r.URL.Query().Get("legal_entity_id"))
		assert.Equal(t, "2026-01-01T00:00:00Z", r.URL.Query().Get("from"))
		assert.Equal(t, "2026-02-01T00:00:00Z", r.URL.Query().Get("to"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"event_id":"ev-3","legal_entity_id":"entity-1"}]`))
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	recs, err := c.ListByEntityAndDateRange(context.Background(), "entity-1", from, to)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, domain.SourceWorkflowHistory, recs[0].SourceType)
	assert.Equal(t, "ev-3", recs[0].SourceRecordID)
	assert.JSONEq(t, `{"event_id":"ev-3","legal_entity_id":"entity-1"}`, string(recs[0].RawJSON))
}

func TestWorkflowHistoryClient_ListByEntityAndDateRange_Unreachable_FailsClosed(t *testing.T) {
	c := aggregator.NewWorkflowHistoryClient("http://127.0.0.1:1", zap.NewNop())
	_, err := c.ListByEntityAndDateRange(context.Background(), "entity-1", time.Now(), time.Now())
	assert.ErrorIs(t, err, aggregator.ErrSourceUnavailable)
}

func TestWorkflowHistoryClient_ListByEntityAndDateRange_NonOKStatus_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	_, err := c.ListByEntityAndDateRange(context.Background(), "entity-1", time.Now(), time.Now())
	assert.ErrorIs(t, err, aggregator.ErrSourceUnavailable)
}

func TestAccessDecisionClient_GetByID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/access-decisions/ad-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_decision_id":"ad-1","decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	c := aggregator.NewAccessDecisionClient(srv.URL, zap.NewNop())
	rec, err := c.GetByID(context.Background(), "ad-1")
	require.NoError(t, err)
	assert.Equal(t, domain.SourceAccessDecision, rec.SourceType)
	assert.Equal(t, "ad-1", rec.SourceRecordID)
}

func TestAccessDecisionClient_GetByID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := aggregator.NewAccessDecisionClient(srv.URL, zap.NewNop())
	_, err := c.GetByID(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, aggregator.ErrSourceNotFound)
}

func TestAccessDecisionClient_GetByID_Unreachable_FailsClosed(t *testing.T) {
	c := aggregator.NewAccessDecisionClient("http://127.0.0.1:1", zap.NewNop())
	_, err := c.GetByID(context.Background(), "ad-1")
	assert.ErrorIs(t, err, aggregator.ErrSourceUnavailable)
}

func TestWorkflowClient_GetByID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/workflows/wf-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_instance":{"workflow_instance_id":"wf-1"},"stages":[]}`))
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowClient(srv.URL, zap.NewNop())
	rec, err := c.GetByID(context.Background(), "wf-1")
	require.NoError(t, err)
	assert.Equal(t, domain.SourceWorkflowInstance, rec.SourceType)
}

func TestWorkflowClient_GetByID_ServerError_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowClient(srv.URL, zap.NewNop())
	_, err := c.GetByID(context.Background(), "wf-1")
	assert.ErrorIs(t, err, aggregator.ErrSourceUnavailable)
}
