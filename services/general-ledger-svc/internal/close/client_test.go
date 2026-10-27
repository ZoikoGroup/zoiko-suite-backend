package close

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
)

func TestHTTPClient_CheckPeriodOpen_PostingPolicy(t *testing.T) {
	tests := []struct {
		name           string
		postingPolicy  string
		expectedErr    error
	}{
		{
			name:          "OPEN policy allows posting",
			postingPolicy: "OPEN",
			expectedErr:   nil,
		},
		{
			name:          "REOPENED policy allows posting",
			postingPolicy: "REOPENED",
			expectedErr:   nil,
		},
		{
			name:           "RESTRICTED policy requires soft-close override",
			postingPolicy:  "RESTRICTED",
			expectedErr:    domain.ErrSoftCloseOverrideRequired,
		},
		{
			name:           "CLOSE_JOURNALS_ONLY policy treated as hard closed",
			postingPolicy:  "CLOSE_JOURNALS_ONLY",
			expectedErr:    domain.ErrPeriodHardClosed,
		},
		{
			name:           "CLOSED policy refuses posting",
			postingPolicy:  "CLOSED",
			expectedErr:    domain.ErrPeriodHardClosed,
		},
		{
			name:           "unknown policy fails closed",
			postingPolicy:  "SOME_UNKNOWN_VALUE",
			expectedErr:    domain.ErrPeriodHardClosed,
		},
		{
			name:           "empty policy fails closed",
			postingPolicy:  "",
			expectedErr:    domain.ErrPeriodHardClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				body := `{"close_status":"OPEN","period_state":"OPEN","posting_policy":"` + tt.postingPolicy + `"}`
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			client := NewHTTPClient(srv.URL, zap.NewNop())
			err := client.CheckPeriodOpen(context.Background(), "tenant1", "le1", "2026-10")

			if tt.expectedErr == nil {
				if err != nil {
					t.Errorf("expected nil error, got %v", err)
				}
			} else {
				if !errors.Is(err, tt.expectedErr) {
					t.Errorf("expected error %v, got %v", tt.expectedErr, err)
				}
			}
		})
	}
}

func TestHTTPClient_CheckPeriodOpen_404_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, zap.NewNop())
	err := client.CheckPeriodOpen(context.Background(), "tenant1", "le1", "2026-10")

	if err != nil {
		t.Errorf("expected nil for 404, got %v", err)
	}
}

func TestHTTPClient_CheckPeriodOpen_Non200Non404_ReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, zap.NewNop())
	err := client.CheckPeriodOpen(context.Background(), "tenant1", "le1", "2026-10")

	if !errors.Is(err, domain.ErrCloseServiceUnavailable) {
		t.Errorf("expected ErrCloseServiceUnavailable for 500, got %v", err)
	}
}

func TestHTTPClient_CheckPeriodOpen_MalformedJSON_ReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, zap.NewNop())
	err := client.CheckPeriodOpen(context.Background(), "tenant1", "le1", "2026-10")

	if !errors.Is(err, domain.ErrCloseServiceUnavailable) {
		t.Errorf("expected ErrCloseServiceUnavailable for malformed JSON, got %v", err)
	}
}

func TestHTTPClient_CheckPeriodOpen_Unreachable_ReturnsUnavailable(t *testing.T) {
	// Use a port nothing is listening on
	client := NewHTTPClient("http://127.0.0.1:1", zap.NewNop())
	err := client.CheckPeriodOpen(context.Background(), "tenant1", "le1", "2026-10")

	if !errors.Is(err, domain.ErrCloseServiceUnavailable) {
		t.Errorf("expected ErrCloseServiceUnavailable for unreachable server, got %v", err)
	}
}