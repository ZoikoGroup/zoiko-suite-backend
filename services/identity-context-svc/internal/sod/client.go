// Package sod provides a client for GOV-04, the Segregation of Duties
// service.
//
// WHY THIS IS SEPARATE FROM AUTHZ. The authority matrix (spec section 3) is
// explicit that these are different decisions with different owners:
//
//	GOV-03  Permit/Deny + obligations      must never own: SoD exception
//	GOV-04  Static/dynamic conflict        must never own: permission grant
//
// and invariant 6 states it as a rule: "Authorization does not override SoD;
// SoD does not create permission." A permitted action can still be a conflict,
// and a conflict-free action can still be unauthorized. Collapsing the two
// calls into one client would make it possible to satisfy one and believe both
// had been checked, which is precisely the failure the separation exists to
// prevent.
//
// This service asks GOV-04 about exactly two things, and both are commands
// where one human's act is checked against another's:
//
//   - AttachSupportContext — the grantee must not be the approver, and the
//     approver must not hold a role that conflicts with granting support
//     access to the tenant in question.
//   - UpdatePrincipalStatus — suspending or reactivating a principal is a
//     maker-checker action in an organisation that separates identity
//     administration from security administration.
package sod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ErrConflict is returned on a CONFLICT decision. It maps to the spec's
// SOD_CONFLICT stable error class and to HTTP 409.
//
// Deliberately NOT folded into the authorization denial: a caller told
// "authorization denied" will go and ask for a permission grant, which cannot
// fix a segregation conflict and may well be granted, leaving the conflict in
// place and the caller convinced it was resolved.
var ErrConflict = errors.New("segregation of duties conflict")

// ErrUnavailable is returned when no decision could be obtained. Callers fail
// closed on it — a conflict check that did not run is not a check that passed.
var ErrUnavailable = errors.New("segregation of duties service unavailable")

// Decision is one conflict evaluation.
type Decision struct {
	Result string `json:"result"`
	// RuleVersion is the SoD policy version that decided. Recorded in the
	// evidence so the decision is reproducible against the rules as they
	// stood, not as they stand now.
	RuleVersion string `json:"rule_version"`
	ConflictID  string `json:"conflict_id"`
	Reason      string `json:"reason"`
	// ExceptionRef names an approved compensating control, when one exists.
	// Its presence is what turns a conflict into a permitted action, and it is
	// GOV-04's to issue — this service never creates one.
	ExceptionRef string `json:"exception_ref"`
}

// Request is one conflict question.
type Request struct {
	TenantID string `json:"tenant_id"`
	// ActionType is the command being attempted.
	ActionType string `json:"action_type"`
	// MakerPrincipalID is whoever is performing the action.
	MakerPrincipalID string `json:"maker_principal_id"`
	// CheckerPrincipalID is whoever approved it, where the action carries an
	// approver. Empty for a single-party action, which GOV-04 may still refuse
	// if its rules require two.
	CheckerPrincipalID string `json:"checker_principal_id,omitempty"`
	// SubjectPrincipalID is who the action is ABOUT, when that differs from
	// the maker. Suspending your own account and suspending someone else's are
	// different conflict questions.
	SubjectPrincipalID string `json:"subject_principal_id,omitempty"`
	CorrelationID      string `json:"correlation_id,omitempty"`
}

// Checker is the narrow interface callers depend on.
type Checker interface {
	// CheckConflict returns nil when the action is conflict-free, ErrConflict
	// on a CONFLICT decision, or ErrUnavailable when no decision was reached.
	CheckConflict(ctx context.Context, req Request) (*Decision, error)
}

// HTTPClient talks to a real GOV-04 instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewHTTPClient builds a client bound to baseURL.
func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// A short timeout on purpose. These calls sit in front of privileged
		// commands, not the hot resolve path, but a support engineer waiting
		// thirty seconds to learn their elevation was refused will retry, and
		// the retry is what turns one stuck call into a queue.
		http: &http.Client{Timeout: 3 * time.Second},
		log:  log,
	}
}

