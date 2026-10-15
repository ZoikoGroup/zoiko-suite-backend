package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 13 Oct 2026: the edge forwards the session facts the signed envelope
// carries, in place of what the caller wrote (S1-B1 source channel; the
// assurance and support context authorization-svc reads).

func verifyClaims(t *testing.T, claims jwt.MapClaims, clientHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h, key, _ := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, claims))
	for k, v := range clientHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec
}

// S1-B1: the verified channel wins over the caller's.
func TestVerify_SourceChannelFromEnvelopeOverridesClient(t *testing.T) {
	claims := validClaims()
	claims["source_channel"] = "web"
	rec := verifyClaims(t, claims, map[string]string{"X-Source-Channel": "system"})
	assert.Equal(t, "web", rec.Header().Get("X-Source-Channel"))
	assert.Equal(t, "verified", rec.Header().Get("X-Source-Channel-Basis"))
}

// No channel on the envelope: the caller's value is passed on, labelled.
func TestVerify_SourceChannelAssertedWhenEnvelopeNamesNone(t *testing.T) {
	rec := verifyClaims(t, validClaims(), map[string]string{"X-Source-Channel": "mobile"})
	assert.Equal(t, "mobile", rec.Header().Get("X-Source-Channel"))
	assert.Equal(t, "asserted", rec.Header().Get("X-Source-Channel-Basis"))

	rec = verifyClaims(t, validClaims(), nil)
	assert.Empty(t, rec.Header().Get("X-Source-Channel"))
	assert.Empty(t, rec.Header().Get("X-Source-Channel-Basis"))
}

func TestVerify_AssuranceLevelFromSessionTrust(t *testing.T) {
	for mfa, want := range map[bool]string{true: "MFA", false: "SINGLE_FACTOR"} {
		claims := validClaims()
		claims["session_trust_posture"] = map[string]any{"mfa_verified": mfa}
		rec := verifyClaims(t, claims, map[string]string{"X-Assurance-Level": "PHISHING_RESISTANT"})
		assert.Equal(t, want, rec.Header().Get("X-Assurance-Level"), "mfa_verified=%v", mfa)
	}
	// No posture claim: nothing asserted (and Traefik strips the client's copy).
	rec := verifyClaims(t, validClaims(), map[string]string{"X-Assurance-Level": "PHISHING_RESISTANT"})
	assert.Empty(t, rec.Header().Get("X-Assurance-Level"))
}

func TestVerify_SupportContextOnlyFromEnvelope(t *testing.T) {
	claims := validClaims()
	claims["support_context_id"] = "sup-verified-1"
	rec := verifyClaims(t, claims, map[string]string{"X-Support-Context-Id": "sup-forged"})
	assert.Equal(t, "sup-verified-1", rec.Header().Get("X-Support-Context-Id"))

	rec = verifyClaims(t, validClaims(), map[string]string{"X-Support-Context-Id": "sup-forged"})
	assert.Empty(t, rec.Header().Get("X-Support-Context-Id"), "a client-written support context must never be echoed")
}

// The envelope's iat is when it was minted, not when the user logged in, so
// the gateway never claims a login age; the header is only ever stripped.
func TestVerify_NeverSetsAuthnAge(t *testing.T) {
	rec := verifyClaims(t, validClaims(), map[string]string{"X-Authn-Age-Seconds": "1"})
	assert.Empty(t, rec.Header().Get("X-Authn-Age-Seconds"))
}
