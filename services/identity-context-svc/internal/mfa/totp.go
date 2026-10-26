// Package mfa implements the TOTP second factor (RFC 6238) and the encryption
// of its shared secrets at rest.
//
// It is the first second factor in the estate. ZS-IAM-001 §3 forbids
// password-only privileged sessions and §13/§14 require high-assurance step-up
// before a JIT or break-glass session is issued; until this package existed
// mfa_verified could only ever be false, so that requirement was unsatisfiable
// and authorization-svc's S-3 stayed ❌.
//
// TOTP is not phishing-resistant. The standard accepts "tenant/security-approved
// high-assurance authentication", so it is the interim factor pending Security's
// approval and a WebAuthn factor; see the service's RUNBOOK.
//
// Parameters are fixed at the RFC 6238 defaults (HMAC-SHA1, 6 digits, 30 s)
// because those are what every authenticator app implements. SHA-1's collision
// weakness does not apply to HMAC.
package mfa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is RFC 6238's default and what authenticator apps implement
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Algorithm, Digits and PeriodSeconds are the RFC 6238 defaults.
	Algorithm     = "SHA1"
	Digits        = 6
	PeriodSeconds = 30

	// SecretBytes is 160 bits, RFC 4226 §4's recommended shared-secret length.
	SecretBytes = 20
)

// ErrMalformedCode is a code that is not exactly Digits decimal digits. It is
// rejected before any comparison so a malformed value never reaches the
// lockout counter as if it were a guess.
var ErrMalformedCode = errors.New("code must be exactly 6 digits")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a fresh random shared secret.
func NewSecret() ([]byte, error) {
	s := make([]byte, SecretBytes)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}
	return s, nil
}

// EncodeSecret renders a secret the way authenticator apps expect it typed or
// scanned: unpadded base32.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// Step is the RFC 6238 time step containing t.
func Step(t time.Time) int64 { return t.Unix() / PeriodSeconds }

// Code is the RFC 4226 HOTP value of secret at counter step.
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	// Dynamic truncation, RFC 4226 §5.3.
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	return fmt.Sprintf("%0*d", Digits, bin%1_000_000)
}

// NormalizeCode strips the spaces and dashes people type ("123 456") and
// checks the result is Digits decimal digits.
func NormalizeCode(code string) (string, error) {
	c := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code))
	if len(c) != Digits {
		return "", ErrMalformedCode
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return "", ErrMalformedCode
		}
	}
	return c, nil
}

// Match reports the step at which code is valid for secret, looking skew steps
// either side of now to absorb clock drift (RFC 6238 §5.2 recommends at most
// one).
//
// It returns the step rather than a bool because the caller must refuse a step
// it has already accepted: a TOTP code is valid for its whole window, and
// without that check an observed code could be replayed for up to 90 seconds.
// Every candidate is compared, in constant time, so the response time does not
// reveal which window matched.
func Match(secret []byte, code string, now time.Time, skew int) (int64, bool) {
	if skew < 0 {
		skew = 0
	}
	cur := Step(now)
	var matched int64
	found := 0
	for d := -skew; d <= skew; d++ {
		step := cur + int64(d)
		if subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1 {
			// The latest matching step wins, so a later replay check is made
			// against the most recent window the code belongs to.
			matched = step
			found = 1
		}
	}
	return matched, found == 1
}

// OTPAuthURI is the Key Uri Format authenticator apps scan from a QR code.
func OTPAuthURI(issuer, account, encodedSecret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", encodedSecret)
	q.Set("issuer", issuer)
	q.Set("algorithm", Algorithm)
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(PeriodSeconds))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
