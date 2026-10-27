package federation

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
)

// ErrTokenRejected is wrapped by every verification failure. The reason inside is for evidence and the
// SIEM stream; a login caller sees only the uniform rejection.
var ErrTokenRejected = errors.New("federated token rejected")

// RejectError carries the internal reason a token was refused.
type RejectError struct{ Reason string }

func (e *RejectError) Error() string { return "federated token rejected: " + e.Reason }
func (e *RejectError) Unwrap() error { return ErrTokenRejected }

func reject(reason string) error { return &RejectError{Reason: reason} }

// ReasonOf extracts the internal reason from a verification error, or "".
func ReasonOf(err error) string {
	var r *RejectError
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// maxTokenBytes bounds what is parsed. A signed ID token is a few kilobytes; nothing legitimate needs more.
const maxTokenBytes = 16 << 10

// Verified is what a successfully verified ID token asserts.
type Verified struct {
	Issuer    string
	Subject   string
	KeyID     string
	MFA       bool
	Degraded  bool
	IssuedAt  time.Time
	ExpiresAt time.Time
	TokenHash string

	// What the token says about the person, used only for just-in-time provisioning of a connection that opts in.
	// The email is informational: it is never how a person is found or bound.
	Email       string
	DisplayName string
	Attrs       Attributes
}

// PeekIssuer reads the issuer a token CLAIMS without trusting it. It selects which connection's
// configuration to verify against; the verification itself then requires the claim to equal that
// connection's approved issuer exactly, under a signature from that connection's keys.
func PeekIssuer(token string) (string, error) {
	if token == "" || len(token) > maxTokenBytes {
		return "", reject("malformed")
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return "", reject("malformed")
	}
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return "", reject("malformed")
	}
	return iss, nil
}

// Verifier checks an ID token against one approved connection version.
type Verifier struct {
	keys *KeyManager
	now  func() time.Time
}

// NewVerifier builds a verifier over a key manager.
func NewVerifier(keys *KeyManager) *Verifier { return &Verifier{keys: keys, now: time.Now} }

// SetClock replaces the clock; for tests.
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

// Verify checks signature, issuer, audience, authorized party, lifetime, age, nonce and subject. It
// fails closed: every check must pass, and the result carries only what the token proved.
func (v *Verifier) Verify(ctx context.Context, conn Connection, cfg Config, token, nonce string) (*Verified, error) {
	if token == "" || len(token) > maxTokenBytes {
		return nil, reject("malformed")
	}
	header := jwt.MapClaims{}
	parsedHeader, _, err := jwt.NewParser().ParseUnverified(token, header)
	if err != nil {
		return nil, reject("malformed")
	}
	alg, _ := parsedHeader.Header["alg"].(string)
	if !contains(cfg.AllowedAlgs, alg) {
		return nil, reject("unsupported_alg")
	}
	kid, _ := parsedHeader.Header["kid"].(string)

	key, err := v.keys.Resolve(ctx, conn, cfg, kid, alg)
	switch {
	case errors.Is(err, ErrKeysUnavailable):
		return nil, err
	case err != nil:
		return nil, reject("unknown_key")
	}

	now := v.now()
	claims := jwt.MapClaims{}
	parsed, err := jwt.NewParser(
		jwt.WithValidMethods([]string{alg}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Duration(cfg.ClockSkewSeconds)*time.Second),
		jwt.WithTimeFunc(func() time.Time { return now }),
	).ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return key.Key, nil })
	if err != nil || !parsed.Valid {
		return nil, reject(classify(err))
	}

	// from here the signature has verified against the tenant's approved key: a refusal below is a refusal of
	// an authentic assertion, which is what durable evidence may be written for
	sum := sha256.Sum256([]byte(token))
	partial := &Verified{Issuer: cfg.Issuer, KeyID: key.KeyID, Degraded: key.Degraded, TokenHash: hex.EncodeToString(sum[:])}
	if raw, _ := claims[cfg.SubjectClaim].(string); raw != "" {
		partial.Subject = evidenceSubject(raw)
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return partial, reject("missing_iat")
	}
	skew := time.Duration(cfg.ClockSkewSeconds) * time.Second
	if iat.Time.After(now.Add(skew)) {
		return partial, reject("issued_in_future")
	}
	if now.Sub(iat.Time) > time.Duration(cfg.MaxTokenAgeSeconds)*time.Second+skew {
		return partial, reject("too_old")
	}
	exp, _ := claims.GetExpirationTime()

	// OIDC Core 3.1.3.7: with several audiences the authorized party must be this client
	if aud, _ := claims.GetAudience(); len(aud) > 1 {
		if azp, _ := claims["azp"].(string); azp != cfg.Audience {
			return partial, reject("azp_mismatch")
		}
	} else if azp, ok := claims["azp"].(string); ok && azp != "" && azp != cfg.Audience {
		return partial, reject("azp_mismatch")
	}

	if !cfg.NonceOptional {
		if nonce == "" {
			return partial, reject("nonce_missing")
		}
		got, _ := claims["nonce"].(string)
		if subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
			return partial, reject("nonce_mismatch")
		}
	}

	subject, ok := claims[cfg.SubjectClaim].(string)
	if !ok || !validSubject(subject) {
		return partial, reject("subject_missing")
	}

	out := &Verified{
		Issuer: cfg.Issuer, Subject: subject, KeyID: key.KeyID, Degraded: key.Degraded,
		IssuedAt: iat.Time, TokenHash: partial.TokenHash,
		MFA: mfaAsserted(cfg, claims),
	}
	out.Email, out.DisplayName, out.Attrs = directoryClaims(cfg, claims)
	if exp != nil {
		out.ExpiresAt = exp.Time
	}
	return out, nil
}

