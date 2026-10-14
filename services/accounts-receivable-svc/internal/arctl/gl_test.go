package arctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

func glServer(t *testing.T, mappingStatus int, popBody string, popStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/account-mappings/AR_RECEIVABLE_STANDARD":
			w.WriteHeader(mappingStatus)
			_, _ = w.Write([]byte(`{"mapping_key":"AR_RECEIVABLE_STANDARD","account_code":"1200"}`))
		case "/v1/control-populations/account-postings":
			q := r.URL.Query()
			if q.Get("account_codes") != "1200" || q.Get("normal_balance") != "DEBIT" || q.Get("period_id") != "2026-09" || q.Get("limit") != "1" || q.Get("legal_entity_id") != "e1" {
				w.WriteHeader(400)
				return
			}
			if r.Header.Get("X-Tenant-Id") != "t1" || r.Header.Get("X-Principal-Id") != "svc" {
				w.WriteHeader(401)
				return
			}
			w.WriteHeader(popStatus)
			_, _ = w.Write([]byte(popBody))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestHTTPGLBalance(t *testing.T) {
	caller := svcclient.Caller{TenantID: "t1", PrincipalID: "svc"}
	good := `{"records":[],"next_cursor":"","watermark":"wm-9","declared_totals":{"row_count":3,"totals":{"USD":"1234.5600","EUR":"0.10"}}}`
	cases := []struct {
		name      string
		mapStatus int
		body      string
		status    int
		available bool
	}{
		{"ok", 200, good, 200, true},
		{"mapping missing", 404, good, 200, false},
		{"population 503", 200, good, 503, false},
		{"no watermark", 200, `{"declared_totals":{"row_count":0,"totals":{}}}`, 200, false},
		{"no declared totals", 200, `{"watermark":"x"}`, 200, false},
		{"garbage", 200, `<html>`, 200, false},
		{"non-decimal total", 200, `{"watermark":"x","declared_totals":{"row_count":1,"totals":{"USD":"1e3"}}}`, 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := glServer(t, tc.mapStatus, tc.body, tc.status)
			defer srv.Close()
			got, err := NewHTTPGLBalance(srv.URL).ControlAccountBalance(context.Background(), caller, "e1", "2026-09", []string{MappingARReceivable})
			require.NoError(t, err)
			require.Equal(t, tc.available, got.Available, got.Error)
			if tc.available {
				require.Equal(t, "1234.5600", got.Totals["USD"].String())
				require.Equal(t, "0.1000", got.Totals["EUR"].String())
				require.Equal(t, "wm-9", got.Watermark)
				require.Equal(t, []string{"1200"}, got.AccountCodes)
			} else {
				require.NotEmpty(t, got.Error)
			}
		})
	}
}

// An unreachable general ledger is data (UNVERIFIED), not a Go error and not a pass.
func TestHTTPGLBalance_Unreachable(t *testing.T) {
	srv := glServer(t, 200, "", 200)
	url := srv.URL
	srv.Close()
	got, err := NewHTTPGLBalance(url).ControlAccountBalance(context.Background(), svcclient.Caller{TenantID: "t1"}, "e1", "2026-09", []string{MappingARReceivable})
	require.NoError(t, err)
	require.False(t, got.Available)
	require.NotEmpty(t, got.Error)
}
