package ar05

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Callback authentication. A provider has no principal and no tenant header; the ONLY thing that authenticates it is
// an HMAC-SHA256 signature made with a per-provider secret. The signed message binds the timestamp and nonce to the raw
// body so none of the three can be swapped:
//
//	signature = hex( HMAC-SHA256( secret, timestamp + "." + nonce + "." + rawBody ) )
//
// Headers (all required):
//
//	X-Zoiko-Provider    provider id (selects the secret; also in the attempt record)
//	X-Zoiko-Timestamp   unix seconds
//	X-Zoiko-Nonce       unique per delivery (single use per provider)
//	X-Zoiko-Signature   "sha256=" + hex signature
const (
	HeaderProvider  = "X-Zoiko-Provider"
	HeaderTimestamp = "X-Zoiko-Timestamp"
	HeaderNonce     = "X-Zoiko-Nonce"
	HeaderSignature = "X-Zoiko-Signature"
	SigAlg          = "HMAC-SHA256"
	sigPrefix       = "sha256="

	// DefaultCallbackWindow is the accepted clock skew / age of a signed callback.
	DefaultCallbackWindow = 5 * time.Minute
)

// Signature verification failures. The HTTP layer answers all of them 401 CALLBACK_UNAUTHENTICATED except
// ErrCallbackStale (CALLBACK_STALE); it never says which check failed first.
var (
	ErrNoSecret          = errors.New("no callback secret is configured for this provider")
	ErrSignatureMissing  = errors.New("signature, timestamp or nonce header is missing")
	ErrSignatureMalform  = errors.New("signature is malformed")
	ErrSignatureMismatch = errors.New("signature does not match")
	ErrCallbackStale     = errors.New("callback timestamp is outside the accepted window")
)

var providerRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// SecretSource returns the callback signing secret of a provider (never stored in the database or the pack).
type SecretSource interface {
	Secret(provider string) ([]byte, bool)
}

// EnvSecrets reads AR_EINVOICE_CALLBACK_SECRET_<PROVIDER>, where PROVIDER is the provider id upper-cased with every
// character outside [A-Z0-9] replaced by "_". An empty value is "no secret".
type EnvSecrets struct{}

// EnvSecretName is the environment variable name for a provider.
func EnvSecretName(provider string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(provider) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "AR_EINVOICE_CALLBACK_SECRET_" + b.String()
}

func (EnvSecrets) Secret(provider string) ([]byte, bool) {
	if !providerRe.MatchString(provider) {
		return nil, false
	}
	v := os.Getenv(EnvSecretName(provider))
	if v == "" {
		return nil, false
	}
	return []byte(v), true
}

// MapSecrets is a fixed secret table (tests, and deployments that inject from a vault client).
type MapSecrets map[string][]byte

func (m MapSecrets) Secret(provider string) ([]byte, bool) {
	s, ok := m[provider]
	return s, ok && len(s) > 0
}

// Sign computes the signature header value for a delivery.
func Sign(secret []byte, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	mac.Write([]byte("."))
	mac.Write(body)
	return sigPrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifyCallback checks the signature (constant time) and then the timestamp window. The signature is checked first,
// so an unauthenticated caller learns nothing about the window. The returned time is the signed timestamp.
func VerifyCallback(secret []byte, signature, timestamp, nonce string, body []byte, now time.Time, window time.Duration) (time.Time, error) {
	if len(secret) == 0 {
		return time.Time{}, ErrNoSecret
	}
	if signature == "" || timestamp == "" || nonce == "" || len(nonce) > 128 {
		return time.Time{}, ErrSignatureMissing
	}
	if !strings.HasPrefix(signature, sigPrefix) {
		return time.Time{}, ErrSignatureMalform
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, sigPrefix))
	if err != nil || len(got) != sha256.Size {
		return time.Time{}, ErrSignatureMalform
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return time.Time{}, ErrSignatureMismatch
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return time.Time{}, ErrSignatureMalform
	}
	ts := time.Unix(secs, 0).UTC()
	if d := now.Sub(ts); d > window || d < -window {
		return ts, ErrCallbackStale
	}
	return ts, nil
}
