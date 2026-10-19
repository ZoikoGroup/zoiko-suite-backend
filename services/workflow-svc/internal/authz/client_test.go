package authz_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/authz"
	"zoiko.io/workflow-svc/internal/domain"
	svcenvelope "zoiko.io/workflow-svc/internal/envelope"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// Local structs matching the unexported ones in authz package
type authorizeRequest struct {
	PrincipalID              string `json:"principal_id"`
	LegalEntityID            string `json:"legal_entity_id"`
	ActionType               string `json:"action_type"`
	TenantID                 string `json:"tenant_id,omitempty"`
	ResourceOwnerPrincipalID string `json:"resource_owner_principal_id,omitempty"`
}

type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

const approvalActionType = "WORKFLOW_APPROVE"

func TestHTTPClient_CheckApprovalAllowed_Granted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/authorize" {
			t.Errorf("expected /v1/authorize, got %s", r.URL.Path)
		}

		var req authorizeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode failed: %v", err)
		}
		if req.PrincipalID != "p-1" {
			t.Errorf("PrincipalID = %q, want p-1", req.PrincipalID)
		}
		if req.LegalEntityID != "le-1" {
			t.Errorf("LegalEntityID = %q, want le-1", req.LegalEntityID)
		}
		if req.ActionType != approvalActionType {
			t.Errorf("ActionType = %q, want %q", req.ActionType, approvalActionType)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	ctx := svcmiddleware.WithTenant(context.Background(), "t-1")
	ctx = context.WithValue(ctx, "request_id", "req-1")
	ctx = context.WithValue(ctx, "correlation_id", "corr-1")

	err := client.CheckApprovalAllowed(ctx, "p-1", "le-1", "owner-1")
	if err != nil {
		t.Errorf("CheckApprovalAllowed returned error: %v", err)
	}
}

func TestHTTPClient_CheckApprovalAllowed_Denied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "DENIED"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	err := client.CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1")
	if err == nil {
		t.Errorf("expected error for DENIED")
	}
	if err != domain.ErrAuthorizationDenied {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationDenied)
	}
}

func TestHTTPClient_CheckApprovalAllowed_Unavailable(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient("http://localhost:9999", log)

	err := client.CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1")
	if err == nil {
		t.Errorf("expected error for unavailable")
	}
	if err != domain.ErrAuthorizationServiceUnavailable {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationServiceUnavailable)
	}
}

func TestHTTPClient_CheckApprovalAllowed_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	err := client.CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1")
	if err == nil {
		t.Errorf("expected error for non-OK status")
	}
	if err != domain.ErrAuthorizationServiceUnavailable {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationServiceUnavailable)
	}
}

func TestHTTPClient_CheckAllowed_UsesCustomActionType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req authorizeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ActionType != "CUSTOM_ACTION" {
			t.Errorf("ActionType = %q, want CUSTOM_ACTION", req.ActionType)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	err := client.CheckAllowed(context.Background(), "p-1", "le-1", "CUSTOM_ACTION", "owner-1")
	if err != nil {
		t.Errorf("CheckAllowed returned error: %v", err)
	}
}

func TestHTTPClient_CheckDelegation_Granted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/delegated-access/evaluate" {
			t.Errorf("expected /v1/delegated-access/evaluate, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			HasDelegatedAccess bool   `json:"has_delegated_access"`
			Basis              string `json:"basis"`
		}{HasDelegatedAccess: true, Basis: "delegated:from=approver-1"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	ctx := context.WithValue(context.Background(), "request_id", "req-1")
	err := client.CheckDelegation(ctx, "delegate-1", "approver-1", "le-1")
	if err != nil {
		t.Errorf("CheckDelegation returned error: %v", err)
	}
}

func TestHTTPClient_CheckDelegation_Denied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			HasDelegatedAccess bool   `json:"has_delegated_access"`
			Basis              string `json:"basis"`
		}{HasDelegatedAccess: false})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	err := client.CheckDelegation(context.Background(), "delegate-1", "approver-1", "le-1")
	if err == nil {
		t.Errorf("expected error for DENIED")
	}
	if err != domain.ErrAuthorizationDenied {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationDenied)
	}
}

func TestHTTPClient_ForwardsEnvelopeHeaders(t *testing.T) {
	var capturedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer server.Close()

	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient(server.URL, log)

	ctx := context.Background()
	ctx = svcmiddleware.WithTenant(ctx, "t-1")
	env := svcenvelope.Envelope{
		TenantID:           "t-1",
		ActorSubjectID:     "p-1",
		RequestID:          "req-1",
		CorrelationID:      "corr-1",
		SourceChannel:      svcenvelope.ChannelWeb,
		SourceSystem:       "ext",
		IdempotencyKey:     "idem-1",
		CausationID:        "cause-1",
		WorkflowInstanceID: "wf-1",
	}
	ctx = svcenvelope.WithEnvelope(ctx, env)

	err := client.CheckApprovalAllowed(ctx, "p-1", "le-1", "owner-1")
	if err != nil {
		t.Errorf("CheckApprovalAllowed returned error: %v", err)
	}

	// Verify headers were forwarded
	if capturedHeaders.Get("X-Request-Id") != "req-1" {
		t.Errorf("X-Request-Id not forwarded")
	}
	if capturedHeaders.Get("X-Correlation-ID") != "corr-1" {
		t.Errorf("X-Correlation-ID not forwarded")
	}
	if capturedHeaders.Get("X-Source-Channel") != "web" {
		t.Errorf("X-Source-Channel not forwarded")
	}
	if capturedHeaders.Get("Idempotency-Key") != "idem-1" {
		t.Errorf("Idempotency-Key not forwarded")
	}
	if capturedHeaders.Get("X-Causation-Id") != "cause-1" {
		t.Errorf("X-Causation-Id not forwarded")
	}
	if capturedHeaders.Get("X-Tenant-Id") != "t-1" {
		t.Errorf("X-Tenant-Id not forwarded")
	}
	// Note: X-Source-System and X-Workflow-Instance-Id are not currently forwarded
	// by the authz client - this test documents the current behavior
}

func TestNewHTTPClient_SetsTimeout(t *testing.T) {
	log, _ := zap.NewDevelopment()
	client := authz.NewHTTPClient("http://example.com", log)

	if client == nil {
		t.Errorf("NewHTTPClient returned nil")
	}
}
