package idenv

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
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

// KeySource resolves an envelope signing key by kid.
type KeySource interface {
	PublicKey(ctx context.Context, kid string) (crypto.PublicKey, error)
}

// ErrKeysUnavailable: the key set could not be read and nothing is cached for
// the kid. ErrKeyNotFound: the set was read and has no such kid (a rotation
// not yet published, or a forged kid).
var (
	ErrKeysUnavailable = errors.New("idenv: key set unavailable")
	ErrKeyNotFound     = errors.New("idenv: no key for kid")
)

// JWKS fetches identity-context-svc's published signing keys and caches them
// by kid. It never sees a private key.
//
// identity-context publishes the active key and, during a rotation, the next
// and the previous one (AU-05), so an envelope signed by either verifies here
// without a restart. An unknown kid triggers a refetch at most once per
// MinRefresh, so random kids cannot turn every request into a key fetch.
type JWKS struct {
	url        string
	ttl        time.Duration
	minRefresh time.Duration
	client     *http.Client

	mu          sync.Mutex
	byKid       map[string]crypto.PublicKey
	fetchedAt   time.Time
	attemptedAt time.Time
}

// NewJWKS builds a key set client. client may be nil (5 s timeout).
func NewJWKS(url string, ttl time.Duration, client *http.Client) *JWKS {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &JWKS{url: url, ttl: ttl, minRefresh: 10 * time.Second, client: client,
		byKid: map[string]crypto.PublicKey{}}
}

// PublicKey returns the key for kid, refreshing a stale cache or an unknown
// kid (rate-limited). A refresh failure keeps serving a key already trusted.
func (c *JWKS) PublicKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
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

// Ping forces a fetch (readiness probes, startup log).
func (c *JWKS) Ping(ctx context.Context) error { return c.refresh(ctx) }

// Kids lists the cached key ids (startup log, tests).
func (c *JWKS) Kids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byKid))
	for k := range c.byKid {
		out = append(out, k)
	}
	return out
}

// JWK is one published key.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

func (c *JWKS) refresh(ctx context.Context) error {
	c.mu.Lock()
	c.attemptedAt = time.Now()
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: unexpected status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []JWK `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	byKid := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.PublicKey()
		if err != nil {
			continue
		}
		byKid[k.Kid] = pub
	}
	c.mu.Lock()
	c.byKid, c.fetchedAt = byKid, time.Now()
	c.mu.Unlock()
	return nil
}

// PublicKey decodes the JWK (RSA or EC P-256).
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eb {
			e = e<<8 + int(b)
		}
		if e < 3 || len(n) < 256 {
			return nil, errors.New("RSA key too small")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: e}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, err
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, errors.New("EC point not on curve")
		}
		return pub, nil
	default:
		return nil, fmt.Errorf("unsupported kty %q", k.Kty)
	}
}

// JWKFromPublicKey renders a public key as a JWK (identity-context's JWKS
// endpoint, and DPoP thumbprints).
func JWKFromPublicKey(kid string, pub crypto.PublicKey) (JWK, error) {
	switch p := pub.(type) {
	case *rsa.PublicKey:
		return JWK{Kty: "RSA", Kid: kid, Use: "sig", Alg: "RS256",
			N: base64.RawURLEncoding.EncodeToString(p.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.E)).Bytes())}, nil
	case *ecdsa.PublicKey:
		if p.Curve != elliptic.P256() {
			return JWK{}, errors.New("only P-256 EC keys are supported")
		}
		return JWK{Kty: "EC", Kid: kid, Use: "sig", Alg: "ES256", Crv: "P-256",
			X: base64.RawURLEncoding.EncodeToString(pad32(p.X.Bytes())),
			Y: base64.RawURLEncoding.EncodeToString(pad32(p.Y.Bytes()))}, nil
	default:
		return JWK{}, fmt.Errorf("unsupported public key type %T", pub)
	}
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// StaticKeys is a KeySource over fixed keys (identity-context verifying its
// own envelopes, and tests).
type StaticKeys map[string]crypto.PublicKey

// PublicKey implements KeySource.
func (s StaticKeys) PublicKey(_ context.Context, kid string) (crypto.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w %q", ErrKeyNotFound, kid)
}
