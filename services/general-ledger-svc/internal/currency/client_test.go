package currency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMinorUnitsReadsTheRegistryCachesAndFailsClosed(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		switch r.URL.Path {
		case "/v1/currencies/JPY":
			_, _ = w.Write([]byte(`{"code":"JPY","minor_unit":{"minor_unit":0}}`))
		case "/v1/currencies/XXX":
			_, _ = w.Write([]byte(`{"code":"XXX"}`))
		case "/v1/currencies/BAD":
			_, _ = w.Write([]byte(`{"minor_unit":{"minor_unit":9}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	asOf := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if u, err := c.MinorUnits(context.Background(), "JPY", asOf); err != nil || u != 0 {
			t.Fatalf("%d %v", u, err)
		}
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("answers are cached: %d requests", hits)
	}
	for _, code := range []string{"XXX", "BAD", "NOPE"} {
		if _, err := c.MinorUnits(context.Background(), code, asOf); err == nil {
			t.Errorf("%s: a currency with no usable minor unit must be an error, never a guess", code)
		}
	}
	srv.Close()
	if _, err := New(srv.URL).MinorUnits(context.Background(), "GBP", asOf); err == nil {
		t.Fatal("an unreachable registry must fail closed")
	}
}
