// Package workflowclient is obligations-svc's client of workflow-svc for two
// WFC-05 needs:
//
//   - A waiver, a not-applicable decision or an evidence exemption needs an
//     APPROVED workflow decided by a principal other than the requester, bound
//     to that exact subject (R-001 s7.1, R-INV-07; tracker OB-S10). The
//     approval is checked with GET /v1/workflows/{id} and
//     POST /v1/workflows/{id}/verify-release, never taken on the caller's word.
//   - An escalation milestone opens a case (POST /v1/cases), idempotent on the
//     source clock and milestone (WF-S7; tracker OB-S11).
//
// Every failure to get an answer is ErrWorkflowUnavailable: callers fail
// closed.
package workflowclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/circuitbreaker"
	"zoiko.io/obligations-svc/internal/domain"
	svcenvelope "zoiko.io/obligations-svc/internal/envelope"
)

// Approval is what obligations-svc needs to know about an approval workflow.
type Approval struct {
	WorkflowInstanceID string
	WorkflowType       string
	WorkflowStatus     string
	SubjectType        string
	SubjectID          string
	InitiatedBy        string
	// Deciders are the principals recorded on the approved stages.
	Deciders []string
}

// Client is the workflow-svc surface obligations-svc uses.
type Client interface {
	// VerifyApproval confirms the workflow is APPROVED for exactly this
	// subject fingerprint and returns it. It returns domain.ErrApprovalRequired
	// when the workflow exists but does not release, and
	// domain.ErrWorkflowUnavailable when workflow-svc cannot answer.
	VerifyApproval(ctx context.Context, workflowInstanceID, subjectFingerprint string) (*Approval, error)
	// OpenCase opens (or returns) the case for an escalation.
	OpenCase(ctx context.Context, req CaseRequest) (caseID string, err error)
}

