// Package signatory fetches policy-svc's signatory-matrix decisions and checks
// them the way ZS-GOV-INT-002 §2.3 requires of every enforcement point: the
// decision must verify against policy-svc's published key, name this tenant,
// legal entity and resource, come from policy-svc for the SIGNATORY_MATRIX
// family, not be future-dated or expired, and carry a known outcome. Anything
// else is ERROR here, whatever the decision says. A 200 from policy-svc is
// never by itself a decision.
package signatory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	idenv "zoiko.io/identity-envelope"
	"zoiko.io/identity-envelope/govdecision"
)

// Owner and family of the decisions this package accepts.
const (
	OwningService = "policy-svc"
	PolicyFamily  = "SIGNATORY_MATRIX"
)

var (
	// ErrUnavailable: policy-svc could not be asked (fail closed: 503).
	ErrUnavailable = errors.New("policy-svc unavailable")
	// ErrNotFound: policy-svc holds no such decision for this tenant.
	ErrNotFound = errors.New("signatory decision not found")
)

// UntrustedError lists why a decision cannot be acted on.
type UntrustedError struct{ Problems []string }

func (e *UntrustedError) Error() string {
	return "signatory decision cannot be trusted: " + strings.Join(e.Problems, "; ")
}

// Request names what the caller expects the decision to be about.
type Request struct {
	TenantID      string
	LegalEntityID string
	DecisionID    string
	// PrincipalID and CorrelationID travel to policy-svc's input contract
	// (the verified envelope, when the caller has one, is forwarded too).
	PrincipalID   string
	CorrelationID string
	// Resource, when set, must equal the decision's resource exactly.
	Resource govdecision.Resource
	// RequireValid refuses a decision past its valid_until (creating a
	// signature workflow). A release re-check passes false and judges the
	// requirement itself instead.
	RequireValid bool
}

// Verifier fetches and checks decisions.
type Verifier interface {
	Verify(ctx context.Context, req Request) (*govdecision.Decision, error)
}

// HTTPVerifier asks policy-svc over HTTP (or its mTLS listener).
type HTTPVerifier struct {
	baseURL string
	http    *http.Client
	now     func() time.Time
	keysTTL time.Duration

	mu     sync.Mutex
	keys   govdecision.KeySet
	keysAt time.Time
}

// NewHTTPVerifier returns a verifier for policy-svc at baseURL. hc nil uses a
// plain client with a 5 s timeout.
func NewHTTPVerifier(baseURL string, hc *http.Client) *HTTPVerifier {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPVerifier{baseURL: strings.TrimRight(baseURL, "/"), http: hc, now: time.Now, keysTTL: 5 * time.Minute}
}

func (v *HTTPVerifier) get(ctx context.Context, path string, r Request) (int, []byte, error) {
	tenantID, legalEntityID := r.TenantID, r.LegalEntityID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.baseURL+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// The caller's verified identity travels with the read (policy-svc
	// authorizes it as POLICY_READ); the tenant is pinned to the workflow's.
	idenv.Forward(ctx, req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	if legalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	}
	if req.Header.Get("X-Principal-Id") == "" && r.PrincipalID != "" {
		req.Header.Set("X-Principal-Id", r.PrincipalID)
	}
	req.Header.Set("X-Request-Id", uuid.NewString())
	correlation := r.CorrelationID
	if correlation == "" {
		correlation = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlation)
	if req.Header.Get("X-Source-Channel") == "" {
		req.Header.Set("X-Source-Channel", "system")
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return resp.StatusCode, body, nil
}

// keySet returns policy-svc's decision keys, cached; refresh forces a fetch
// (an unknown key id may be a rotation).
func (v *HTTPVerifier) keySet(ctx context.Context, r Request, refresh bool) (govdecision.KeySet, error) {
	v.mu.Lock()
	if !refresh && v.keys != nil && v.now().Sub(v.keysAt) < v.keysTTL {
		k := v.keys
		v.mu.Unlock()
		return k, nil
	}
	v.mu.Unlock()
	code, body, err := v.get(ctx, "/v1/policy-decision-keys", Request{TenantID: r.TenantID, PrincipalID: r.PrincipalID, CorrelationID: r.CorrelationID})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("%w: decision keys answered %d", ErrUnavailable, code)
	}
	keys, err := govdecision.ParseJWKS(body)
	if err != nil {
		return nil, fmt.Errorf("%w: decision keys unreadable: %v", ErrUnavailable, err)
	}
	v.mu.Lock()
	v.keys, v.keysAt = keys, v.now()
	v.mu.Unlock()
	return keys, nil
}

// Verify fetches decision req.DecisionID and checks it.
func (v *HTTPVerifier) Verify(ctx context.Context, req Request) (*govdecision.Decision, error) {
	code, raw, err := v.get(ctx, "/internal/v1/policy/decisions/"+url.PathEscape(req.DecisionID), req)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		return nil, ErrNotFound
	case code != http.StatusOK:
		// 401/403 is this service (or the caller) lacking POLICY_READ: not an
		// answer about the decision, and never a reason to accept one.
		return nil, fmt.Errorf("%w: policy-svc answered %d", ErrUnavailable, code)
	}
	keys, err := v.keySet(ctx, req, false)
	if err != nil {
		return nil, err
	}
	if keys.Verify(raw) != nil {
		if keys, err = v.keySet(ctx, req, true); err != nil {
			return nil, err
		}
	}
	return Check(raw, req, keys, v.now())
}

// Check applies the contract checks to a decision's raw JSON.
func Check(raw []byte, req Request, keys govdecision.KeySet, now time.Time) (*govdecision.Decision, error) {
	verdict := govdecision.Check(raw, govdecision.Expectation{
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, Resource: req.Resource,
		OwningService: OwningService, PolicyFamily: PolicyFamily, Now: now, Keys: keys,
	})
	problems := verdict.Problems
	if !req.RequireValid {
		// A release re-check judges a stored decision's requirement, not its
		// freshness: drop only the expiry problem.
		kept := problems[:0]
		for _, p := range problems {
			if p != govdecision.ReasonDecisionExpired {
				kept = append(kept, p)
			}
		}
		problems = kept
	}
	if len(problems) > 0 || verdict.Decision == nil {
		if verdict.Decision == nil && len(problems) == 0 {
			problems = []string{"decision unreadable"}
		}
		return nil, &UntrustedError{Problems: problems}
	}
	return verdict.Decision, nil
}

// Quorum returns the decision's SIGNATURE_QUORUM required action.
func Quorum(d *govdecision.Decision) (*govdecision.RequiredAction, bool) {
	for i := range d.RequiredActions {
		if d.RequiredActions[i].Kind == govdecision.ActionKindSignatureQuorum {
			return &d.RequiredActions[i], true
		}
	}
	return nil, false
}
