// Package approval confirms the independent approval a rule activation cites
// (Governance audit J-02, ZS-JUR-001 §3, V-001 §18.1).
//
// Activating a rule (DRAFT -> ACTIVE) requires X-Approval-Reference. It was
// recorded on the status-history row and never checked: any string passed as
// "approval", so the only control was that the activator was not the author.
// The reference is now a workflow-svc approval workflow: it must exist in the
// caller's tenant, be APPROVED, be bound to this rule as its subject
// (subject_type jurisdiction_rule, subject_id the rule id), and have been
// decided by someone other than the rule's author.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	idenv "zoiko.io/identity-envelope"

	svcenvelope "zoiko.io/jurisdiction-rules-svc/internal/envelope"
)

// SubjectType is the workflow subject_type an activation approval names.
const SubjectType = "jurisdiction_rule"

var (
	// ErrNotValid: the reference is not an approval of this activation.
	ErrNotValid = errors.New("approval reference is not a valid approval of this rule")
	// ErrUnavailable: workflow-svc could not answer; the activation fails
	// closed rather than treat "cannot tell" as "approved".
	ErrUnavailable = errors.New("workflow-svc unavailable")
)

// Verifier confirms an approval reference.
type Verifier interface {
	Verify(ctx context.Context, reference, ruleID, ruleAuthor string) error
}

// Client reads GET /v1/workflows/{id} from workflow-svc as the caller (the
// verified envelope is forwarded), so workflow-svc's own read authorization
// and tenant isolation apply.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client for workflow-svc at baseURL.
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type workflow struct {
	WorkflowInstanceID string  `json:"workflow_instance_id"`
	WorkflowStatus     string  `json:"workflow_status"`
	SubjectType        *string `json:"subject_type"`
	SubjectID          *string `json:"subject_id"`
	InitiatedBy        string  `json:"initiated_by"`
	Stages             []struct {
		StageStatus string  `json:"stage_status"`
		ActedBy     *string `json:"acted_by"`
	} `json:"stages"`
}

// Verify returns nil when reference is an APPROVED workflow for ruleID decided
// by someone other than ruleAuthor; ErrNotValid (wrapped with the reason)
// when it is not; ErrUnavailable when workflow-svc cannot answer.
func (c *Client) Verify(ctx context.Context, reference, ruleID, ruleAuthor string) error {
	if !uuidRe.MatchString(reference) {
		return fmt.Errorf("%w: the reference is not a workflow id", ErrNotValid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/workflows/"+url.PathEscape(reference), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest:
		return fmt.Errorf("%w: no such approval in this tenant", ErrNotValid)
	case http.StatusForbidden:
		// The activator may not read the approval it cites.
		return fmt.Errorf("%w: the approval is not readable by the activator", ErrNotValid)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: workflow-svc answered %d: %s", ErrUnavailable, resp.StatusCode, body)
	}

	var wf workflow
	if err := json.NewDecoder(resp.Body).Decode(&wf); err != nil {
		return fmt.Errorf("%w: malformed workflow: %v", ErrUnavailable, err)
	}
	switch {
	case wf.WorkflowStatus != "APPROVED":
		return fmt.Errorf("%w: the approval is %s, not APPROVED", ErrNotValid, wf.WorkflowStatus)
	case wf.SubjectType == nil || *wf.SubjectType != SubjectType || wf.SubjectID == nil || *wf.SubjectID != ruleID:
		return fmt.Errorf("%w: the approval is not bound to this rule", ErrNotValid)
	}
	decided := false
	for _, st := range wf.Stages {
		if st.ActedBy == nil || *st.ActedBy == "" {
			continue
		}
		decided = true
		if *st.ActedBy == ruleAuthor {
			return fmt.Errorf("%w: the rule's author decided its own approval", ErrNotValid)
		}
	}
	if !decided {
		return fmt.Errorf("%w: the approval records no decision", ErrNotValid)
	}
	return nil
}