// CaseRequest is the body of POST /v1/cases for an escalation (WF-S2/WF-S7).
type CaseRequest struct {
	TenantID        string `json:"-"`
	LegalEntityID   string `json:"legal_entity_id"`
	CaseFamily      string `json:"case_family"`
	Severity        string `json:"severity"`
	Confidentiality string `json:"confidentiality"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	SourceType      string `json:"source_type"`
	SourceID        string `json:"source_id"`
	Milestone       string `json:"milestone"`
	IdempotencyKey  string `json:"-"`
}

// ServicePrincipal is the principal obligations-svc acts as when it opens a
// case outside a request (the escalation relay).
const ServicePrincipal = "svc-obligations"

// HTTPClient implements Client.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
	breaker *circuitbreaker.CircuitBreaker
}

// New binds a client to workflow-svc's base URL.
func New(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 5 * time.Second}, log: log,
		breaker: circuitbreaker.NewCircuitBreaker("workflow-svc", circuitbreaker.DefaultConfig())}
}

type workflowWire struct {
	WorkflowInstanceID string  `json:"workflow_instance_id"`
	WorkflowType       string  `json:"workflow_type"`
	WorkflowStatus     string  `json:"workflow_status"`
	SubjectType        *string `json:"subject_type"`
	SubjectID          *string `json:"subject_id"`
	InitiatedBy        string  `json:"initiated_by"`
}

// transitionWire is one row of GET /v1/workflows/{id}/evidence.
type transitionWire struct {
	ToState string `json:"to_state"`
	ActedBy string `json:"acted_by"`
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any, hdr map[string]string) (int, []byte, error) {
	var status int
	var raw []byte
	err := c.breaker.Execute(ctx, func() error {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
		if err != nil {
			return domain.ErrWorkflowUnavailable
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		svcenvelope.ForwardTo(ctx, req)
		idenv.Forward(ctx, req)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.log.Error("workflow-svc unreachable; failing closed", zap.String("path", path), zap.Error(err))
			return domain.ErrWorkflowUnavailable
		}
		defer resp.Body.Close()
		status = resp.StatusCode
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if status >= 500 {
			return domain.ErrWorkflowUnavailable
		}
		return nil
	})
	if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		return 0, nil, domain.ErrWorkflowUnavailable
	}
	return status, raw, err
}

// VerifyApproval implements Client.
func (c *HTTPClient) VerifyApproval(ctx context.Context, id, fingerprint string) (*Approval, error) {
	path := "/v1/workflows/" + url.PathEscape(id)
	status, raw, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest:
		return nil, fmt.Errorf("%w: workflow %s not found", domain.ErrApprovalRequired, id)
	case http.StatusForbidden, http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: the caller cannot read workflow %s", domain.ErrApprovalRequired, id)
	default:
		return nil, domain.ErrWorkflowUnavailable
	}
	var w workflowWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, domain.ErrWorkflowUnavailable
	}
	a := &Approval{WorkflowInstanceID: w.WorkflowInstanceID, WorkflowType: w.WorkflowType, WorkflowStatus: w.WorkflowStatus,
		InitiatedBy: w.InitiatedBy}
	if w.SubjectType != nil {
		a.SubjectType = *w.SubjectType
	}
	if w.SubjectID != nil {
		a.SubjectID = *w.SubjectID
	}
	es, eraw, err := c.do(ctx, http.MethodGet, path+"/evidence", nil, nil)
	if err != nil {
		return nil, err
	}
	if es != http.StatusOK {
		return nil, fmt.Errorf("%w: the approval evidence of workflow %s cannot be read (%d)", domain.ErrApprovalRequired, id, es)
	}
	var trs []transitionWire
	if err := json.Unmarshal(eraw, &trs); err != nil {
		return nil, domain.ErrWorkflowUnavailable
	}
	seen := map[string]bool{}
	for _, tr := range trs {
		if strings.EqualFold(tr.ToState, "APPROVED") && tr.ActedBy != "" && !seen[tr.ActedBy] {
			seen[tr.ActedBy] = true
			a.Deciders = append(a.Deciders, tr.ActedBy)
		}
	}
	vs, vraw, err := c.do(ctx, http.MethodPost, path+"/verify-release",
		map[string]any{"current_subject_fingerprint": fingerprint}, nil)
	if err != nil {
		return nil, err
	}
	switch vs {
	case http.StatusOK:
		return a, nil
	case http.StatusConflict, http.StatusNotFound, http.StatusBadRequest, http.StatusForbidden, http.StatusUnauthorized,
		http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		return a, fmt.Errorf("%w: workflow %s does not release for this subject (%d %s)", domain.ErrApprovalRequired, id, vs, short(vraw))
	}
	return nil, domain.ErrWorkflowUnavailable
}

// OpenCase implements Client.
func (c *HTTPClient) OpenCase(ctx context.Context, req CaseRequest) (string, error) {
	hdr := map[string]string{"Idempotency-Key": req.IdempotencyKey}
	if _, ok := idenv.FromContext(ctx); !ok {
		// The escalation relay has no request envelope: it acts as the
		// service principal in the clock's tenant. Under IDENTITY_VERIFY=require
		// on workflow-svc this path needs the mTLS peer listener (WF-S16).
		hdr["X-Principal-Id"] = ServicePrincipal
		hdr["X-Tenant-Id"] = req.TenantID
		hdr["X-Correlation-ID"] = req.IdempotencyKey
		// workflow-svc's input contract (§4) requires both on every write:
		// the case's legal entity (INV-02) and the channel, here a system
		// relay. Without them every escalation case was refused 400
		// envelope_incomplete (found by the live test of 26 Nov 2026).
		hdr["X-Legal-Entity-Id"] = req.LegalEntityID
		hdr["X-Source-Channel"] = "system"
		hdr["X-Source-System"] = "obligations-svc"
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/cases", req, hdr)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("workflow-svc refused the case (%d): %s", status, short(raw))
	}
	var out struct {
		CaseID string `json:"case_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.CaseID == "" {
		return "", fmt.Errorf("workflow-svc answered %d without a case_id", status)
	}
	return out.CaseID, nil
}

func short(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
