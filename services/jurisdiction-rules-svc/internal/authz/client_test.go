package authz_test

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/jurisdiction-rules-svc/internal/authz"
)

func TestNewClient_LocalEnvironment_UsesStub(t *testing.T) {
	log := zap.NewNop()
	client, err := authz.NewClient("local", "http://authorization-svc", log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected client, got nil")
	}
	if err := client.Authorize(context.Background(), "jwt", "res", "act"); err != nil {
		t.Errorf("expected stub to permit, got %v", err)
	}
}

func TestNewClient_ProductionEnvironment_PlaceholderURL_ReturnsError(t *testing.T) {
	log := zap.NewNop()
	_, err := authz.NewClient("production", "http://authorization-svc", log)
	if err == nil {
		t.Fatal("expected error when starting in production with placeholder URL, got nil")
	}
}

func TestNewClient_ProductionEnvironment_ValidURL_ReturnsHTTPClient(t *testing.T) {
	log := zap.NewNop()
	client, err := authz.NewClient("production", "http://real-authz.zoiko.internal", log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected client, got nil")
	}
}
