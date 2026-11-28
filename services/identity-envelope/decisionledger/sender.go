package decisionledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPSender POSTs records to governance-decision-log-svc. Client is the
// writer's mTLS workload identity (identity-envelope/mtlsclient): the ledger
// admits the relay as a peer workload vouching for Principal, which must hold
// GOVERNANCE_DECISION_RECORD (authorization-svc migrations 000043, 000046).
type HTTPSender struct {
	// BaseURL is the ledger's mTLS listener, e.g.
	// https://governance-decision-log-svc:8450.
	BaseURL string
	// Principal is the writer's service principal (svc-<name>).
	Principal string
	// KeyPrefix makes the Idempotency-Key "<prefix>:<decision_id>".
	KeyPrefix string
	// Purpose is sent as X-Purpose-Context.
	Purpose string
	Client  *http.Client
}

// Send implements Sender. 201 (recorded) and 200 (a replay of the same
// record) are success; 409 is ErrPermanent; anything else (400 after a
// contract change, 401/403 while a grant is missing, 5xx, a network error) is
// retried, because a deploy, a grant or time fixes it.
func (s *HTTPSender) Send(ctx context.Context, rec Record) error {
	if s.Principal == "" {
		return fmt.Errorf("decisionledger: no ledger principal configured; the record stays pending")
	}
	if err := rec.Normalize(); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", ErrPermanent, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("decisionledger: build request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", rec.TenantID)
	h.Set("X-Principal-Id", s.Principal)
	h.Set("X-Legal-Entity-Id", rec.LegalEntityID)
	// The same correlation id on every attempt: the ledger treats a changed
	// correlation under the same Idempotency-Key as a different request (409).
	h.Set("X-Correlation-ID", rec.CorrelationID)
	h.Set("X-Request-Id", requestID())
	h.Set("X-Source-Channel", "system")
	if s.Purpose != "" {
		h.Set("X-Purpose-Context", s.Purpose)
	}
	h.Set("Idempotency-Key", s.KeyPrefix+":"+rec.DecisionID)
	if rec.CausationID != "" {
		h.Set("X-Causation-Id", rec.CausationID)
	}
	if rec.JurisdictionContext != "" {
		h.Set("X-Jurisdiction-Context", rec.JurisdictionContext)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("decisionledger: call governance-decision-log-svc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: governance-decision-log-svc returned 409: %s", ErrPermanent, msg)
	default:
		return fmt.Errorf("decisionledger: governance-decision-log-svc returned %d: %s", resp.StatusCode, msg)
	}
}

func requestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
