package authz_test

import (
	"context"
	"encoding/json"
	"errors"
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
	PrincipalID              string            `json:"principal_id"`
	LegalEntityID            string            `json:"legal_entity_id"`
	ActionType               string            `json:"action_type"`
	TenantID                 string            `json:"tenant_id,omitempty"`
	ResourceOwnerPrincipalID string            `json:"resource_owner_principal_id,omitempty"`
	Attributes               map[string]string `json:"attributes,omitempty"`
}

type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	AccessDecisionID string `json:"access_decision_id,omitempty"`
	DecisionBasis    string `json:"decision_basis,omitempty"`
}

func newClient(url string) *authz.HTTPClient {
	return authz.NewHTTPClient(url, zap.NewNop())
}

func TestHTTPClient_CheckApprovalAllowed_Granted(t *testing.T) {
	var got authorizeRequest
	var tenantHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/authorize" {
			t.Errorf("got %s %s, want POST /v1/authorize", r.Method, r.URL.Path)
		}
		tenantHeader = r.Header.Get("X-Tenant-Id")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED", AccessDecisionID: "ad-1", DecisionBasis: "rbac:role=APPROVER"})
	}))
	defer server.Close()

	ctx := svcmiddleware.WithTenant(context.Background(), "t-1")
	auth, err := newClient(server.URL).CheckApprovalAllowed(ctx, "p-1", "le-1", "owner-1",
		map[string]string{authz.AttrWorkflowType: "PAYMENT", authz.AttrApproverRole: "FINANCE"})
	if err != nil {
		t.Fatalf("CheckApprovalAllowed returned error: %v", err)
	}
	if got.PrincipalID != "p-1" || got.LegalEntityID != "le-1" || got.ActionType != authz.ApprovalAction || got.ResourceOwnerPrincipalID != "owner-1" {
		t.Errorf("request = %+v", got)
	}
	// The tenant used to be read from a context key nothing set, so the
	// question was asked outside any tenant.
	if got.TenantID != "t-1" || tenantHeader != "t-1" {
		t.Errorf("tenant body=%q header=%q, want t-1", got.TenantID, tenantHeader)
	}
	if got.Attributes[authz.AttrWorkflowType] != "PAYMENT" || got.Attributes[authz.AttrApproverRole] != "FINANCE" {
		t.Errorf("attributes = %v", got.Attributes)
	}
	if auth.AccessDecisionID != "ad-1" || auth.Basis != "rbac:role=APPROVER" {
		t.Errorf("authority = %+v, want the decision id and basis recorded", auth)
	}
}

func TestHTTPClient_CheckApprovalAllowed_Denied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "DENIED"})
	}))
	defer server.Close()

	_, err := newClient(server.URL).CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1", nil)
	if !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationDenied)
	}
}

// STEP_UP_MFA and every other non-GRANTED outcome deny: the client never
// treats an outcome it does not recognise as permission.
func TestHTTPClient_CheckApprovalAllowed_StepUpIsNotAGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "STEP_UP_MFA"})
	}))
	defer server.Close()

	if _, err := newClient(server.URL).CheckApprovalAllowed(context.Background(), "p-1", "le-1", "", nil); !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Errorf("error = %v, want denied", err)
	}
}

func TestHTTPClient_CheckApprovalAllowed_Unavailable(t *testing.T) {
	_, err := newClient("http://localhost:9999").CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1", nil)
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationServiceUnavailable)
	}
}

func TestHTTPClient_CheckApprovalAllowed_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := newClient(server.URL).CheckApprovalAllowed(context.Background(), "p-1", "le-1", "owner-1", nil)
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
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
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer server.Close()

	if err := newClient(server.URL).CheckAllowed(context.Background(), "p-1", "le-1", "CUSTOM_ACTION", "owner-1"); err != nil {
		t.Errorf("CheckAllowed returned error: %v", err)
	}
}

type delegationAnswer struct {
	HasDelegatedAccess bool   `json:"has_delegated_access"`
	Basis              string `json:"basis"`
	DelegationID       string `json:"delegation_id,omitempty"`
}

