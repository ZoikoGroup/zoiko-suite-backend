// Package vaultsecrets leases this service's provider and webhook secrets from secret-vault-integration-svc instead of
// reading them from the environment (S7-B3, ZS-SVC-Y-001 INV-26).
//
// It uses the vault's /v1/secrets/use operation, the one documented exception to "the vault never returns material",
// which hands out only the PROVIDER_CREDENTIAL and CALLBACK_SECRET classes to workloads a policy names. Every use is
// audited by the vault. Secrets are fetched at start-up (a service that cannot get its provider credential does not
// start) and re-fetched on an interval, so a rotation in the vault reaches the running process without a restart.
//
// Not covered, by decision: the action-token and unsubscribe HMAC keys (they belong with the vault's sign operation)
// and the DKIM private key (the vault cannot return it); both stay in the environment.
package vaultsecrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ErrUnavailable means the vault did not hand out the secret.
var ErrUnavailable = errors.New("secret vault unavailable")

// Client calls secret-vault-integration-svc.
type Client struct {
	base      string
	http      *http.Client
	principal string
	tenantID  string
}

// NewClient builds a client. principal is this service's workload identity, which each policy must list in its
// allowed workloads; tenantID is the scope the policies are registered under.
func NewClient(baseURL, principal, tenantID string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("vault base URL must be absolute")
	}
	if strings.TrimSpace(principal) == "" || strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("a workload principal and a tenant scope are required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	if hc.Timeout <= 0 {
		return nil, errors.New("vault HTTP client needs a timeout")
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: hc, principal: principal, tenantID: tenantID}, nil
}

// Use fetches the secret governed by path. It never logs the value.
func (c *Client) Use(ctx context.Context, path string) ([]byte, error) {
	reqID := uuid.NewString()
	payload, err := json.Marshal(map[string]string{"secret_path": path, "requested_by_principal_id": c.principal, "request_id": reqID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/secrets/use", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Tenant-Id", c.tenantID)
	req.Header.Set("X-Request-Id", reqID)
	req.Header.Set("X-Correlation-Id", reqID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "service-secret-lease")
	req.Header.Set("Idempotency-Key", reqID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: vault unreachable", ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable response", ErrUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: vault answered %d for %s", ErrUnavailable, resp.StatusCode, path)
	}
	var out struct {
		CredentialBase64 string `json:"credential_base64"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.CredentialBase64 == "" {
		return nil, fmt.Errorf("%w: the vault returned no credential for %s", ErrUnavailable, path)
	}
	val, err := base64.StdEncoding.DecodeString(out.CredentialBase64)
	if err != nil || len(val) == 0 {
		return nil, fmt.Errorf("%w: the credential for %s is unreadable", ErrUnavailable, path)
	}
	return val, nil
}

// Fetcher is the part of Client the refresher uses.
type Fetcher interface {
	Use(ctx context.Context, path string) ([]byte, error)
}

// Secret is one vault-held value and what to do with it.
type Secret struct {
	Name  string
	Path  string
	Apply func(value []byte) error
}

// Refresher keeps secrets current. A failed fetch or apply keeps the value already in use and is logged: a vault
// outage must not take a running sender down, and a rotation lands on the next successful fetch.
type Refresher struct {
	f       Fetcher
	secrets []Secret
	log     *zap.Logger
	last    map[string]string
}

// NewRefresher builds a refresher over the given secrets.
func NewRefresher(f Fetcher, secrets []Secret, log *zap.Logger) *Refresher {
	if log == nil {
		log = zap.NewNop()
	}
	return &Refresher{f: f, secrets: secrets, log: log, last: map[string]string{}}
}

// Load fetches and applies every secret once. It returns the first failure: used at start-up, where nothing may
// proceed on a secret that could not be obtained.
func (r *Refresher) Load(ctx context.Context) error {
	for _, s := range r.secrets {
		val, err := r.f.Use(ctx, s.Path)
		if err != nil {
			return fmt.Errorf("secret %s: %w", s.Name, err)
		}
		if err := s.Apply(val); err != nil {
			return fmt.Errorf("secret %s: %w", s.Name, err)
		}
		r.last[s.Name] = string(val)
	}
	return nil
}

// Refresh re-fetches every secret and applies the ones that changed. It reports how many changed.
func (r *Refresher) Refresh(ctx context.Context) int {
	changed := 0
	for _, s := range r.secrets {
		val, err := r.f.Use(ctx, s.Path)
		if err != nil {
			r.log.Warn("secret refresh failed: the value in use is kept", zap.String("secret", s.Name), zap.Error(err))
			continue
		}
		if r.last[s.Name] == string(val) {
			continue
		}
		if err := s.Apply(val); err != nil {
			r.log.Error("a rotated secret was refused: the value in use is kept", zap.String("secret", s.Name), zap.Error(err))
			continue
		}
		r.last[s.Name] = string(val)
		changed++
		r.log.Info("secret rotated", zap.String("secret", s.Name))
	}
	return changed
}

// Run refreshes every interval until ctx ends.
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh(ctx)
		}
	}
}
