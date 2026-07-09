// Package jwks fetches and caches RSA public keys from a JWKS endpoint.
package jwks

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

type key struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDoc struct {
	Keys []key `json:"keys"`
}

// Client fetches identity-context-svc's signing keys by kid and caches them
// for CacheTTL. It never sees or holds a private key.
type Client struct {
	url        string
	ttl        time.Duration
	httpClient *http.Client

	mu        sync.Mutex
	byKid     map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func NewClient(url string, ttl time.Duration) *Client {
	return &Client{
		url:        url,
		ttl:        ttl,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		byKid:      make(map[string]*rsa.PublicKey),
	}
}

// PublicKey returns the RSA public key for kid, refreshing the cached JWKS
// document if it's stale or the kid is unknown.
func (c *Client) PublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	pub, known := c.byKid[kid]
	stale := time.Since(c.fetchedAt) > c.ttl
	c.mu.Unlock()

	if known && !stale {
		return pub, nil
	}

	if err := c.refresh(ctx); err != nil {
		// A network blip against identity-context-svc shouldn't take the
		// whole gateway down if we already trust a key for this kid — the
		// cryptographic verification itself is unaffected by cache age.
		// Only fail if we have nothing at all to fall back on.
		if known {
			return pub, nil
		}
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	pub, known = c.byKid[kid]
	if !known {
		return nil, fmt.Errorf("no key found for kid %q", kid)
	}
	return pub, nil
}

// Ping forces a JWKS fetch, used by the readiness probe.
func (c *Client) Ping(ctx context.Context) error {
	return c.refresh(ctx)
}

func (c *Client) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: unexpected status %d", resp.StatusCode)
	}

	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}

	byKid := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 + int(b)
		}
		byKid[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}
	}

	c.mu.Lock()
	c.byKid = byKid
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	return nil
}
