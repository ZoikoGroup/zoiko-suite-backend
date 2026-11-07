package upstream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/upstream"
)

// S1-B3: the registry answers a host lookup with a tenant (200), with its own 404 (not bound), or not at all. Only the
// first two are answers; everything else is an outage, never read as "unbound".
func TestResolveHost_DistinguishesAnAnswerFromAnOutage(t *testing.T) {
	var gotPath, gotQuery string
	status, body := http.StatusOK, `{"hostname":"a.zoiko.io","tenant_id":"tenant-a","lifecycle_state":"ACTIVE"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("hostname")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := clientFor(srv.URL)

	tenant, lifecycle, found, err := c.ResolveHost(context.Background(), " A.Zoiko.io ")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "tenant-a", tenant)
	assert.Equal(t, "ACTIVE", lifecycle)
	assert.Equal(t, "/v1/resolve-tenant", gotPath)
	assert.Equal(t, "a.zoiko.io", gotQuery, "the host is normalised before it is asked")

	status, body = http.StatusNotFound, `{}`
	_, _, found, err = c.ResolveHost(context.Background(), "x.zoiko.io")
	require.NoError(t, err)
	assert.False(t, found, "404 is the registry saying the host is not bound")

	for _, s := range []int{500, 503, 401} {
		status = s
		_, _, found, err = c.ResolveHost(context.Background(), "x.zoiko.io")
		assert.ErrorIs(t, err, upstream.ErrUpstreamRead, "status %d", s)
		assert.False(t, found)
	}
	status, body = http.StatusOK, `{"tenant_id":""}`
	_, _, _, err = c.ResolveHost(context.Background(), "x.zoiko.io")
	assert.ErrorIs(t, err, upstream.ErrUpstreamRead, "a 200 naming no tenant is not an answer")

	srv.Close()
	_, _, _, err = c.ResolveHost(context.Background(), "x.zoiko.io")
	assert.ErrorIs(t, err, upstream.ErrUpstreamRead)
}
