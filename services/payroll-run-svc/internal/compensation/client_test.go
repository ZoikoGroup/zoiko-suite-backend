package compensation_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/compensation"
)

func TestListApprovedBonuses(t *testing.T) {
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		_, _ = w.Write([]byte(`[{"grant_id":"g-1","employee_id":"w-1","bonus_type":"ANNUAL","amount":500.5,"currency":"GBP","grant_date":"2026-09-15","status":"APPROVED","approved_by":"mgr-1"}]`))
	}))
	defer srv.Close()

	bs, err := compensation.NewClient(srv.URL, nil).ListApprovedBonuses(context.Background(), "t-1", "p-1", "w-1")
	require.NoError(t, err)
	require.Len(t, bs, 1)
	assert.Equal(t, 500.5, bs[0].Amount)
	require.NotNil(t, bs[0].ApprovedBy)
	assert.Equal(t, "mgr-1", *bs[0].ApprovedBy)
	assert.Equal(t, "/v1/compensation/bonuses", got.URL.Path)
	assert.Equal(t, "w-1", got.URL.Query().Get("employee_id"))
	assert.Equal(t, "APPROVED", got.URL.Query().Get("status"))
	assert.Equal(t, "t-1", got.Header.Get("X-Tenant-Id"))
}

func TestListApprovedBonuses_AnyFailureIsAnErrorNeverAnEmptyList(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`[]`))
		}))
		_, err := compensation.NewClient(srv.URL, nil).ListApprovedBonuses(context.Background(), "t", "p", "w")
		srv.Close()
		assert.Error(t, err, "status %d must not read as 'no bonuses'", status)
	}
	_, err := compensation.NewClient("http://127.0.0.1:1", nil).ListApprovedBonuses(context.Background(), "t", "p", "w")
	assert.Error(t, err)
}
