package fiscalcal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestHTTP_ExistsReadsUnderTheEntitysTenantAndFailsClosed(t *testing.T) {
	var gotPath, gotTenant string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant = r.URL.Path, r.Header.Get("X-Tenant-Id")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	v := NewHTTP(srv.URL, zap.NewNop())
	ctx := context.Background()

	ok, err := v.Exists(ctx, "tenant-a", "cal-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "/v1/fiscal-calendars/cal-1", gotPath)
	assert.Equal(t, "tenant-a", gotTenant)

	status = http.StatusNotFound
	ok, err = v.Exists(ctx, "tenant-a", "cal-1")
	require.NoError(t, err)
	assert.False(t, ok, "404 is an answer, not an outage")

	for _, s := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		status = s
		_, err = v.Exists(ctx, "tenant-a", "cal-1")
		assert.ErrorIs(t, err, ErrUnavailable, "status %d", s)
	}
	srv.Close()
	_, err = v.Exists(ctx, "tenant-a", "cal-1")
	assert.ErrorIs(t, err, ErrUnavailable)

	_, err = Unavailable{}.Exists(ctx, "t", "c")
	assert.ErrorIs(t, err, ErrUnavailable)
}