// CheckConflict asks GOV-04.
//
// Fails CLOSED on every ambiguity: a transport error, a non-200, an
// undecodable body and an unrecognised result all return ErrUnavailable, and
// callers refuse the command. The alternative — treating "we could not ask"
// as "no conflict" — means a GOV-04 outage silently disables the control
// rather than blocking on it, and nobody finds out until an audit.
func (c *HTTPClient) CheckConflict(ctx context.Context, req Request) (*Decision, error) {
	if req.TenantID == "" || req.ActionType == "" || req.MakerPrincipalID == "" {
		return nil, fmt.Errorf("%w: tenant_id, action_type and maker_principal_id are required", ErrUnavailable)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal request: %v", ErrUnavailable, err)
	}

	url := c.baseURL + "/v1/sod/evaluate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-Id", req.TenantID)
	if req.CorrelationID != "" {
		httpReq.Header.Set("X-Correlation-ID", req.CorrelationID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		c.log.Error("SoD service unreachable — failing closed",
			zap.String("action_type", req.ActionType),
			zap.String("correlation_id", req.CorrelationID),
			zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("SoD service returned an unexpected status — failing closed",
			zap.Int("status", resp.StatusCode),
			zap.String("action_type", req.ActionType))
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}

	var decision Decision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrUnavailable, err)
	}

	switch strings.ToUpper(decision.Result) {
	case "NO_CONFLICT", "PERMITTED", "CLEAR":
		return &decision, nil
	case "CONFLICT", "DENIED":
		// An approved compensating control turns a conflict into a permitted
		// action, and only GOV-04 can say one applies. Its presence in the
		// response IS that statement.
		if decision.ExceptionRef != "" {
			c.log.Info("SoD conflict cleared by an approved compensating control",
				zap.String("action_type", req.ActionType),
				zap.String("exception_ref", decision.ExceptionRef),
				zap.String("rule_version", decision.RuleVersion))
			return &decision, nil
		}
		return &decision, fmt.Errorf("%w: %s", ErrConflict, decision.Reason)
	default:
		// An unrecognised verdict is not a pass. A future GOV-04 adding a
		// third outcome must not have it read as approval here.
		c.log.Error("SoD service returned an unrecognised result — failing closed",
			zap.String("result", decision.Result))
		return nil, fmt.Errorf("%w: unrecognised result %q", ErrUnavailable, decision.Result)
	}
}

// ── Local-development stub ───────────────────────────────────────────────────

// PermitAllChecker clears every conflict. LOCAL DEVELOPMENT ONLY.
//
// Mirrors the authz package's PermitAllClient, including its refusal to build
// outside development — the two controls are paired, and a deployment that
// wired a real GOV-03 but a stubbed GOV-04 would satisfy "authorization
// enforced" while quietly having no segregation control at all.
type PermitAllChecker struct {
	log *zap.Logger
}

func (p *PermitAllChecker) CheckConflict(_ context.Context, req Request) (*Decision, error) {
	p.log.Debug("SoD stub — no conflict (local development only)",
		zap.String("action_type", req.ActionType))
	return &Decision{Result: "NO_CONFLICT", RuleVersion: "stub-0"}, nil
}

// NewChecker returns the right implementation for env.
//
// Refuses to hand back the stub in staging or production, and refuses a
// placeholder URL there too, for the same reason authz.NewClient does: a
// governance control that is silently absent is worse than one that is loudly
// broken, because only the second gets fixed.
func NewChecker(env, baseURL string, log *zap.Logger) (Checker, error) {
	normalized := strings.ToLower(strings.TrimSpace(env))
	isProd := normalized == "production" || normalized == "staging"

	if isProd {
		if baseURL == "" || strings.Contains(baseURL, "localhost") || strings.Contains(baseURL, "example.com") {
			return nil, fmt.Errorf(
				"security violation: SOD_SERVICE_URL (%q) is unusable in %s environment", baseURL, env)
		}
		return NewHTTPClient(baseURL, log), nil
	}

	if baseURL == "" {
		log.Warn("using NO-CONFLICT segregation-of-duties stub — wire real GOV-04 before production")
		return &PermitAllChecker{log: log}, nil
	}
	return NewHTTPClient(baseURL, log), nil
}
