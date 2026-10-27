package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Notice is one credential link to deliver.
type Notice struct {
	TenantID    string
	PrincipalID string
	// Intent is the notification intent (activation or reset).
	Intent        string
	Link          string
	ExpiresAt     time.Time
	SourceEventID string
	CorrelationID string
}

// Notifier delivers credential links. A link is a credential: it is never
// logged, never put on the event bus, and never returned to an
// administrator, only sent to the person's own verified address.
type Notifier interface {
	Notify(ctx context.Context, n Notice) error
}

// ErrNotifierUnconfigured: no delivery channel exists, so no link can be
// issued (503). Refusing is right: a link nobody receives is a dead account,
// and handing it to the administrator would let them set someone else's
// password.
var ErrNotifierUnconfigured = errors.New("credential delivery is not configured (NOTIFICATION_SERVICE_URL)")

// WorkloadToken mints this service's own envelope for a downstream audience
// (oidc.Provider.WorkloadEnvelope), so the call is attributable to
// identity-context-svc and verifiable at the receiver (X-R3).
type WorkloadToken func(tenantID, audience string) (string, error)

// NCDNotifier delivers through notification-svc's communication API
// (ZS-SVC-Y-001): create → prepare → dispatch, with a purpose-scoped
// idempotency key so a retry never sends twice.
type NCDNotifier struct {
	BaseURL            string
	ServicePrincipalID string
	LegalEntityID      string
	Token              WorkloadToken
	Client             *http.Client
}

func (n *NCDNotifier) call(ctx context.Context, tenantID, path, idemKey, corr string, body any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", n.ServicePrincipalID)
	req.Header.Set("X-Correlation-ID", corr)
	req.Header.Set("X-Request-Id", idemKey)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Workload-Id", "identity-context-svc")
	if n.Token != nil {
		if tok, err := n.Token(tenantID, "notification-svc"); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notification-svc %s: status %d", path, resp.StatusCode)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

// Notify implements Notifier.
func (n *NCDNotifier) Notify(ctx context.Context, nt Notice) error {
	if n == nil || n.BaseURL == "" {
		return ErrNotifierUnconfigured
	}
	exp := nt.ExpiresAt.UTC()
	created, err := n.call(ctx, nt.TenantID, "/v1/communications", nt.SourceEventID+"-create", nt.CorrelationID, map[string]any{
		"intent_id": nt.Intent, "legal_entity_id": n.LegalEntityID, "recipient_principal_id": nt.PrincipalID,
		"recipient_tenant_id": nt.TenantID, "locale": "en",
		"variables":       map[string]string{"link": nt.Link, "expires_at": exp.Format(time.RFC3339)},
		"source_event_id": nt.SourceEventID, "source_event_type": nt.Intent,
		"privacy_permission":   map[string]string{"decision": "PERMIT"},
		"marketing_permission": map[string]string{"decision": "DENY"},
		"expires_at":           exp,
	})
	if err != nil {
		return err
	}
	comm, _ := created["communication"].(map[string]any)
	id, _ := comm["communication_id"].(string)
	if id == "" {
		return errors.New("notification-svc returned no communication id")
	}
	if _, err := n.call(ctx, nt.TenantID, "/v1/communications/"+id+"/prepare", nt.SourceEventID+"-prepare", nt.CorrelationID, map[string]any{}); err != nil {
		return err
	}
	_, err = n.call(ctx, nt.TenantID, "/v1/communications/"+id+"/dispatch", nt.SourceEventID+"-dispatch", nt.CorrelationID, map[string]any{})
	return err
}

// DevNotifier holds the last link per principal in memory and exposes it
// only to the local development read route. It refuses to exist outside
// local/development (see NewDevNotifier).
type DevNotifier struct {
	mu    sync.Mutex
	links map[string]Notice
}

// NewDevNotifier builds one; env must be local or development.
func NewDevNotifier(env string) (*DevNotifier, error) {
	if env != "local" && env != "development" {
		return nil, fmt.Errorf("the development link store is refused in %s", env)
	}
	return &DevNotifier{links: map[string]Notice{}}, nil
}

// Notify implements Notifier.
func (d *DevNotifier) Notify(_ context.Context, n Notice) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.links[n.TenantID+"/"+n.PrincipalID+"/"+n.Intent] = n
	return nil
}

// Last returns the last link sent to a principal for intent.
func (d *DevNotifier) Last(tenantID, principalID, intent string) (Notice, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.links[tenantID+"/"+principalID+"/"+intent]
	return n, ok
}
