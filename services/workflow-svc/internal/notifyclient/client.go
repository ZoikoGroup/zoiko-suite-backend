// Package notifyclient delivers workflow notices to notification-svc's
// POST /v1/notifications (Doc 03 §9.7). It is the outbox relay's sender for
// destination "notification": a notice is written to outbox_events in the
// transition's own transaction (internal/notify decides who is told what), and
// this deliverer sends it with backoff until notification-svc has it.
//
// A notice that is never delivered leaves the decision standing: the relay
// retries it, then parks it for an operator. Nothing here runs on the request
// path.
package notifyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zoiko.io/workflow-svc/internal/notify"
	"zoiko.io/workflow-svc/internal/outbox"
)

// CommunicationClass is T0, transactional: a notice about a request the
// recipient is part of. notification-svc's preferences may not suppress it
// the way they suppress marketing.
const CommunicationClass = "T0"

// Deliverer sends one notice to notification-svc as workflow-svc's own
// service principal, which needs NOTIFICATION_SEND (seed-rbac.sql section 5).
type Deliverer struct {
	baseURL     string
	principalID string
	http        *http.Client
}

// New binds the deliverer to notification-svc at baseURL, sending as
// principalID.
func New(baseURL, principalID string) *Deliverer {
	return &Deliverer{
		baseURL:     strings.TrimRight(baseURL, "/"),
		principalID: principalID,
		http:        &http.Client{Timeout: 10 * time.Second},
	}
}

// NewWith is New over a caller-supplied client (tests; an mTLS client once
// notification-svc runs a peer listener).
func NewWith(baseURL, principalID string, client *http.Client) *Deliverer {
	return &Deliverer{baseURL: strings.TrimRight(baseURL, "/"), principalID: principalID, http: client}
}

type sendRequest struct {
	RecipientPrincipalID string `json:"recipient_principal_id"`
	LegalEntityID        string `json:"legal_entity_id"`
	Channel              string `json:"channel"`
	Subject              string `json:"subject"`
	Body                 string `json:"body"`
	SourceEventType      string `json:"source_event_type,omitempty"`
	SourceReference      string `json:"source_reference,omitempty"`
	CorrelationID        string `json:"correlation_id"`
	IdempotencyKey       string `json:"idempotency_key"`
	CommunicationClass   string `json:"communication_class"`
}

// IdempotencyKey is the key a notice is sent under. It is the outbox row's
// own id, so a redelivery after a lost response is answered as a replay and
// nobody is told twice.
func IdempotencyKey(outboxEventID string) string {
	return "workflow-notice:" + outboxEventID
}

// Deliver implements outbox.Deliverer.
func (d *Deliverer) Deliver(ctx context.Context, e outbox.StoredEvent) error {
	if d.principalID == "" {
		return fmt.Errorf("WORKFLOW_NOTIFY_PRINCIPAL_ID is not set; the notice stays pending")
	}
	var n notify.Notice
	if err := json.Unmarshal(e.Payload, &n); err != nil {
		return fmt.Errorf("%w: undecodable notice: %v", outbox.ErrPermanent, err)
	}
	if n.RecipientPrincipalID == "" || n.Channel == "" || n.Subject == "" {
		return fmt.Errorf("%w: notice without recipient, channel or subject", outbox.ErrPermanent)
	}
	correlation := e.Correlation()
	if correlation == "" {
		// notification-svc requires one; the workflow is the flow the notice
		// belongs to.
		correlation = e.AggregateID
	}
	key := IdempotencyKey(e.OutboxEventID)
	body, err := json.Marshal(sendRequest{
		RecipientPrincipalID: n.RecipientPrincipalID,
		LegalEntityID:        e.LegalEntityID,
		Channel:              n.Channel,
		Subject:              n.Subject,
		Body:                 n.Body,
		SourceEventType:      n.SourceEventType,
		SourceReference:      n.SourceReference,
		CorrelationID:        correlation,
		IdempotencyKey:       key,
		CommunicationClass:   CommunicationClass,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", outbox.ErrPermanent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/v1/notifications", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notification request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", e.TenantID)
	h.Set("X-Principal-Id", d.principalID)
	h.Set("X-Workload-Id", "workflow-svc")
	h.Set("X-Legal-Entity-Id", e.LegalEntityID)
	h.Set("X-Correlation-ID", correlation)
	// Deterministic, like the key: a retry is the same request.
	h.Set("X-Request-Id", e.OutboxEventID)
	h.Set("X-Source-Channel", "system")
	h.Set("X-Workflow-Instance-Id", e.AggregateID)
	h.Set("Idempotency-Key", key)

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("call notification-svc: %w", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		// 201 also covers a notice notification-svc recorded as FAILED (no
		// email address on file, say): that outcome is its to keep and retry,
		// not this relay's.
		return nil
	case resp.StatusCode == http.StatusConflict:
		// The key already names a different notice: resending cannot succeed.
		return fmt.Errorf("%w: %d: %s", outbox.ErrPermanent, resp.StatusCode, msg)
	default:
		// 400 (a contract mismatch between versions), 401/403 (the send grant
		// is missing), 429 (quota), 5xx: a deploy, a grant or time fixes these.
		return fmt.Errorf("notification-svc returned %d: %s", resp.StatusCode, msg)
	}
}
