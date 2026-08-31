// Package webhook implements real HMAC-SHA256 signature verification for
// BNK-07's provider callback endpoint — the direct fix for negative-path
// scenario #1 ("forged callback accepted"). This is exactly the mechanism
// real payment providers use (Stripe's Stripe-Signature header, Adyen's
// HMAC signature, etc.) — a shared secret, established out-of-band when a
// real provider integration is configured, and the raw request body,
// never the parsed/re-serialized JSON (re-serialization can silently
// change byte-for-byte content and invalidate a legitimate signature, or
// worse, let a malicious body slip through if verified against a
// re-encoded copy instead of what was actually received).
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Verify reports whether signatureHex is a valid hex-encoded HMAC-SHA256
// of rawBody using secret. Uses constant-time comparison to avoid a timing
// side-channel on the comparison itself.
func Verify(secret []byte, rawBody []byte, signatureHex string) bool {
	if len(secret) == 0 || signatureHex == "" {
		return false
	}
	sig, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(rawBody)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}

// Sign computes the same HMAC-SHA256 a legitimate caller (or this
// package's own tests, standing in for a real provider) would send — used
// only to construct a valid signature for verification, never by the
// verifying side itself in production.
func Sign(secret []byte, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(rawBody)
	return hex.EncodeToString(mac.Sum(nil))
}
