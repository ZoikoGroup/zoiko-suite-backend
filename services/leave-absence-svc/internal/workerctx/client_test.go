package workerctx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/workerctx"
)

var caller = workerctx.Caller{TenantID: "t-1", PrincipalID: "p-1", LegalEntityID: "le-1", CorrelationID: "c-1"}

func server(t *testing.T, status int, body string, capture *http.Request) *workerctx.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = *r.Clone(r.Context())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return workerctx.NewClient(srv.URL, nil)
}

func TestValidate_VerdictsAndForwardedContext(t *testing.T) {
	var got http.Request
	c := server(t, 200, `{"valid":true,"reasons":[]}`, &got)
	res, err := c.Validate(context.Background(), caller, "w-1", "le-1", "2026-09-30")
	require.NoError(t, err)
	assert.True(t, res.Valid)
	assert.Equal(t, "/v1/wfp/worker-financial-profiles/validate-context", got.URL.Path)
	assert.Equal(t, "w-1", got.URL.Query().Get("worker_id"))
	assert.Equal(t, "le-1", got.URL.Query().Get("legal_entity_id"))
	assert.Equal(t, "2026-09-30", got.URL.Query().Get("as_of"))
	assert.Equal(t, "t-1", got.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "p-1", got.Header.Get("X-Principal-Id"))
	assert.Equal(t, "le-1", got.Header.Get("X-Legal-Entity-Id"))

	c = server(t, 200, `{"valid":false,"reasons":["NO_ACTIVE_PROFILE","X"]}`, nil)
	res, err = c.Validate(context.Background(), caller, "w-1", "le-1", "2026-09-30")
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, []string{"NO_ACTIVE_PROFILE", "X"}, res.Reasons)
}

func TestValidate_FailsClosed(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500, 503} {
		c := server(t, status, `{"valid":true}`, nil)
		_, err := c.Validate(context.Background(), caller, "w", "le", "2026-09-30")
		assert.ErrorIs(t, err, domain.ErrWorkerContextUnavailable, "status %d must never read as valid", status)
	}
	c := server(t, 200, `not json`, nil)
	_, err := c.Validate(context.Background(), caller, "w", "le", "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrWorkerContextUnavailable)

	_, err = workerctx.NewClient("http://127.0.0.1:1", nil).Validate(context.Background(), caller, "w", "le", "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrWorkerContextUnavailable)
}
