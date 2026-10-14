package clients_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/employee-master-svc/internal/clients"
	"zoiko.io/employee-master-svc/internal/domain"
)

var caller = clients.CallerContext{TenantID: "t-1", PrincipalID: "p-1", LegalEntityID: "le-1", CorrelationID: "c-1"}

func trustServer(t *testing.T, status int, body string) (*clients.SourceTrust, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := clients.NewSourceTrust(srv.URL, nil)
	return c, &got
}

func TestSourceTrust_Check(t *testing.T) {
	active := `{"status":"ACTIVE","allowed_capabilities":["WORKER_PROFILE","TIME_LEAVE"]}`

	c, req := trustServer(t, 200, active)
	require.NoError(t, c.Check(context.Background(), caller, "hris/eu 1", "WORKER_PROFILE"))
	assert.Equal(t, "/v1/payroll/wfp/source-trust-profiles/by-source/hris%2Feu%201", req.URL.EscapedPath(), "the source id is path-escaped")
	assert.Equal(t, "t-1", req.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "p-1", req.Header.Get("X-Principal-Id"))
	assert.Equal(t, "le-1", req.Header.Get("X-Legal-Entity-Id"))

	c, _ = trustServer(t, 200, active)
	assert.ErrorIs(t, c.Check(context.Background(), caller, "hris", "PAYROLL_RESULT"), domain.ErrSourceNotTrusted, "trust is per capability")

	for name, body := range map[string]string{
		"suspended": `{"status":"SUSPENDED","allowed_capabilities":["WORKER_PROFILE"]}`,
		"pending":   `{"status":"PENDING_CERTIFICATION","allowed_capabilities":["WORKER_PROFILE"]}`,
	} {
		c, _ = trustServer(t, 200, body)
		assert.ErrorIs(t, c.Check(context.Background(), caller, "hris", "WORKER_PROFILE"), domain.ErrSourceNotTrusted, name)
	}

	c, _ = trustServer(t, 404, `{}`)
	assert.ErrorIs(t, c.Check(context.Background(), caller, "unknown", "WORKER_PROFILE"), domain.ErrSourceNotTrusted)

	for _, status := range []int{401, 403, 500, 503} {
		c, _ = trustServer(t, status, `{}`)
		assert.ErrorIs(t, c.Check(context.Background(), caller, "hris", "WORKER_PROFILE"), domain.ErrSourceTrustUnavailable, "status %d fails closed", status)
	}
	c, _ = trustServer(t, 200, `not json`)
	assert.ErrorIs(t, c.Check(context.Background(), caller, "hris", "WORKER_PROFILE"), domain.ErrSourceTrustUnavailable)

	down := clients.NewSourceTrust("http://127.0.0.1:1", nil)
	assert.ErrorIs(t, down.Check(context.Background(), caller, "hris", "WORKER_PROFILE"), domain.ErrSourceTrustUnavailable)
}

func jurServer(t *testing.T, status int, body string) *clients.Jurisdictions {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return clients.NewJurisdictions(srv.URL, nil)
}

func TestJurisdictions_Exists(t *testing.T) {
	ok, err := jurServer(t, 200, `{"jurisdiction_id":"x","active_flag":true}`).Exists(context.Background(), "x", "c")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = jurServer(t, 200, `{"jurisdiction_id":"x","active_flag":false}`).Exists(context.Background(), "x", "c")
	require.NoError(t, err)
	assert.False(t, ok, "a deactivated jurisdiction is not supported")

	ok, err = jurServer(t, 404, `{"error":"jurisdiction_not_found"}`).Exists(context.Background(), "x", "c")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = jurServer(t, 503, `{"error":"store_unavailable"}`).Exists(context.Background(), "x", "c")
	assert.ErrorIs(t, err, domain.ErrJurisdictionUnavailable, "503 means cannot answer, never no")
	_, err = jurServer(t, 200, `garbage`).Exists(context.Background(), "x", "c")
	assert.ErrorIs(t, err, domain.ErrJurisdictionUnavailable)
	_, err = clients.NewJurisdictions("http://127.0.0.1:1", nil).Exists(context.Background(), "x", "c")
	assert.ErrorIs(t, err, domain.ErrJurisdictionUnavailable)
}
