package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"

	"zoiko.io/identity-context-svc/internal/domain"
)

// acr values for the assurance taxonomy (IAM §31 "session assurance
// taxonomy"): what the ID token's acr says about how the person signed in.
const (
	acrSingle            = "urn:zoikosuite:acr:single-factor"
	acrMFA               = "urn:zoikosuite:acr:mfa"
	acrPhishingResistant = "urn:zoikosuite:acr:phishing-resistant"
)

func acrFor(assurance string) string {
	switch assurance {
	case domain.AssurancePhishingResistant:
		return acrPhishingResistant
	case domain.AssuranceMFA:
		return acrMFA
	default:
		return acrSingle
	}
}

// atHash is OIDC Core §3.1.3.6's at_hash for an RS256/ES256 token.
func atHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func hasScope(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// mintIDToken signs an ID token for p's login session.
func (p *Provider) mintIDToken(c *Client, pr *domain.Principal, sess *domain.AuthSession, nonce, scope, accessToken string) (string, error) {
	now := p.o.Now()
	claims := map[string]any{
		"iss":       p.issuer,
		"sub":       pr.PrincipalID,
		"aud":       c.ClientID,
		"azp":       c.ClientID,
		"iat":       now.Unix(),
		"exp":       now.Add(p.ap.IDTokenTTL).Unix(),
		"auth_time": sess.AuthTime.Unix(),
		"amr":       sess.AMR,
		"acr":       acrFor(sess.AssuranceLevel),
		"sid":       sess.SID,
		"tenant_id": pr.TenantID,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if accessToken != "" {
		claims["at_hash"] = atHash(accessToken)
	}
	if hasScope(scope, "email") {
		claims["email"] = pr.Email
	}
	if hasScope(scope, "profile") {
		claims["name"] = pr.DisplayName
	}
	return p.o.Ring.Sign(claims, "JWT")
}

// sessionAge reports how long ago the login session authenticated.
func sessionAge(sess *domain.AuthSession, now time.Time) time.Duration {
	return now.Sub(sess.AuthTime)
}
