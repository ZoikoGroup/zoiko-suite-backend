package decisionlog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/decisionlog"
)

// Matches the params sent by the client (fields have no JSON tags, so Go uses field names)
type recordDecisionRequest struct {
	TenantID                  string   `json:"TenantID"`
	LegalEntityID             string   `json:"LegalEntityID"`
	ActorPrincipalID          string   `json:"ActorPrincipalID"`
	ActionType                string   `json:"ActionType"`
	ResourceType              string   `json:"ResourceType"`
	ResourceID                string   `json:"ResourceID"`
	Outcome                   string   `json:"Outcome"`
	Rationale                 *string  `json:"Rationale,omitempty"`
	CorrelationID             string   `json:"CorrelationID"`
	CausationID               *string  `json:"CausationID,omitempty"`
	WorkflowType              string   `json:"WorkflowType"`
	WorkflowDefinitionID      *string  `json:"WorkflowDefinitionID,omitempty"`
	WorkflowDefinitionVersion *int     `json:"WorkflowDefinitionVersion,omitempty"`
	EvidenceRefs              []string `json:"EvidenceRefs,omitempty"`
	SubjectType               *string  `json:"SubjectType,omitempty"`
	SubjectID                 *string  `json:"SubjectID,omitempty"`
	SubjectVersion            *int     `json:"SubjectVersion,omitempty"`
	SubjectFingerprint        *string  `json:"SubjectFingerprint,omitempty"`
}

type recordDecisionResponse struct {
	DecisionID string `json:"decision_id"`
}

func TestHTTPClient_RecordDecision_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/decisions" {
			t.Errorf("expected /v1/decisions, got %s", r.URL.Path)
		}

		var req recordDecisionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode failed: %v", err)
		}
		if req.TenantID != "t-1" {
			t.Errorf("TenantID = %q, want t-1", req.TenantID)
		}
		if req.ActionType != "WORKFLOW_APPROVED" {
			t.Errorf("ActionType = %q, want WORKFLOW_APPROVED", req.ActionType)
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(recordDecisionResponse{DecisionID: "dec-123"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := decisionlog.NewHTTPClient(server.URL, log)

	decisionID, err := client.RecordDecision(context.Background(), decisionlog.RecordDecisionParams{
		TenantID:         "t-1",
		LegalEntityID:    "le-1",
		ActorPrincipalID: "p-1",
		ActionType:       "WORKFLOW_APPROVED",
		ResourceType:     "workflow_instance",
		ResourceID:       "wf-1",
		Outcome:          "APPROVED",
		CorrelationID:    "corr-1",
	})
	if err != nil {
		t.Errorf("RecordDecision returned error: %v", err)
	}
	if decisionID != "dec-123" {
		t.Errorf("DecisionID = %q, want dec-123", decisionID)
	}
}

func TestHTTPClient_RecordDecision_Unavailable(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := decisionlog.NewHTTPClient("http://localhost:9999", log)

	_, err := client.RecordDecision(context.Background(), decisionlog.RecordDecisionParams{
		TenantID:         "t-1",
		LegalEntityID:    "le-1",
		ActorPrincipalID: "p-1",
		ActionType:       "WORKFLOW_APPROVED",
		ResourceType:     "workflow_instance",
		ResourceID:       "wf-1",
		Outcome:          "APPROVED",
		CorrelationID:    "corr-1",
	})
	if err == nil {
		t.Errorf("expected error for unavailable")
	}
}

func TestHTTPClient_RecordDecision_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := decisionlog.NewHTTPClient(server.URL, log)

	_, err := client.RecordDecision(context.Background(), decisionlog.RecordDecisionParams{
		TenantID:         "t-1",
		LegalEntityID:    "le-1",
		ActorPrincipalID: "p-1",
		ActionType:       "WORKFLOW_APPROVED",
		ResourceType:     "workflow_instance",
		ResourceID:       "wf-1",
		Outcome:          "APPROVED",
		CorrelationID:    "corr-1",
	})
	if err == nil {
		t.Errorf("expected error for non-OK status")
	}
}

func TestHTTPClient_RecordDecision_SetsHeaders(t *testing.T) {
	var capturedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(recordDecisionResponse{DecisionID: "dec-123"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := decisionlog.NewHTTPClient(server.URL, log)

	_, err := client.RecordDecision(context.Background(), decisionlog.RecordDecisionParams{
		TenantID:         "t-1",
		LegalEntityID:    "le-1",
		ActorPrincipalID: "p-1",
		ActionType:       "WORKFLOW_APPROVED",
		ResourceType:     "workflow_instance",
		ResourceID:       "wf-1",
		Outcome:          "APPROVED",
		CorrelationID:    "corr-1",
		CausationID:      stringPtr("cause-1"),
	})
	if err != nil {
		t.Errorf("RecordDecision returned error: %v", err)
	}

	// Client sets these headers
	if capturedHeaders.Get("X-Tenant-Id") != "t-1" {
		t.Errorf("X-Tenant-Id not set")
	}
	if capturedHeaders.Get("X-Principal-Id") != "p-1" {
		t.Errorf("X-Principal-Id not set")
	}
	if capturedHeaders.Get("X-Correlation-ID") != "corr-1" {
		t.Errorf("X-Correlation-ID not set")
	}
	if capturedHeaders.Get("X-Causation-Id") != "cause-1" {
		t.Errorf("X-Causation-Id not set")
	}
}

func stringPtr(s string) *string {
	return &s
}

func TestNewHTTPClient(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := decisionlog.NewHTTPClient("http://example.com", log)

	if client == nil {
		t.Errorf("NewHTTPClient returned nil")
	}
}
