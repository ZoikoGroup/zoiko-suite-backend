package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client talks to general-ledger-svc over its HTTP API and nothing else. It carries identities, never credentials.
type client struct {
	base      string
	tenant    string
	principal string
	entity    string
	hc        *http.Client
}

func newClient(base, tenant, principal, entity string, timeout time.Duration) *client {
	return &client{base: strings.TrimRight(base, "/"), tenant: tenant, principal: principal, entity: entity, hc: &http.Client{Timeout: timeout}}
}

// apiError is a refusal or failure the service answered with.
type apiError struct {
	Status int
	Code   string
	Detail string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("%d: %s", e.Status, e.Detail)
}

// do sends one request. A 2xx answer is decoded into out (when given); anything else is an *apiError, with the body's own code and
// detail when it has them.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", c.tenant)
	req.Header.Set("X-Principal-Id", c.principal)
	if c.entity != "" {
		req.Header.Set("X-Legal-Entity-Id", c.entity)
	}
	req.Header.Set("X-Correlation-ID", "gak-rollout")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{Status: resp.StatusCode, Detail: strings.TrimSpace(string(raw))}
		var p struct {
			Error  string `json:"error"`
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &p) == nil && (p.Error != "" || p.Code != "") {
			ae.Code, ae.Detail = p.Error, p.Detail
			if p.Code != "" {
				ae.Code = p.Code
			}
		}
		return ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: the answer is not what was expected: %w", method, path, err)
		}
	}
	return nil
}
