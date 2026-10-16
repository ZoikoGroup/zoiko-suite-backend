package idenv

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// HeaderDPoP carries the DPoP proof (RFC 9449 §4.1).
const HeaderDPoP = "DPoP"

// ErrBinding is an envelope whose sender constraint was not satisfied: a
// DPoP-bound envelope without a valid proof for this request, or a
// certificate-bound one presented without that certificate (AU-27).
var ErrBinding = errors.New("identity envelope sender constraint not satisfied")

// DPoPProof is a verified proof.
type DPoPProof struct {
	JKT string
	JTI string
	IAT time.Time
}

// ReplayCache remembers DPoP proof ids for the proof window. MemoryReplay is
// per process; a service running several replicas behind one URL should back
// it with a shared store (identity-context uses Redis).
type ReplayCache interface {
	// Seen records jti until until and reports whether it was already there.
	Seen(jti string, until time.Time) bool
}

// MemoryReplay is an in-process ReplayCache.
type MemoryReplay struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewMemoryReplay builds one.
func NewMemoryReplay() *MemoryReplay { return &MemoryReplay{seen: map[string]time.Time{}} }

// Seen implements ReplayCache.
func (m *MemoryReplay) Seen(jti string, until time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if len(m.seen) > 8192 {
		for k, u := range m.seen {
			if now.After(u) {
				delete(m.seen, k)
			}
		}
	}
	if u, ok := m.seen[jti]; ok && now.Before(u) {
		return true
	}
	m.seen[jti] = until
	return false
}

// DPoPVerifier checks DPoP proofs.
type DPoPVerifier struct {
	// MaxAge bounds how old a proof's iat may be (default 2 min); proofs from
	// the future beyond Leeway are refused.
	MaxAge time.Duration
	Leeway time.Duration
	Replay ReplayCache
	Now    func() time.Time
}

// NewDPoPVerifier builds a verifier with defaults and an in-memory replay cache.
func NewDPoPVerifier() *DPoPVerifier {
	return &DPoPVerifier{MaxAge: 2 * time.Minute, Leeway: 30 * time.Second, Replay: NewMemoryReplay(), Now: time.Now}
}

type dpopClaims struct {
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	ATH   string `json:"ath,omitempty"`
	Nonce string `json:"nonce,omitempty"`
	jwt.RegisteredClaims
}

// Verify checks proof for a request to method htu. accessToken, when not
// empty, must match the proof's ath (RFC 9449 §4.3 step 11).
func (d *DPoPVerifier) Verify(proof, method, htu, accessToken string) (*DPoPProof, error) {
	if strings.TrimSpace(proof) == "" {
		return nil, errors.New("dpop: no proof")
	}
	now := d.now()
	var jwk JWK
	claims := &dpopClaims{}
	_, err := jwt.ParseWithClaims(proof, claims, func(t *jwt.Token) (any, error) {
		if typ, _ := t.Header["typ"].(string); typ != "dpop+jwt" {
			return nil, errors.New("dpop: typ must be dpop+jwt")
		}
		switch t.Method.Alg() {
		case jwt.SigningMethodES256.Alg(), jwt.SigningMethodRS256.Alg(), jwt.SigningMethodPS256.Alg():
		default:
			return nil, fmt.Errorf("dpop: unsupported alg %q", t.Method.Alg())
		}
		raw, err := json.Marshal(t.Header["jwk"])
		if err != nil || t.Header["jwk"] == nil {
			return nil, errors.New("dpop: no jwk header")
		}
		var hdr map[string]any
		_ = json.Unmarshal(raw, &hdr)
		for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
			if _, ok := hdr[private]; ok {
				return nil, errors.New("dpop: jwk carries private key material")
			}
		}
		if err := json.Unmarshal(raw, &jwk); err != nil {
			return nil, fmt.Errorf("dpop: jwk: %w", err)
		}
		return jwk.PublicKey()
	}, jwt.WithoutClaimsValidation())
	if err != nil {
		return nil, fmt.Errorf("dpop: %w", err)
	}
	if claims.ID == "" || claims.IssuedAt == nil {
		return nil, errors.New("dpop: jti and iat are required")
	}
	iat := claims.IssuedAt.Time
	if iat.After(now.Add(d.leeway())) || now.Sub(iat) > d.maxAge() {
		return nil, errors.New("dpop: proof iat outside the acceptance window")
	}
	if !strings.EqualFold(claims.HTM, method) {
		return nil, errors.New("dpop: htm does not match the request method")
	}
	if !SameHTU(claims.HTU, htu) {
		return nil, fmt.Errorf("dpop: htu %q does not match the request %q", claims.HTU, htu)
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		if claims.ATH != base64.RawURLEncoding.EncodeToString(sum[:]) {
			return nil, errors.New("dpop: ath does not match the presented token")
		}
	}
	jkt, err := Thumbprint(jwk)
	if err != nil {
		return nil, err
	}
	if d.Replay != nil && d.Replay.Seen(jkt+"|"+claims.ID, iat.Add(d.maxAge()+d.leeway())) {
		return nil, errors.New("dpop: proof replayed")
	}
	return &DPoPProof{JKT: jkt, JTI: claims.ID, IAT: iat}, nil
}

