package documentvault_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/documentvault"
	"zoiko.io/workflow-svc/internal/domain"
)

type documentSummary struct {
	DocumentID     string `json:"document_id"`
	TenantID       string `json:"tenant_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	CurrentVersion int    `json:"current_version"`
}

func TestHTTPClient_VerifyDocument_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/documents/doc-123" {
			t.Errorf("expected /v1/documents/doc-123, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(documentSummary{
			DocumentID:     "doc-123",
			TenantID:       "t-1",
			LegalEntityID:  "le-1",
			CurrentVersion: 5,
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	version, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err != nil {
		t.Errorf("VerifyDocument returned error: %v", err)
	}
	if version != 5 {
		t.Errorf("version = %d, want 5", version)
	}
}

func TestHTTPClient_VerifyDocument_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for not found")
	}
	if err != domain.ErrAuditAcceptanceEvidenceInvalid {
		t.Errorf("error = %v, want %v", err, domain.ErrAuditAcceptanceEvidenceInvalid)
	}
}

func TestHTTPClient_VerifyDocument_Unavailable(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient("http://localhost:9999", log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for unavailable")
	}
}

func TestHTTPClient_VerifyDocument_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for non-OK status")
	}
}

func TestHTTPClient_VerifyDocument_TenantMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(documentSummary{
			DocumentID:     "doc-123",
			TenantID:       "t-other", // mismatch
			LegalEntityID:  "le-1",
			CurrentVersion: 5,
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for tenant mismatch")
	}
	if err != domain.ErrAuditAcceptanceEvidenceInvalid {
		t.Errorf("error = %v, want %v", err, domain.ErrAuditAcceptanceEvidenceInvalid)
	}
}

func TestHTTPClient_VerifyDocument_LegalEntityMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(documentSummary{
			DocumentID:     "doc-123",
			TenantID:       "t-1",
			LegalEntityID:  "le-other", // mismatch
			CurrentVersion: 5,
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for legal entity mismatch")
	}
	if err != domain.ErrAuditAcceptanceEvidenceInvalid {
		t.Errorf("error = %v, want %v", err, domain.ErrAuditAcceptanceEvidenceInvalid)
	}
}

func TestHTTPClient_VerifyDocument_VersionZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(documentSummary{
			DocumentID:     "doc-123",
			TenantID:       "t-1",
			LegalEntityID:  "le-1",
			CurrentVersion: 0, // invalid
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err == nil {
		t.Errorf("expected error for version 0")
	}
	if err != domain.ErrAuditAcceptanceEvidenceInvalid {
		t.Errorf("error = %v, want %v", err, domain.ErrAuditAcceptanceEvidenceInvalid)
	}
}

func TestHTTPClient_VerifyDocument_SetsHeaders(t *testing.T) {
	var capturedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(documentSummary{
			DocumentID:     "doc-123",
			TenantID:       "t-1",
			LegalEntityID:  "le-1",
			CurrentVersion: 5,
		})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := documentvault.NewHTTPClient(server.URL, log)

	_, err := client.VerifyDocument(context.Background(), "t-1", "le-1", "doc-123", "p-1", "corr-1")
	if err != nil {
		t.Errorf("VerifyDocument returned error: %v", err)
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
	client := documentvault.NewHTTPClient("http://example.com", log)

	if client == nil {
		t.Errorf("NewHTTPClient returned nil")
	}
}
