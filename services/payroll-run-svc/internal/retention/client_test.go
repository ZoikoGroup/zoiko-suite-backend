package retention_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/retention"
)

var caller = retention.Caller{TenantID: "11111111-1111-1111-1111-111111111111", PrincipalID: "p-1", CorrelationID: "corr-1"}

func TestResolve_ForwardsTheCallerAndParsesTheTwoFindings(t *testing.T) {
	var gotTenant, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, gotQuery = r.Header.Get("X-Tenant-Id"), r.URL.RawQuery
		assert.Equal(t, "/v1/retention/resolve", r.URL.Path)
		_, _ = w.Write([]byte(`{"blocked":true,"matched_hold":{"legal_hold_id":"h-1","authority":"court"},"applicable_policy":{"retention_policy_id":"p-1","min_retention_days":2555}}`))
	}))
	defer srv.Close()
	res, err := retention.NewClient(srv.URL, nil).Resolve(context.Background(), caller, "WFP_PAYROLL_RESULT", "GB", "le-1")
	require.NoError(t, err)
	assert.True(t, res.Blocked)
	assert.Equal(t, "h-1", res.Hold.ID)
	assert.Equal(t, 2555, res.Policy.MinRetentionDays)
	assert.Equal(t, caller.TenantID, gotTenant)
	assert.Contains(t, gotQuery, "record_class=WFP_PAYROLL_RESULT")
	assert.Contains(t, gotQuery, "jurisdiction_code=GB")
	assert.Contains(t, gotQuery, "entity_ref=le-1")
}

func TestResolve_FailsClosedOnEveryUnreadableAnswer(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"server error":           func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"forbidden":              func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"bad request":            func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
		"not json":               func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) },
		"blocked without a hold": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"blocked":true}`)) },
	} {
		srv := httptest.NewServer(h)
		_, err := retention.NewClient(srv.URL, nil).Resolve(context.Background(), caller, "WFP_PAYROLL_RESULT", "", "")
		assert.ErrorIs(t, err, domain.ErrRetentionUnavailable, name)
		srv.Close()
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening
	_, err := retention.NewClient(url, nil).Resolve(context.Background(), caller, "WFP_PAYROLL_RESULT", "", "")
	assert.ErrorIs(t, err, domain.ErrRetentionUnavailable, "unreachable")
}
