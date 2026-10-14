package identity

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWKS fetches and caches identity-context-svc's envelope signing keys by kid.
// It never sees or holds a private key.
//
// The same client gateway-auth-svc runs, with one addition: an unknown kid
// triggers a refetch at most once per minRefresh. Without that bound a caller
// could send envelopes with random kids and turn every request into a fetch
// against identity-context-svc.
type JWKS struct {
	url        string
	ttl        time.Duration
	minRefresh time.Duration
	httpClient *http.Client

	mu          sync.Mutex
	byKid       map[string]*rsa.PublicKey
	fetchedAt   time.Time
	attemptedAt time.Time
}

// ErrKeysUnavailable: the key set could not be read and nothing is cached for
// the kid. identity-context-svc is down; every asserted request fails.
// ErrKeyNotFound: the set was read and has no such kid (a rotation not yet
// seen, or a forged kid).
var (
	ErrKeysUnavailable = errors.New("identity: key set unavailable")
	ErrKeyNotFound     = errors.New("identity: no key for kid")
)

// NewJWKS builds a client. httpClient may be nil (a 5 s default).
func NewJWKS(url string, ttl time.Duration, httpClient *http.Client) *JWKS {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &JWKS{url: url, ttl: ttl, minRefresh: 10 * time.Second, httpClient: httpClient,
		byKid: map[string]*rsa.PublicKey{}}
}

// PublicKey returns the key for kid, refreshing a stale cache or an unknown
// kid (rate-limited).
func (c *JWKS) PublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	pub, known := c.byKid[kid]
	stale := time.Since(c.fetchedAt) > c.ttl
	recentlyTried := time.Since(c.attemptedAt) < c.minRefresh
	c.mu.Unlock()

	if known && !stale {
		return pub, nil
	}
	if !known && !stale && recentlyTried {
		return nil, fmt.Errorf("%w %q", ErrKeyNotFound, kid)
	}
	if err := c.refresh(ctx); err != nil {
		// A blip against identity-context-svc must not refuse every request
		// when a trusted key for this kid is already held: verification
		// itself does not depend on cache age.
		if known {
			return pub, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if pub, known = c.byKid[kid]; !known {
		return nil, fmt.Errorf("%w %q", ErrKeyNotFound, kid)
	}
	return pub, nil
}

// Ping forces a fetch (readiness and startup log).
func (c *JWKS) Ping(ctx context.Context) error { return c.refresh(ctx) }

func (c *JWKS) refresh(ctx context.Context) error {
	c.mu.Lock()
	c.attemptedAt = time.Now()
	c.mu.Unlock()

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
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	byKid := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 + int(b)
		}
		byKid[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: e}
	}
	c.mu.Lock()
	c.byKid, c.fetchedAt = byKid, time.Now()
	c.mu.Unlock()
	return nil
}
