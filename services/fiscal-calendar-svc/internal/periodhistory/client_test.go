package periodhistory_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/fiscal-calendar-svc/internal/periodhistory"
)

func TestCalendarUsage_Ok(t *testing.T) {
	var gotPath, gotTenant, gotEntity, gotWorkload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotTenant, gotEntity, gotWorkload = r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Legal-Entity-Id"), r.Header.Get("X-Workload-Id")
		_, _ = w.Write([]byte(`{"latest_period_end":"2026-06-30","has_posted_or_closed_periods":true}`))
	}))
	defer srv.Close()

	h, err := periodhistory.New(srv.URL+"/", nil).CalendarUsage(context.Background(), "tenant-a", "entity-1", "cal-1")
	require.NoError(t, err)
	assert.True(t, h.HasPostedOrClosedPeriods)
	assert.Equal(t, "2026-06-30", h.LatestPeriodEnd.String())
	assert.Equal(t, "/v1/calendar-usage?calendar_id=cal-1", gotPath)
	assert.Equal(t, "tenant-a", gotTenant)
	assert.Equal(t, "entity-1", gotEntity)
	assert.Equal(t, "fiscal-calendar-svc", gotWorkload)
}

func TestCalendarUsage_NullEndAndNothingPosted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"latest_period_end":null,"has_posted_or_closed_periods":false}`))
	}))
	defer srv.Close()
	h, err := periodhistory.New(srv.URL, nil).CalendarUsage(context.Background(), "t", "e", "c")
	require.NoError(t, err)
	assert.False(t, h.HasPostedOrClosedPeriods)
	assert.Nil(t, h.LatestPeriodEnd)
}

func TestCalendarUsage_ErrorsAreErrors(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"500":      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"404":      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
		"not json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("nope")) },
		"bad date": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"latest_period_end":"06/30/2026","has_posted_or_closed_periods":true}`))
		},
	}
	for name, hf := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(hf)
			defer srv.Close()
			_, err := periodhistory.New(srv.URL, nil).CalendarUsage(context.Background(), "t", "e", "c")
			assert.Error(t, err)
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := periodhistory.New(url, nil).CalendarUsage(context.Background(), "t", "e", "c")
		assert.Error(t, err)
	})
}
