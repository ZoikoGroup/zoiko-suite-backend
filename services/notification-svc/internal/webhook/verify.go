package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Callback authentication (ZS-SVC-Y-001 INV-27, NP-26, TC-12).
//
// A provider callback changes delivery state, creates suppressions and can
// disable a recipient's address. The route is exempt from the request envelope
// (a provider carries no ZoikoSuite headers), so until this check existed the
// only thing standing between the internet and those state changes was the
// URL. Nothing authenticated the caller.
//
// The scheme is deliberately generic: the callback is signed with a shared
// secret per provider, the way a certified ingress (XIC, Y-001 section 10.1
// "authenticated provider callback ingress through XIC") or a provider's own
// gateway would. Native provider schemes (an SNS certificate, an ECDSA public
// key) are not implemented here because no provider has been selected yet
// (open decision OD-02); each would be a separate verifier behind this same
// gate.
//
//	X-Zoiko-Signature: t=<unix seconds>,v1=<hex hmac-sha256>[,v1=<hex>...]
//	signed string:     "<t>.<raw request body>"
//
// The timestamp is inside the signed string, so a captured callback cannot be
// replayed outside the tolerance window and a signature cannot be moved onto a
// different timestamp. A provider with no configured secret is refused, not
// waved through: this check fails closed.

// SignatureHeader carries the callback signature.
const SignatureHeader = "X-Zoiko-Signature"

// DefaultTolerance is how far a callback timestamp may differ from the clock.
const DefaultTolerance = 5 * time.Minute

// MinSecretLength is the shortest accepted signing secret, in bytes.
const MinSecretLength = 16

// Verification failures. Callers map every one of them to the same 401 so the
// response does not tell an attacker which part was wrong; the reason is logged.
var (
	ErrNoSecret         = errors.New("no webhook secret configured for this provider")
	ErrMissingSignature = errors.New("missing webhook signature")
	ErrMalformedHeader  = errors.New("malformed webhook signature header")
	ErrStaleTimestamp   = errors.New("webhook timestamp outside the tolerance window")
	ErrBadSignature     = errors.New("webhook signature does not match")
)

// Verifier checks callback signatures. A nil Verifier verifies nothing and is
// refused by the handler; construct one with NewVerifier.
type Verifier struct {
	secrets   map[string][][]byte
	tolerance time.Duration
	now       func() time.Time
}

// NewVerifier builds a verifier from per-provider secrets. Several secrets per
// provider are accepted so a secret can be rotated without dropping callbacks.
// Provider names are matched case-insensitively. A non-positive tolerance uses
// DefaultTolerance.
func NewVerifier(secrets map[string][]string, tolerance time.Duration) *Verifier {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	v := &Verifier{secrets: make(map[string][][]byte, len(secrets)), tolerance: tolerance, now: time.Now}
	for provider, list := range secrets {
		key := normalizeProvider(provider)
		for _, s := range list {
			if len(s) >= MinSecretLength {
				v.secrets[key] = append(v.secrets[key], []byte(s))
			}
		}
	}
	return v
}

// Providers reports the providers that have at least one usable secret.
func (v *Verifier) Providers() []string {
	if v == nil {
		return nil
	}
	out := make([]string, 0, len(v.secrets))
	for p := range v.secrets {
		out = append(out, p)
	}
	return out
}

func normalizeProvider(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// Sign produces a signature header value for body at time ts. It is what a
// gateway signs with, and what the tests sign with.
func Sign(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac(secret, t, body))
}

func mac(secret []byte, t string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(t))
	h.Write([]byte{'.'})
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks header against body for provider. It returns nil only when the
// timestamp is inside the tolerance window and at least one v1 value matches the
// HMAC of "<t>.<body>" under at least one of the provider's secrets. Comparison
// is constant time.
func (v *Verifier) Verify(provider, header string, body []byte) error {
	secrets := v.secrets[normalizeProvider(provider)]
	if len(secrets) == 0 {
		return ErrNoSecret
	}
	if strings.TrimSpace(header) == "" {
		return ErrMissingSignature
	}
	var t string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return ErrMalformedHeader
		}
		switch kv[0] {
		case "t":
			if t != "" {
				return ErrMalformedHeader
			}
			t = kv[1]
		case "v1":
			b, err := hex.DecodeString(kv[1])
			if err != nil || len(b) != sha256.Size {
				return ErrMalformedHeader
			}
			sigs = append(sigs, b)
		}
	}
	if t == "" || len(sigs) == 0 {
		return ErrMalformedHeader
	}
	sec, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return ErrMalformedHeader
	}
	skew := v.now().Sub(time.Unix(sec, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > v.tolerance {
		return ErrStaleTimestamp
	}
	for _, secret := range secrets {
		want := mac(secret, t, body)
		for _, got := range sigs {
			if hmac.Equal(want, got) {
				return nil
			}
		}
	}
	return ErrBadSignature
}
