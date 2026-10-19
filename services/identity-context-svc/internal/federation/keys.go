package federation

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
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// JWKSFetcher retrieves a JWKS document. The production implementation is HTTPFetcher over the shared
// egress-guarded client; tests supply their own.
type JWKSFetcher interface {
	Fetch(ctx context.Context, uri string) ([]byte, error)
}

// HTTPFetcher fetches a JWKS over an HTTP client that must already enforce the egress policy (the
// shared egress.Policy.NewHTTPClient: pinned dialer, no redirects, no proxy).
type HTTPFetcher struct {
	Client   *http.Client
	MaxBytes int64
}

// Fetch performs one bounded GET. Anything but a 200 with a JSON body within the size limit is an error.
func (f *HTTPFetcher) Fetch(ctx context.Context, uri string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint answered %d", resp.StatusCode)
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = 256 << 10
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("jwks read failed: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, errors.New("jwks document is too large")
	}
	return body, nil
}

// KeyRepo is the persistence the key manager needs.
type KeyRepo interface {
	LoadKeySet(ctx context.Context, tenantID, connectionID string) (*KeySetRecord, error)
	SaveKeySet(ctx context.Context, rec KeySetRecord) error
	RecordKeyFetchFailure(ctx context.Context, tenantID, connectionID, jwksURI, message string, at time.Time) error
}

// Health lets the key manager report a connection as degraded or recovered. Implementations change only
// ACTIVE <-> DEGRADED and write the transition's event with it.
type Health interface {
	SetKeyHealth(ctx context.Context, tenantID, connectionID string, degraded bool, reason string) error
}

// KeyManager resolves the public key a token's signature is checked against.
//
// The approved JWKS URI is the only source. A key set is refreshed when it is older than RefreshAfter,
// when it was fetched from a different URI than the connection now names, or when a token names a key id
// it does not hold (at most once per MinRefresh, so a forged kid cannot make this service hammer the IdP).
// When the IdP cannot be reached the connection's own outage behaviour decides: FAIL_CLOSED refuses, and
// CACHED_KEYS keeps verifying against the last good set until its grace period ends. A key set that
// fetched but held no usable key never replaces a good one.
type KeyManager struct {
	repo         KeyRepo
	fetcher      JWKSFetcher
	health       Health
	log          *zap.Logger
	RefreshAfter time.Duration
	MinRefresh   time.Duration
	FetchTimeout time.Duration
	now          func() time.Time
	flight       singleflight.Group
	mu           sync.Mutex
	failedAt     map[string]time.Time
}

// NewKeyManager builds a manager with production defaults.
func NewKeyManager(repo KeyRepo, fetcher JWKSFetcher, health Health, log *zap.Logger) *KeyManager {
	if log == nil {
		log = zap.NewNop()
	}
	return &KeyManager{repo: repo, fetcher: fetcher, health: health, log: log,
		RefreshAfter: 15 * time.Minute, MinRefresh: 30 * time.Second, FetchTimeout: 8 * time.Second, now: time.Now,
		failedAt: map[string]time.Time{}}
}

// SetClock replaces the clock; for tests.
func (m *KeyManager) SetClock(now func() time.Time) { m.now = now }

// ResolvedKey is a key accepted for a token.
type ResolvedKey struct {
	Key      crypto.PublicKey
	KeyID    string
	Degraded bool
}

