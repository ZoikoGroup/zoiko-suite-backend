package federation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	tIssuer   = "https://idp.example.com"
	tAudience = "zoiko-client"
	tTenant   = "tenant-1"
	tConn     = "conn-1"
)

// testIdP is a stand-in identity provider: it owns signing keys, publishes them as a JWKS and mints tokens.
type testIdP struct {
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	kid    string
	ecKid  string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testIdP{rsaKey: r, ecKey: e, kid: "rsa-1", ecKid: "ec-1"}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, k *rsa.PublicKey) map[string]any {
	return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64u(k.N.Bytes()), "e": b64u(big.NewInt(int64(k.E)).Bytes())}
}

func ecJWK(kid string, k *ecdsa.PublicKey) map[string]any {
	return map[string]any{"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64u(k.X.FillBytes(make([]byte, 32))), "y": b64u(k.Y.FillBytes(make([]byte, 32)))}
}

func jwksDoc(keys ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return b
}

func (i *testIdP) jwks() []byte {
	return jwksDoc(rsaJWK(i.kid, &i.rsaKey.PublicKey), ecJWK(i.ecKid, &i.ecKey.PublicKey))
}

// claims returns a valid set of claims for the default connection, to be altered per test.
func (i *testIdP) claims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": tIssuer, "aud": tAudience, "sub": "user-123", "nonce": "n-1",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

func (i *testIdP) mint(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = i.kid
	s, err := tok.SignedString(i.rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (i *testIdP) mintEC(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = i.ecKid
	s, err := tok.SignedString(i.ecKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testConfig() Config {
	return Config{Issuer: tIssuer, Audience: tAudience, JWKSURI: "https://idp.example.com/jwks",
		AllowedAlgs: []string{"RS256", "ES256"}}.Normalize()
}

func testConnection() Connection {
	v := 1
	return Connection{ConnectionID: tConn, TenantID: tTenant, Name: "corp", Protocol: "OIDC", Status: ConnActive, ActiveVersion: &v, LatestVersion: 1}
}

// fakeFetcher serves a JWKS document and counts fetches.
type fakeFetcher struct {
	mu    sync.Mutex
	doc   []byte
	err   error
	calls int
	delay time.Duration
}

func (f *fakeFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	doc, err, d := f.doc, f.err, f.delay
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return doc, err
}

func (f *fakeFetcher) set(doc []byte, err error) { f.mu.Lock(); f.doc, f.err = doc, err; f.mu.Unlock() }
func (f *fakeFetcher) count() int                { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// memRepo is the part of Repo the key manager and the login use, in memory. Anything else panics, which
// is how a test learns it reached for something it did not mean to.
type memRepo struct {
	Repo
	mu       sync.Mutex
	keySet   *KeySetRecord
	health   []string
	conn     *Connection
	ver      *Version
	links    map[string]*Link
	used     map[string]bool
	logins   []LoginRecord
	denied   []LoginRecord
	findErr  error
	keyLoads int
}

func newMemRepo() *memRepo { return &memRepo{links: map[string]*Link{}, used: map[string]bool{}} }

func (m *memRepo) LoadKeySet(_ context.Context, _, _ string) (*KeySetRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyLoads++
	if m.keySet == nil {
		return nil, nil
	}
	c := *m.keySet
	return &c, nil
}

func (m *memRepo) SaveKeySet(_ context.Context, rec KeySetRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keySet = &rec
	return nil
}

func (m *memRepo) RecordKeyFetchFailure(_ context.Context, _, _, _, _ string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keySet != nil {
		m.keySet.LastAttemptAt = at
		m.keySet.ConsecutiveFailures++
	}
	return nil
}

func (m *memRepo) SetKeyHealth(_ context.Context, _, _ string, degraded bool, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if degraded {
		m.health = append(m.health, "degraded")
	} else {
		m.health = append(m.health, "ok")
	}
	return nil
}

func (m *memRepo) FindLoginConnection(_ context.Context, tenantID, issuer string) (*Connection, *Version, error) {
	if m.findErr != nil {
		return nil, nil, m.findErr
	}
	if m.conn == nil || tenantID != m.conn.TenantID || issuer != m.ver.Config.Issuer {
		return nil, nil, nil
	}
	return m.conn, m.ver, nil
}

func (m *memRepo) FindLink(_ context.Context, tenantID, issuer, subject string) (*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[tenantID+"|"+issuer+"|"+subject], nil
}

func (m *memRepo) CompleteLogin(_ context.Context, rec LoginRecord, _, tokenHash string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used[tokenHash] {
		return ErrTokenReplayed
	}
	m.used[tokenHash] = true
	m.logins = append(m.logins, rec)
	return nil
}

func (m *memRepo) RecordDenied(_ context.Context, rec LoginRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.denied = append(m.denied, rec)
	return nil
}

func (m *memRepo) lastHealth() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.health) == 0 {
		return ""
	}
	return m.health[len(m.health)-1]
}

var errBoom = errors.New("boom")