func (d *DPoPVerifier) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DPoPVerifier) maxAge() time.Duration {
	if d.MaxAge > 0 {
		return d.MaxAge
	}
	return 2 * time.Minute
}

func (d *DPoPVerifier) leeway() time.Duration {
	if d.Leeway > 0 {
		return d.Leeway
	}
	return 30 * time.Second
}

// Thumbprint is the RFC 7638 SHA-256 JWK thumbprint, base64url.
func Thumbprint(k JWK) (string, error) {
	var canon string
	switch k.Kty {
	case "EC":
		canon = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, k.Crv, k.X, k.Y)
	case "RSA":
		canon = fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, k.E, k.N)
	default:
		return "", fmt.Errorf("thumbprint: unsupported kty %q", k.Kty)
	}
	sum := sha256.Sum256([]byte(canon))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// ThumbprintOf is Thumbprint for a public key.
func ThumbprintOf(pub crypto.PublicKey) (string, error) {
	k, err := JWKFromPublicKey("", pub)
	if err != nil {
		return "", err
	}
	return Thumbprint(k)
}

// CertThumbprint is the RFC 8705 x5t#S256 of a certificate.
func CertThumbprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SameHTU compares two htu values without query and fragment (RFC 9449 §4.3
// step 9), case-insensitively on scheme and host, ignoring default ports.
func SameHTU(a, b string) bool {
	na, ea := normalizeHTU(a)
	nb, eb := normalizeHTU(b)
	return ea == nil && eb == nil && na == nb
}

func normalizeHTU(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("htu is not an absolute URL")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path, nil
}

// RequestHTU reconstructs the URL the client addressed: the forwarded
// scheme/host/prefix a proxy (Traefik ForwardAuth, or a path-stripping route)
// recorded, else the request's own.
func RequestHTU(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstHeader(r, "X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := firstHeader(r, "X-Forwarded-Host"); h != "" {
		host = h
	}
	path := r.URL.EscapedPath()
	if u := firstHeader(r, "X-Forwarded-Uri"); u != "" {
		// ForwardAuth: the original request URI, query included.
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		path = u
	} else if pre := firstHeader(r, "X-Forwarded-Prefix"); pre != "" {
		path = strings.TrimRight(pre, "/") + path
	}
	return scheme + "://" + host + path
}

// RequestMethod is the method the client used (X-Forwarded-Method behind
// ForwardAuth, else the request's own).
func RequestMethod(r *http.Request) string {
	if m := firstHeader(r, "X-Forwarded-Method"); m != "" {
		return m
	}
	return r.Method
}

func firstHeader(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