// Resolve returns the verification key for a token header, or an error. ErrKeysUnavailable means the
// answer could not be decided (an outage); any other error means the token names no acceptable key.
func (m *KeyManager) Resolve(ctx context.Context, conn Connection, cfg Config, kid, alg string) (*ResolvedKey, error) {
	now := m.now()
	rec, err := m.repo.LoadKeySet(ctx, conn.TenantID, conn.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	}
	sameURI := rec != nil && rec.JWKSURI == cfg.JWKSURI
	need, forced := false, false
	switch {
	case !sameURI:
		need, forced = true, true
	case now.Sub(rec.FetchedAt) > m.RefreshAfter:
		need = true
	case kid != "" && !hasKey(rec.JWKS, kid) && now.Sub(rec.LastAttemptAt) >= m.MinRefresh:
		need = true
	}

	degraded := false
	if need {
		fresh, ferr := m.refetch(ctx, conn, cfg)
		switch {
		case ferr == nil:
			rec, sameURI = fresh, true
			m.reportHealth(ctx, conn, false, "")
		case sameURI && !forced && now.Sub(rec.FetchedAt) <= m.RefreshAfter:
			// the cache is still within its normal life: this refresh was only a look for an unknown key id
		case sameURI && cfg.OutageBehavior == OutageCachedKeys &&
			now.Sub(rec.FetchedAt) <= time.Duration(cfg.KeyGraceSeconds)*time.Second:
			degraded = true
			m.reportHealth(ctx, conn, true, "signing keys could not be refreshed; verifying against the cached set")
		default:
			m.reportHealth(ctx, conn, true, "signing keys could not be refreshed and no usable cached set remains")
			return nil, fmt.Errorf("%w: %v", ErrKeysUnavailable, ferr)
		}
	}
	if rec == nil || !sameURI {
		return nil, ErrKeysUnavailable
	}
	k, err := pickKey(rec.JWKS, kid, alg)
	if err != nil {
		return nil, err
	}
	k.Degraded = degraded
	return k, nil
}

func (m *KeyManager) refetch(ctx context.Context, conn Connection, cfg Config) (*KeySetRecord, error) {
	// a connection with no stored key set has no failure timestamp to throttle on: remember the last
	// failed attempt in memory so a stream of logins cannot make this service hammer an unreachable IdP
	m.mu.Lock()
	if at, ok := m.failedAt[conn.ConnectionID]; ok && m.now().Sub(at) < m.MinRefresh {
		m.mu.Unlock()
		return nil, errors.New("a recent fetch failed; not retrying yet")
	}
	m.mu.Unlock()
	rec, err := m.fetchShared(ctx, conn, cfg)
	m.mu.Lock()
	if err != nil {
		m.failedAt[conn.ConnectionID] = m.now()
	} else {
		delete(m.failedAt, conn.ConnectionID)
	}
	m.mu.Unlock()
	return rec, err
}