func classify(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "not_yet_valid"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "issuer_mismatch"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return "audience_mismatch"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "signature_invalid"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "missing_claim"
	default:
		return "invalid"
	}
}

// validSubject accepts a stable opaque identifier: non-empty, bounded, no whitespace or control
// characters. It deliberately does not look like an email check, because an address is not a subject.
func validSubject(s string) bool {
	if s == "" || len(s) > 255 || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// mfaAsserted reports whether the token proves a second factor under the VALUES THE TENANT APPROVED. An
// IdP's amr claim means nothing here unless it matches the connection's approved list.
func mfaAsserted(cfg Config, claims jwt.MapClaims) bool {
	if len(cfg.MFAAMRValues) > 0 {
		switch amr := claims["amr"].(type) {
		case []any:
			for _, a := range amr {
				if s, ok := a.(string); ok && contains(cfg.MFAAMRValues, s) {
					return true
				}
			}
		case string:
			if contains(cfg.MFAAMRValues, amr) {
				return true
			}
		}
	}
	if len(cfg.MFAACRValues) > 0 {
		if acr, ok := claims["acr"].(string); ok && contains(cfg.MFAACRValues, acr) {
			return true
		}
	}
	return false
}

// evidenceSubject makes a claimed subject safe to store as evidence when it is not valid enough to use:
// bounded, with control characters removed.
func evidenceSubject(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 255 {
			break
		}
	}
	return b.String()
}

// SignatureVerified reports whether a refusal happened AFTER the token's signature verified against the
// connection's approved keys. Durable evidence is written only for those; everything earlier is counted
// and streamed but never stored, so an anonymous sender cannot grow a table.
func SignatureVerified(v *Verified) bool { return v != nil }

// directoryClaims reads the directory facts a token carries. An address the IdP says is unverified is dropped.
func directoryClaims(cfg Config, claims jwt.MapClaims) (email, name string, attrs Attributes) {
	attrs = Attributes{}
	if e, ok := claims[cfg.EmailClaim].(string); ok {
		verified := true
		switch v := claims["email_verified"].(type) {
		case bool:
			verified = v
		case string:
			verified = strings.EqualFold(v, "true")
		}
		if verified {
			email = strings.TrimSpace(e)
		}
	}
	if n, ok := claims["name"].(string); ok && len(n) <= 255 {
		name = strings.TrimSpace(n)
	}
	for _, key := range []string{"groups", "roles"} {
		if list, ok := claims[key].([]any); ok {
			var vals []string
			for _, v := range list {
				if s, ok := v.(string); ok && s != "" && len(s) <= 255 && len(vals) < 200 {
					vals = append(vals, s)
				}
			}
			if len(vals) > 0 {
				attrs[key] = vals
			}
		}
	}
	if d, ok := claims["department"].(string); ok && d != "" && len(d) <= 255 {
		attrs["department"] = []string{d}
	}
	return email, name, attrs
}
