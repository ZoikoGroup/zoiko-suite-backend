// Package svcclient is the small shared HTTP helper behind this service's
// downstream contract clients (AP-01 eligibility, TAX, ORG-10, AP-04 receipts,
// AP-03 PO lines). Every call carries the governed headers and returns the raw
// status and body, so each client decides what a status means; a transport
// error is always reported as ErrUnavailable and callers fail closed on it.
package svcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable means the dependency could not give an answer (transport error,
// timeout, undecodable body, or a contract endpoint that does not exist yet).
// "Cannot answer" is never "no problem": callers fail closed.
var ErrUnavailable = errors.New("dependency unavailable")

// Caller identifies who the downstream call is made on behalf of.
type Caller struct {
	TenantID       string
	PrincipalID    string
	CorrelationID  string
	RequestID      string
	IdempotencyKey string
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// Do performs the call. body, when non-nil, is JSON-encoded.
func (c *Client) Do(ctx context.Context, caller Caller, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, ErrUnavailable
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Source-Channel", "system")
	if caller.PrincipalID != "" {
		req.Header.Set("X-Principal-Id", caller.PrincipalID)
	}
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", caller.CorrelationID)
	}
	if caller.RequestID != "" {
		req.Header.Set("X-Request-Id", caller.RequestID)
	}
	if caller.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", caller.IdempotencyKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	return resp.StatusCode, raw, nil
}
