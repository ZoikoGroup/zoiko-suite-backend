package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/keys"
)

// TestVerifyBearerRefusals restores the refusal cases the 15 Oct keyring
// rewrite deleted with jwt_test.go and idp_test.go (Group 1 re-audit F-7,
// 19 Oct 2026). Every claim VerifyBearer checks has a case here: with any one
// check removed, its case fails.
func TestVerifyBearerRefusals(t *testing.T) {
	ring, err := keys.NewKeyring(rsaSigner(t, "k1"))
	require.NoError(t, err)
	v := auth.NewJWTVerifier(testCfg, ring)
	now := time.Now()

	base := func() map[string]any {
		return map[string]any{
			"sub": "sub-1", "tenant_id": "t1", "iss": "identity-context-svc", "aud": auth.IdPTokenAudience,
			"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": "j-1",
		}
	}
	sign := func(c map[string]any) string {
		tok, err := ring.Sign(c, auth.TypeAccessToken)
		require.NoError(t, err)
		return tok
	}

	// Sanity: the base token is accepted, so each refusal below is caused by
	// the one thing it changes.
	vc, err := v.VerifyBearer(context.Background(), sign(base()))
	require.NoError(t, err)
	require.Equal(t, "sub-1", vc.Subject)
	require.Equal(t, "t1", vc.TenantID)

	cases := map[string]func(map[string]any){
		"wrong audience":            func(c map[string]any) { c["aud"] = "zoiko-internal" },
		"wrong issuer":              func(c map[string]any) { c["iss"] = "someone-else" },
		"expired":                   func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() },
		"no exp":                    func(c map[string]any) { delete(c, "exp") },
		"issued in the future":      func(c map[string]any) { c["iat"] = now.Add(10 * time.Minute).Unix() },
		"missing sub":               func(c map[string]any) { delete(c, "sub") },
		"missing tenant_id":         func(c map[string]any) { delete(c, "tenant_id") },
		"auth_time in future":       func(c map[string]any) { c["auth_time"] = now.Add(10 * time.Minute).Unix() },
		"neither iat nor auth_time": func(c map[string]any) { delete(c, "iat") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(c)
			_, err := v.VerifyBearer(context.Background(), sign(c))
			require.Error(t, err)
		})
	}

	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(sign(base()), ".")
		forged, _ := json.Marshal(map[string]any{"sub": "admin", "tenant_id": "t1", "iss": "identity-context-svc",
			"aud": auth.IdPTokenAudience, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
		parts[1] = base64.RawURLEncoding.EncodeToString(forged)
		_, err := v.VerifyBearer(context.Background(), strings.Join(parts, "."))
		require.Error(t, err, "a payload swapped under the original signature")
	})

	t.Run("alg none", func(t *testing.T) {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"at+jwt","kid":"k1"}`))
		body, _ := json.Marshal(base())
		tok := hdr + "." + base64.RawURLEncoding.EncodeToString(body) + "."
		_, err := v.VerifyBearer(context.Background(), tok)
		require.Error(t, err)
	})

	t.Run("unknown kid", func(t *testing.T) {
		other, err := keys.NewKeyring(rsaSigner(t, "k-unknown"))
		require.NoError(t, err)
		tok, err := other.Sign(base(), auth.TypeAccessToken)
		require.NoError(t, err)
		_, err = v.VerifyBearer(context.Background(), tok)
		require.Error(t, err)
	})
}