func delegationServer(t *testing.T, answer delegationAnswer, seen *map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/delegated-access/evaluate" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if seen != nil {
			_ = json.NewDecoder(r.Body).Decode(seen)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
}

// Governance audit row 23: the question names the approver as the delegator
// and asks about WORKFLOW_APPROVE (it asked about WORKFLOW_DELEGATE, which no
// delegation confers), and the answer's delegation id is kept for lineage.
func TestHTTPClient_CheckDelegation_AsksAboutTheNamedApprover(t *testing.T) {
	var seen map[string]string
	server := delegationServer(t, delegationAnswer{HasDelegatedAccess: true, Basis: "delegated:from=approver-1", DelegationID: "d-1"}, &seen)
	defer server.Close()

	ctx := svcmiddleware.WithTenant(context.Background(), "t-1")
	auth, err := newClient(server.URL).CheckDelegation(ctx, "delegate-1", "approver-1", "le-1")
	if err != nil {
		t.Fatalf("CheckDelegation returned error: %v", err)
	}
	if seen["delegator_principal_id"] != "approver-1" || seen["action_type"] != authz.ApprovalAction || seen["tenant_id"] != "t-1" {
		t.Errorf("request = %v", seen)
	}
	if auth.DelegationID != "d-1" || auth.Basis != "delegated:from=approver-1" {
		t.Errorf("authority = %+v", auth)
	}
}

func TestHTTPClient_CheckDelegation_Denied(t *testing.T) {
	server := delegationServer(t, delegationAnswer{HasDelegatedAccess: false, Basis: "no_delegated_grant"}, nil)
	defer server.Close()

	if _, err := newClient(server.URL).CheckDelegation(context.Background(), "delegate-1", "approver-1", "le-1"); !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Errorf("error = %v, want %v", err, domain.ErrAuthorizationDenied)
	}
}

// The old substring match: approver "approver-1" matched a basis naming
// "approver-10". Only an exact delegator, with a delegation id, is accepted.
func TestHTTPClient_CheckDelegation_RefusesAnotherDelegatorsAnswer(t *testing.T) {
	for name, answer := range map[string]delegationAnswer{
		"prefix of another delegator":  {HasDelegatedAccess: true, Basis: "delegated:from=approver-10", DelegationID: "d-9"},
		"no delegation id (old authz)": {HasDelegatedAccess: true, Basis: "delegated:from=approver-1"},
	} {
		server := delegationServer(t, answer, nil)
		_, err := newClient(server.URL).CheckDelegation(context.Background(), "delegate-1", "approver-1", "le-1")
		server.Close()
		if !errors.Is(err, domain.ErrAuthorizationDenied) {
			t.Errorf("%s: error = %v, want denied", name, err)
		}
	}
}

func TestHTTPClient_ForwardsEnvelopeHeaders(t *testing.T) {
	var capturedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer server.Close()

	ctx := svcmiddleware.WithTenant(context.Background(), "t-1")
	ctx = svcenvelope.WithEnvelope(ctx, svcenvelope.Envelope{
		TenantID:       "t-1",
		ActorSubjectID: "p-1",
		RequestID:      "req-1",
		CorrelationID:  "corr-1",
		SourceChannel:  svcenvelope.ChannelWeb,
		SourceSystem:   "ext",
		IdempotencyKey: "idem-1",
		CausationID:    "cause-1",
	})

	if _, err := newClient(server.URL).CheckApprovalAllowed(ctx, "p-1", "le-1", "owner-1", nil); err != nil {
		t.Fatalf("CheckApprovalAllowed returned error: %v", err)
	}
	for h, want := range map[string]string{
		"X-Request-Id": "req-1", "X-Correlation-ID": "corr-1", "X-Source-Channel": "web",
		"X-Causation-Id": "cause-1", "X-Tenant-Id": "t-1",
	} {
		if got := capturedHeaders.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	// The command's Idempotency-Key is not reused for the authorization
	// question: authorization-svc would replay one answer for every question
	// asked under the same key.
	if k := capturedHeaders.Get("Idempotency-Key"); k == "idem-1" || k == "" {
		t.Errorf("Idempotency-Key = %q, want a key of the authorization call's own", k)
	}
}
