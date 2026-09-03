// Package identity resolves a recipient principal to the contact endpoint a
// notification is actually delivered to.
//
// Until this package existed, notification-svc knew a recipient only as
// recipient_principal_id and had nowhere to send anything: the register stored
// who the notice was for and never what address it went to, and StubDeliverer
// covered the gap by reporting success without a provider. An EMAIL
// notification could therefore be recorded SENT with no address in existence
// anywhere in the request or the row.
//
// identity-context-svc owns principals and their contact facts (its
// domain.Principal marks Email as PII subject to the owning tenant's residency
// policy), so it is the authority here. This service stores the resolved
// address as a delivery snapshot, not as a competing master — the boundary
// ZS-SVC-Y-001 §1.4 draws between NCD and MDM/IAM.
//
// Like internal/authz, this lives in its own package rather than inline in
// cmd/server so the response parsing is exercised by a test against a real
// server. authz's doc comment records what happens otherwise: a client
// decoding a field the upstream never sends, failing identically to a
// permission nobody granted, for as long as nobody looked.
package identity

import (
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

	"zoiko.io/notification-svc/internal/domain"
)

type Client struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewClient(baseURL string, log *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
		log:     log,
	}
}

// principalResponse is the subset of identity-context-svc's domain.Principal
// this service needs. Only these two fields are decoded on purpose: the
// principal record carries DisplayName and IdentityProviderSubject, both PII
// with no bearing on where a message goes, and a struct that names them would
// pull them into this service's logs and traces for no reason.
type principalResponse struct {
	PrincipalID string `json:"principal_id"`
	Email       string `json:"email"`
}

// ResolveEmail returns the email address identity-context-svc holds for a
// principal within a tenant.
//
// The three outcomes are kept distinct because they mean different things to
// the notification that triggered the lookup:
//
//   - ErrPrincipalNotFound / ErrPrincipalHasNoAddress are settled. Asking
//     again returns the same answer, so the notification is concluded FAILED
//     with that reason on the record.
//   - ErrIdentityServiceUnavailable is unknown, not absent. The address may
//     well exist; this service could not reach the authority that holds it.
//     Recorded distinctly so it is legible as an outage rather than as a
//     recipient with no contact details.
//
// callerPrincipalID is forwarded as X-Principal-Id because identity-context-svc
// refuses an unattributed read. It is the principal that asked for the send,
// not the recipient — the recipient is the subject of the lookup, and using
// the recipient here would let any caller read any principal's address by
// naming it.
func (c *Client) ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error) {
	if tenantID == "" || callerPrincipalID == "" || recipientPrincipalID == "" {
		return "", domain.ErrIdentityMissing
	}

	endpoint := c.baseURL + "/v1/principals/" + url.PathEscape(recipientPrincipalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %s", domain.ErrIdentityServiceUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", callerPrincipalID)
	req.Header.Set("Accept", "application/json")
	if cid := correlationFrom(ctx); cid != "" {
		req.Header.Set("X-Correlation-ID", cid)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Warn("identity-context-svc unreachable",
			zap.String("recipient_principal_id", recipientPrincipalID), zap.Error(err))
		return "", fmt.Errorf("%w: %s", domain.ErrIdentityServiceUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", domain.ErrPrincipalNotFound

	case resp.StatusCode == http.StatusOK:
		// Bounded: a principal record is a few hundred bytes, and an
		// unbounded decode of another service's response is a way for one
		// service's fault to become this one's memory.
		var p principalResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&p); err != nil {
			return "", fmt.Errorf("%w: decoding principal: %s", domain.ErrIdentityServiceUnavailable, err)
		}
		email := strings.TrimSpace(p.Email)
		if email == "" {
			// A 200 with no address is not an outage. The principal exists
			// and has no email on record, which is a fact about the recipient
			// and will not change by asking again.
			return "", domain.ErrPrincipalHasNoAddress
		}
		return email, nil

	default:
		// 401/403 land here too. They are this service's own misconfiguration
		// rather than a fact about the recipient, and treating them as "no
		// address" would write a permanent FAILED for every notification while
		// a header was wrong.
		c.log.Warn("identity-context-svc returned an unexpected status",
			zap.Int("status", resp.StatusCode),
			zap.String("recipient_principal_id", recipientPrincipalID))
		return "", fmt.Errorf("%w: unexpected status %d", domain.ErrIdentityServiceUnavailable, resp.StatusCode)
	}
}

// IsSettled reports whether a resolution error is a fact about the recipient
// rather than a transient failure to reach the authority. A settled failure
// concludes the notification; an unsettled one is still recorded, but marked
// so it reads as an outage and is a candidate for re-attempt.
func IsSettled(err error) bool {
	return errors.Is(err, domain.ErrPrincipalNotFound) || errors.Is(err, domain.ErrPrincipalHasNoAddress)
}

// correlationFrom pulls the correlation id the handler put on the context, so
// a recipient lookup can be tied to the send that caused it when tracing
// across the two services.
func correlationFrom(ctx context.Context) string {
	if v, ok := ctx.Value(correlationKey{}).(string); ok {
		return v
	}
	return ""
}

type correlationKey struct{}

// WithCorrelationID attaches a correlation id for outbound lookups.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationKey{}, correlationID)
}