func (m *KeyManager) fetchShared(ctx context.Context, conn Connection, cfg Config) (*KeySetRecord, error) {
	v, err, _ := m.flight.Do(conn.TenantID+"|"+conn.ConnectionID+"|"+cfg.JWKSURI, func() (any, error) {
		// one caller's cancellation must not fail every caller sharing this fetch
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.FetchTimeout)
		defer cancel()
		at := m.now()
		raw, err := m.fetcher.Fetch(fctx, cfg.JWKSURI)
		if err == nil {
			var n int
			if n = usableKeyCount(raw); n == 0 {
				err = errors.New("the key set holds no usable signing key")
			} else {
				rec := KeySetRecord{ConnectionID: conn.ConnectionID, TenantID: conn.TenantID, JWKSURI: cfg.JWKSURI,
					JWKS: raw, KeyCount: n, FetchedAt: at, LastAttemptAt: at}
				if serr := m.repo.SaveKeySet(fctx, rec); serr != nil {
					return nil, fmt.Errorf("could not store the key set: %w", serr)
				}
				return &rec, nil
			}
		}
		if rerr := m.repo.RecordKeyFetchFailure(fctx, conn.TenantID, conn.ConnectionID, cfg.JWKSURI, trim(err.Error(), 300), at); rerr != nil {
			m.log.Warn("could not record a key fetch failure", zap.String("connection_id", conn.ConnectionID), zap.Error(rerr))
		}
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	return v.(*KeySetRecord), nil
}

func (m *KeyManager) reportHealth(ctx context.Context, conn Connection, degraded bool, reason string) {
	if m.health == nil {
		return
	}
	// recovery is reported on every successful fetch; the store changes state only on a real transition
	if err := m.health.SetKeyHealth(context.WithoutCancel(ctx), conn.TenantID, conn.ConnectionID, degraded, reason); err != nil {
		m.log.Warn("could not record connection key health", zap.String("connection_id", conn.ConnectionID), zap.Error(err))
	}
}

// Refresh forces a fetch from the approved URI now, for an operator's refresh command.
func (m *KeyManager) Refresh(ctx context.Context, conn Connection, cfg Config) (int, error) {
	// an operator's explicit refresh is not throttled by an earlier automatic failure
	m.mu.Lock()
	delete(m.failedAt, conn.ConnectionID)
	m.mu.Unlock()
	rec, err := m.refetch(ctx, conn, cfg)
	if err != nil {
		m.reportHealth(ctx, conn, true, "signing keys could not be refreshed")
		return 0, fmt.Errorf("%w: %v", ErrKeysUnavailable, err)
	}
	m.reportHealth(ctx, conn, false, "")
	return rec.KeyCount, nil
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ── JWK parsing ──────────────────────────────────────────────────────────────

type jwk struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	KeyOps []string `json:"key_ops"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
}

type parsedKey struct {
	kid string
	alg string // the JWK's own alg, empty when it names none
	key crypto.PublicKey
}

func parseJWKS(raw []byte) []parsedKey {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if json.Unmarshal(raw, &set) != nil {
		return nil
	}
	var out []parsedKey
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if len(k.KeyOps) > 0 && !contains(k.KeyOps, "verify") {
			continue
		}
		var pub crypto.PublicKey
		switch k.Kty {
		case "RSA":
			pub = rsaKey(k)
		case "EC":
			pub = ecKey(k)
		}
		if pub != nil {
			out = append(out, parsedKey{kid: k.Kid, alg: k.Alg, key: pub})
		}
	}
	return out
}

func usableKeyCount(raw []byte) int { return len(parseJWKS(raw)) }

func hasKey(raw []byte, kid string) bool {
	for _, k := range parseJWKS(raw) {
		if k.kid == kid {
			return true
		}
	}
	return false
}

func b64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil
	}
	return b
}

func rsaKey(k jwk) crypto.PublicKey {
	n, e := b64(k.N), b64(k.E)
	if len(n) == 0 || len(e) == 0 || len(e) > 4 {
		return nil
	}
	mod := new(big.Int).SetBytes(n)
	exp := new(big.Int).SetBytes(e)
	// a short modulus is a forgeable key; refuse it however the IdP published it
	if mod.BitLen() < 2048 || !exp.IsInt64() || exp.Int64() < 3 || exp.Int64()%2 == 0 {
		return nil
	}
	return &rsa.PublicKey{N: mod, E: int(exp.Int64())}
}

func ecKey(k jwk) crypto.PublicKey {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	default:
		return nil
	}
	x, y := new(big.Int).SetBytes(b64(k.X)), new(big.Int).SetBytes(b64(k.Y))
	if x.Sign() == 0 || y.Sign() == 0 || !curve.IsOnCurve(x, y) {
		return nil
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}
}

// ErrUnknownKey means the token names a key the connection's key set does not hold, or one that cannot
// verify the algorithm the token claims.
var ErrUnknownKey = errors.New("no acceptable signing key for the token")

func pickKey(raw []byte, kid, alg string) (*ResolvedKey, error) {
	var candidates []parsedKey
	for _, k := range parseJWKS(raw) {
		if kid != "" && k.kid != kid {
			continue
		}
		if k.alg != "" && k.alg != alg {
			continue
		}
		if !algFits(alg, k.key) {
			continue
		}
		candidates = append(candidates, k)
	}
	// without a key id the key is unambiguous only when exactly one fits
	if len(candidates) != 1 {
		return nil, ErrUnknownKey
	}
	return &ResolvedKey{Key: candidates[0].key, KeyID: candidates[0].kid}, nil
}

// algFits ties the algorithm family to the key type and curve, so an RSA key can never be asked to
// verify an ECDSA signature or the reverse.
func algFits(alg string, key crypto.PublicKey) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		switch alg {
		case "ES256":
			return k.Curve == elliptic.P256()
		case "ES384":
			return k.Curve == elliptic.P384()
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
