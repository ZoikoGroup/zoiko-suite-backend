package aggregator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
