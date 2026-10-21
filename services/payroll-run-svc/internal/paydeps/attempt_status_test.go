package paydeps

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func TestAttemptStatusReadsTheAdapterStatusAndNeverInventsOne(t *testing.T) {
	var path, tenant, principal, entity string
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		path, tenant, principal, entity = r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"), r.Header.Get("X-Legal-Entity-Id")
		_, _ = w.Write([]byte(`{"AttemptID":"att-1","Status":"PENDING_UNKNOWN"}`))
	})
	st, err := NewBankingHandoff(url, nil).AttemptStatus(context.Background(), "t-1", "svc", "le-1", "att-1")
	require.NoError(t, err)
	assert.Equal(t, "PENDING_UNKNOWN", st)
	assert.Equal(t, "/bnk06/attempts/att-1", path)
	assert.Equal(t, []string{"t-1", "svc", "le-1"}, []string{tenant, principal, entity})

	for name, h := range map[string]http.HandlerFunc{
		"not found": reply(404, ``), "forbidden": reply(403, `{}`), "server error": reply(503, ``), "garbage": reply(200, `nope`), "no status": reply(200, `{"AttemptID":"x"}`),
	} {
		_, err := NewBankingHandoff(server(t, h), nil).AttemptStatus(context.Background(), "t-1", "svc", "le-1", "att-1")
		assert.ErrorIs(t, err, domain.ErrHandoffUnavailable, name)
	}
	_, err = NewBankingHandoff("http://127.0.0.1:1", nil).AttemptStatus(context.Background(), "t-1", "svc", "le-1", "att-1")
	assert.ErrorIs(t, err, domain.ErrHandoffUnavailable, "unreachable is unknown, not a status")
}
