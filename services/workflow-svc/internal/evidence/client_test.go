package evidence_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/evidence"
)

type evidenceSummary struct {
	EvidenceID  string `json:"evidence_id"`
	TenantID    string `json:"tenant_id"`
	StatusFlags struct {
		Contradictory bool `json:"contradictory"`
	} `json:"status_flags"`
}

func TestHTTPClient_GetContradictionFlag_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/audit-evidence/ev-123" {
			t.Errorf("expected /v1/audit-evidence/ev-123, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(evidenceSummary{
			EvidenceID: "ev-123",
			TenantID:   "t-1",
			StatusFlags: struct {
				Contradictory bool `json:"contradictory"`
			}{Contradictory: true},
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	flag, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err != nil {
		t.Errorf("GetContradictionFlag returned error: %v", err)
	}
	if !flag {
		t.Errorf("flag = false, want true")
	}
}

func TestHTTPClient_GetContradictionFlag_False(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(evidenceSummary{
			EvidenceID: "ev-123",
			TenantID:   "t-1",
			StatusFlags: struct {
				Contradictory bool `json:"contradictory"`
			}{Contradictory: false},
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	flag, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err != nil {
		t.Errorf("GetContradictionFlag returned error: %v", err)
	}
	if flag {
		t.Errorf("flag = true, want false")
	}
}

func TestHTTPClient_GetContradictionFlag_Unavailable(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient("http://localhost:9999", log)

	_, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for unavailable")
	}
}

func TestHTTPClient_GetContradictionFlag_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	_, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for non-OK status")
	}
}

func TestHTTPClient_GetContradictionFlag_TenantMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(evidenceSummary{
			EvidenceID: "ev-123",
			TenantID:   "t-other", // mismatch
			StatusFlags: struct {
				Contradictory bool `json:"contradictory"`
			}{Contradictory: true},
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	_, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for tenant mismatch")
	}
}

func TestHTTPClient_GetContradictionFlag_EvidenceIDMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(evidenceSummary{
			EvidenceID: "ev-other", // mismatch
			TenantID:   "t-1",
			StatusFlags: struct {
				Contradictory bool `json:"contradictory"`
			}{Contradictory: true},
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	_, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for evidence ID mismatch")
	}
}

func TestHTTPClient_GetContradictionFlag_SetsHeaders(t *testing.T) {
	var capturedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(evidenceSummary{
			EvidenceID: "ev-123",
			TenantID:   "t-1",
			StatusFlags: struct {
				Contradictory bool `json:"contradictory"`
			}{Contradictory: false},
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient(server.URL, log)

	_, err := client.GetContradictionFlag(context.Background(), "t-1", "ev-123", "p-1", "corr-1")
	if err != nil {
		t.Errorf("GetContradictionFlag returned error: %v", err)
	}

	if capturedHeaders.Get("X-Tenant-Id") != "t-1" {
		t.Errorf("X-Tenant-Id not set")
	}
	if capturedHeaders.Get("X-Principal-Id") != "p-1" {
		t.Errorf("X-Principal-Id not set")
	}
	if capturedHeaders.Get("X-Correlation-ID") != "corr-1" {
		t.Errorf("X-Correlation-ID not set")
	}
}

func TestNewHTTPClient(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := evidence.NewHTTPClient("http://example.com", log)

	if client == nil {
		t.Errorf("NewHTTPClient returned nil")
	}
}
