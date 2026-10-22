package idenv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalid is any envelope that does not verify.
var ErrInvalid = errors.New("identity envelope invalid")

// ErrWrongAudience is an envelope that verifies but was issued for another
// relying party (IAM A18). Wrapped in ErrInvalid.
var ErrWrongAudience = errors.New("identity envelope issued for another audience")

// Verifier checks an envelope's signature, issuer, audience and lifetime.
type Verifier struct {
	keys      KeySource
	issuer    string
	audiences []string
	legacy    bool
	leeway    time.Duration
	now       func() time.Time
}

// VerifierOptions configures NewVerifier.
type VerifierOptions struct {
	Keys   KeySource
	Issuer string
	// Audience is this relying party's own audience (its service name).
	Audience string
	// ExtraAudiences are further audiences this relying party answers to,
	// e.g. gateway-auth-svc's edge audience.
	ExtraAudiences []string
	// AcceptLegacyAudience admits envelopes minted for LegacyAudience, the
	// shared audience every envelope carried before AU-12. A migration state:
	// it lets a callers' envelope reach this service before the edge exchanges
	// it for this audience.
	AcceptLegacyAudience bool
	// Leeway is the clock-skew allowance on exp/nbf/iat (default 30 s).
	Leeway time.Duration
	Now    func() time.Time
}

// NewVerifier builds a verifier.
func NewVerifier(o VerifierOptions) (*Verifier, error) {
	if o.Keys == nil {
		return nil, errors.New("idenv: verifier needs a key source")
	}
	if strings.TrimSpace(o.Issuer) == "" {
		return nil, errors.New("idenv: verifier needs an issuer")
	}
	var auds []string
	for _, a := range append([]string{o.Audience}, o.ExtraAudiences...) {
		if a = strings.TrimSpace(a); a != "" {
			auds = append(auds, a)
		}
	}
	if len(auds) == 0 && !o.AcceptLegacyAudience {
		return nil, errors.New("idenv: verifier needs an audience")
	}
	if o.Leeway <= 0 {
		o.Leeway = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Verifier{keys: o.Keys, issuer: o.Issuer, audiences: auds, legacy: o.AcceptLegacyAudience,
		leeway: o.Leeway, now: o.Now}, nil
}

// Audiences are the audiences this verifier accepts (startup log).
func (v *Verifier) Audiences() []string {
	out := append([]string(nil), v.audiences...)
	if v.legacy {
		out = append(out, LegacyAudience)
	}
	return out
}

// Verify parses and checks token. Any failure is an error wrapping ErrInvalid,
// never partial claims.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg():
		default:
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		// An OAuth access token (typ at+jwt) is for identity-context's resolve
		// endpoint, never a relying party, even if its audience were forged
		// to match: refusing the type closes that confusion outright.
		if typ, _ := t.Header["typ"].(string); strings.EqualFold(typ, "at+jwt") || strings.EqualFold(typ, "dpop+jwt") {
			return nil, fmt.Errorf("token type %q is not an identity envelope", typ)
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("envelope has no kid")
		}
		return v.keys.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(v.issuer),
		jwt.WithLeeway(v.leeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if !v.audienceOK(claims.Audience) {
		return nil, errors.Join(ErrInvalid, ErrWrongAudience)
	}
	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		return nil, errors.Join(ErrInvalid, errors.New("envelope names no principal or tenant"))
	}
	if claims.Principal.TenantID != "" && claims.Principal.TenantID != claims.TenantID {
		return nil, errors.Join(ErrInvalid, errors.New("envelope principal and tenant disagree"))
	}
	if claims.AuthTime > 0 && time.Unix(claims.AuthTime, 0).After(v.now().Add(v.leeway)) {
		return nil, errors.Join(ErrInvalid, errors.New("envelope auth_time is in the future"))
	}
	return claims, nil
}

func (v *Verifier) audienceOK(got jwt.ClaimStrings) bool {
	for _, a := range got {
		for _, want := range v.audiences {
			if a == want {
				return true
			}
		}
		if v.legacy && a == LegacyAudience {
			return true
		}
	}
	return false
}
