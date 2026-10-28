package financialcontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func dateToleranceClient(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, zap.NewNop())
}

func TestGetActiveDateTolerance(t *testing.T) {
	ctx := context.Background()
	get := func(body string) error {
		_, err := dateToleranceClient(t, body).GetActiveDateTolerance(ctx, "t", "p", "le", "M")
		return err
	}

	tol, err := dateToleranceClient(t, `{"items":[{"tolerance_version":1,"absolute_tolerance":"0","date_tolerance_days":1,"date_basis":"CALENDAR"},`+
		`{"tolerance_version":2,"absolute_tolerance":"0","date_tolerance_days":3,"date_basis":"CALENDAR"}]}`).
		GetActiveDateTolerance(ctx, "t", "p", "le", "M")
	if err != nil || tol.Days != 3 || tol.Version != 2 {
		t.Fatalf("expected the latest version (3 days, v2), got %+v, %v", tol, err)
	}
	if err := get(`{"items":[]}`); !errors.Is(err, ErrToleranceNotConfigured) {
		t.Fatalf("no policy: expected ErrToleranceNotConfigured, got %v", err)
	}
	if err := get(`{"items":[{"tolerance_version":1,"absolute_tolerance":"0","date_tolerance_days":3,"date_basis":"BUSINESS"}]}`); !errors.Is(err, ErrUnsupportedDateBasis) {
		t.Fatalf("BUSINESS basis: expected ErrUnsupportedDateBasis, got %v", err)
	}
	for name, body := range map[string]string{
		"days missing":  `{"items":[{"tolerance_version":1,"absolute_tolerance":"0","date_basis":"CALENDAR"}]}`,
		"days negative": `{"items":[{"tolerance_version":1,"absolute_tolerance":"0","date_tolerance_days":-1,"date_basis":"CALENDAR"}]}`,
		"basis unknown": `{"items":[{"tolerance_version":1,"absolute_tolerance":"0","date_tolerance_days":1,"date_basis":"LUNAR"}]}`,
		"no version":    `{"items":[{"absolute_tolerance":"0","date_tolerance_days":1,"date_basis":"CALENDAR"}]}`,
	} {
		if err := get(body); !errors.Is(err, ErrServiceUnavailable) {
			t.Errorf("%s: expected fail-closed ErrServiceUnavailable, got %v", name, err)
		}
	}
}
