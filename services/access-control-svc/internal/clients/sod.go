package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// SoDClient checks segregation-of-duties conflicts via authorization-svc's
// /v1/sod/validate endpoint. It is called BEFORE provisioning a role or
// bundle so a conflicting definition never reaches the enforcement plane.
type SoDClient struct {
	baseURL string
	http    *http.Client
}

func NewSoDClient(baseURL string) *SoDClient {
	return &SoDClient{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

// CheckConflict calls authorization-svc's SoD validation. Returns
// domain.ErrSoDConflict if the requested actions would violate a rule.
func (c *SoDClient) CheckConflict(ctx context.Context, req domain.SoDCheckRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal sod check: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sod/validate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Principal-Id", req.PrincipalID)
	httpReq.Header.Set("X-Tenant-Id", req.TenantID)
	httpReq.Header.Set("X-Legal-Entity-Id", req.LegalEntityID)
	httpReq.Header.Set("X-Correlation-ID", req.CorrelationID)
	httpReq.Header.Set("X-Request-Id", uuid.NewString())
	httpReq.Header.Set("X-Source-Channel", "system")
	httpReq.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sod service unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		var detail map[string]any
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&detail)
		return domain.ErrSoDConflict
	}
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("sod service returned %d: %s", resp.StatusCode, string(detail))
	}
	return nil
}

// ProtectedActionsClient fetches the active protected permissions list.
// Cached by the handler with a short TTL to avoid repeated calls.
type ProtectedActionsClient struct {
	baseURL string
	http    *http.Client
}

func NewProtectedActionsClient(baseURL string) *ProtectedActionsClient {
	return &ProtectedActionsClient{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

// ListActive returns all active protected action names.
func (c *ProtectedActionsClient) ListActive(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/protected-permissions", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Request-Id", uuid.NewString())
	httpReq.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("protected permissions service unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("protected permissions service returned %d: %s", resp.StatusCode, string(detail))
	}

	var perms []struct {
		ActionName string `json:"action_name"`
		ActiveFlag bool   `json:"active_flag"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&perms); err != nil {
		return nil, fmt.Errorf("decode protected permissions: %w", err)
	}

	var active []string
	for _, p := range perms {
		if p.ActiveFlag {
			active = append(active, p.ActionName)
		}
	}
	return active, nil
}