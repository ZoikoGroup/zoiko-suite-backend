package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/oidc"
)

func writeKey(t *testing.T, dir, name string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600))
	return p
}

// JWT_SIGNING_KEYS publishes more than one key, which is what lets a rotation
// keep live envelopes verifiable. Until 19 Oct the setting was loaded and read
// by nothing (Group 1 re-audit F-5).
func TestLoadKeyringHonoursJWTSigningKeys(t *testing.T) {
	dir := t.TempDir()
	oldKey, newKey := writeKey(t, dir, "old.pem"), writeKey(t, dir, "new.pem")
	cfg := &config.Config{JWTKeyID: "fallback", JWTSigningPrivateKeyPath: writeKey(t, dir, "fallback.pem")}
	cfg.AuthPlane.SigningKeys = "k-new=" + newKey + ",k-old=" + oldKey
	cfg.AuthPlane.SigningActiveKid = "k-new"

	ring, err := loadKeyring(cfg)
	require.NoError(t, err)
	assert.Equal(t, "k-new", ring.ActiveKid())
	assert.ElementsMatch(t, []string{"k-new", "k-old"}, ring.Kids(), "both keys are published during the rotation")
}

func TestLoadKeyringFallsBackToTheSinglePEM(t *testing.T) {
	cfg := &config.Config{JWTKeyID: "local-dev-key-1", JWTSigningPrivateKeyPath: writeKey(t, t.TempDir(), "k.pem")}
	ring, err := loadKeyring(cfg)
	require.NoError(t, err)
	assert.Equal(t, "local-dev-key-1", ring.ActiveKid())
	assert.Equal(t, []string{"local-dev-key-1"}, ring.Kids())
}

// The OAuth protocol endpoints bypass the §4 envelope; the probes and the
// policy's own ExemptPaths stay exempt; everything else is checked.
func TestProtocolExempt(t *testing.T) {
	exempt := protocolExempt([]string{"/v1/authenticate"})
	for path, want := range map[string]bool{
		"/oauth2/token": true, "/.well-known/openid-configuration": true, "/v1/context/session/introspect": true,
		"/v1/logout": true, "/health": true, "/readyz": true, "/v1/authenticate": true,
		"/v1/context/resolve": false, "/v1/principals/p-1/status": false,
	} {
		assert.Equal(t, want, exempt(httptest.NewRequest(http.MethodPost, path, nil)), path)
	}
}

// A token response must never be recorded by the idempotency replay store.
func TestSkipProtocolPathsKeepsTheReplayStoreOffTokenRoutes(t *testing.T) {
	reached := false
	replay := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; next.ServeHTTP(w, r) })
	}
	h := skipProtocolPaths(replay)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/oauth2/token", nil))
	assert.False(t, reached, "the token endpoint bypasses the replay store")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/context/support", nil))
	assert.True(t, reached, "a GOV-01 command still goes through it")
}

func TestMergeClientChannelsKeepsExplicitEntries(t *testing.T) {
	reg, err := oidc.NewRegistry(
		oidc.Client{ClientID: "spa", ClientType: oidc.ClientPublic, RedirectURIs: []string{"https://a/cb"},
			GrantTypes: []string{oidc.GrantAuthorizationCode}, SourceChannel: "web"},
		oidc.Client{ClientID: "mobile", ClientType: oidc.ClientPublic, RedirectURIs: []string{"https://b/cb"},
			GrantTypes: []string{oidc.GrantAuthorizationCode}, SourceChannel: "mobile"})
	require.NoError(t, err)
	cfg := &config.Config{IDPClientChannels: map[string]string{"spa": "api"}}
	mergeClientChannels(cfg, reg)
	assert.Equal(t, "api", cfg.IDPClientChannels["spa"], "IDP_CLIENT_CHANNELS wins")
	assert.Equal(t, "mobile", cfg.IDPClientChannels["mobile"])
}
