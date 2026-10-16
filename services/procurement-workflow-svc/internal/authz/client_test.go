package authz_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/procurement-workflow-svc/internal/authz"
	"zoiko.io/procurement-workflow-svc/internal/domain"
)

func TestAuthZClient_CheckAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer server.Close()

	client := authz.NewClient(server.URL, server.Client(), zap.NewNop())
	if err := client.CheckAllowed(context.Background(), "user-1", "le-1", "PROCUREMENT_CASE_CREATE"); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestAuthZClient_CheckDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_outcome":"DENIED"}`))
	}))
	defer server.Close()

	client := authz.NewClient(server.URL, server.Client(), zap.NewNop())
	err := client.CheckAllowed(context.Background(), "user-1", "le-1", "PROCUREMENT_CASE_CREATE")
	if err != domain.ErrAuthorizationDenied {
		t.Fatalf("expected ErrAuthorizationDenied, got %v", err)
	}
}
