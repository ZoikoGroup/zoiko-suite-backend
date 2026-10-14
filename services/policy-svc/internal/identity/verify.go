package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// The consumer-side half of X5 (Governance audit cross-cutting defect; Identity
// audit register R-3): X-Tenant-Id and X-Principal-Id used to be believed
// exactly as sent, so anyone who reached the port directly could act as any
// principal in any tenant. A request is now identity-verified when either
//
//   - it carries identity-context-svc's signed envelope (Authorization:
//     Bearer, RS256, keys from its JWKS, issuer and audience pinned), and the
//     identity headers agree with the envelope's claims — headers it does not
//     carry are filled from the claims; or
//   - it arrived on the mTLS listener with a client certificate the platform
//     CA issued (a peer service, which asserts the headers the way the gateway
//     does).
//
// Anything else is refused in enforce mode. warn checks the same and admits,
// logging what enforce would refuse — a migration state while callers start
// forwarding the envelope, never a resting one.

// Mode is the verification mode.
type Mode string

const (
	Off     Mode = "off"
	Warn    Mode = "warn"
	Enforce Mode = "enforce"
)

// ParseMode reads a configured mode. Anything unrecognised enforces, so a typo
// cannot disable the control.
func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case Off:
		return Off
	case Warn:
		return Warn
	default:
		return Enforce
	}
}

// KeySource resolves a signing key by kid (KeySet in production).
type KeySource interface {
	PublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Verifier checks each request's identity.
type Verifier struct {
	mode     Mode
	keys     KeySource
	issuer   string
	audience string
	log      *zap.Logger
}

// NewVerifier builds a verifier. keys may be nil only in Off mode.
func NewVerifier(mode Mode, keys KeySource, issuer, audience string, log *zap.Logger) *Verifier {
	return &Verifier{mode: mode, keys: keys, issuer: issuer, audience: audience, log: log}
}

// envelopeClaims mirrors the fields gateway-auth-svc reads from
// identity-context-svc's envelope.
type envelopeClaims struct {
	Principal struct {
		PrincipalID string `json:"principal_id"`
	} `json:"principal"`
	TenantID string `json:"tenant_id"`
	jwt.RegisteredClaims
}

// BasisHeader names how the request's identity was established, for the
// handlers' logs and for anyone reading a request trace.
const BasisHeader = "X-Identity-Basis"

const (
	basisEnvelope = "verified_envelope"
	basisPeer     = "mtls_peer"
	basisAsserted = "asserted"
)

var errMismatch = errors.New("identity headers disagree with the signed envelope")

func exempt(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/health", "/metrics":
		return true
	}
	return false
}

func (v *Verifier) verify(ctx context.Context, raw string) (*envelopeClaims, error) {
	claims := &envelopeClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, errors.New("unexpected signing method")
		}
		kid, ok := tok.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("token missing kid header")
		}
		return v.keys.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
	)
	if err != nil {
		return nil, err
	}
	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		return nil, errors.New("envelope has no principal or tenant")
	}
	return claims, nil
}

func bearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}

func refuse(w http.ResponseWriter, legacy, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": legacy, "code": "CONTEXT_UNRESOLVED", "message": message,
	})
}

// Middleware applies the verifier. Mount it before anything reads
// X-Tenant-Id or X-Principal-Id.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.mode == Off || exempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(BasisHeader) // only this middleware states it

		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			r.Header.Set(BasisHeader, basisPeer)
			next.ServeHTTP(w, r)
			return
		}

		raw, hasToken := bearer(r.Header.Get("Authorization"))
		if !hasToken {
			if v.mode == Enforce {
				refuse(w, "identity_not_verified",
					"a signed identity envelope (Authorization: Bearer) or an mTLS peer certificate is required")
				return
			}
			v.log.Warn("identity (warn mode): no signed envelope or peer certificate; headers admitted as asserted",
				zap.String("path", r.URL.Path), zap.String("principal_id", r.Header.Get("X-Principal-Id")))
			r.Header.Set(BasisHeader, basisAsserted)
			next.ServeHTTP(w, r)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		claims, err := v.verify(ctx, raw)
		cancel()
		if err == nil {
			for header, claim := range map[string]string{
				"X-Tenant-Id":    claims.TenantID,
				"X-Principal-Id": claims.Principal.PrincipalID,
			} {
				switch got := strings.TrimSpace(r.Header.Get(header)); {
				case got == "":
					r.Header.Set(header, claim)
				case got != claim:
					err = errMismatch
				}
			}
		}
		if err != nil {
			if v.mode == Enforce {
				msg := "the signed identity envelope could not be verified"
				legacy := "invalid_identity_envelope"
				if errors.Is(err, errMismatch) {
					msg, legacy = errMismatch.Error(), "identity_mismatch"
				}
				v.log.Info("identity refused", zap.String("path", r.URL.Path), zap.Error(err))
				refuse(w, legacy, msg)
				return
			}
			v.log.Warn("identity (warn mode): envelope would be refused; admitted as asserted",
				zap.String("path", r.URL.Path), zap.Error(err))
			r.Header.Set(BasisHeader, basisAsserted)
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Set(BasisHeader, basisEnvelope)
		next.ServeHTTP(w, r)
	})
}
