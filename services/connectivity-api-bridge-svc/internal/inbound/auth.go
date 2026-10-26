package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// Authentication outcomes. Every authentication failure is one uniform refusal to the caller; the distinctions
// below exist for evidence and telemetry, never for the response.
var (
	// ErrAuthFailed: the credential, signature or key did not verify, or none could be tried.
	ErrAuthFailed = errors.New("authentication failed")
	// ErrReplayWindow: the signature verified but the signed timestamp is outside the replay window. Raised only
	// after a valid signature, so it is not an oracle for an attacker without the secret.
	ErrReplayWindow = errors.New("replay window check failed")
	// ErrAuthUnsupported: the contract names a mechanism this listener cannot verify; fail closed.
	ErrAuthUnsupported = errors.New("authentication mechanism is not supported at this listener")
	// ErrSecretUnavailable: the vault could not answer. The sender is told to retry; nothing was processed.
	ErrSecretUnavailable = errors.New("credential material is unavailable")
)

// Secrets releases the material of a bound credential reference for one verification. The caller zeroes it after.
// A missing, refused or foreign reference is ErrAuthFailed; an outage is ErrSecretUnavailable.
type Secrets interface {
	Secret(ctx context.Context, tenantID, credentialRef string) ([]byte, error)
}

// KeyUse is one credential tried against a request, labelled so a caller never tries every secret it can find.
type KeyUse struct {
	Ref   string
	Grace bool
}

// AuthResult is what a successful authentication tells the pipeline.
type AuthResult struct {
	Mechanism string
	// KeyLabel is "current" or "previous" (a credential still inside its rotation grace window).
	KeyLabel string
}

func hasher(algorithm string) func() hash.Hash {
	switch algorithm {
	case "HMAC-SHA384":
		return sha512.New384
	case "HMAC-SHA512":
		return sha512.New
	default:
		return sha256.New
	}
}

func decodeSig(enc, v string) ([]byte, bool) {
	v = strings.TrimSpace(v)
	if p := strings.IndexByte(v, '='); p > 0 && p < 12 { // "sha256=<sig>" style prefixes
		v = v[p+1:]
	}
	switch enc {
	case "base64":
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(v)
		}
		return b, err == nil
	default:
		b, err := hex.DecodeString(v)
		return b, err == nil
	}
}

func parseTimestamp(format, v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	switch format {
	case "RFC3339":
		t, err := time.Parse(time.RFC3339, v)
		return t, err == nil
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}, false
		}
		if n > 1e12 { // milliseconds
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
}

// VerifyHMAC verifies an inbound request signed with a shared secret. It checks the signature against each key
// offered (the current one, then a previous one inside its grace window), in constant time, and only once a
// signature has verified does it judge the timestamp against the replay window (NP-02).
func VerifyHMAC(info registry.AuthInfo, h http.Header, body []byte, keys [][]byte, labels []string, now time.Time) (AuthResult, error) {
	sigRaw := h.Get(info.SignatureHeader)
	if sigRaw == "" {
		return AuthResult{}, ErrAuthFailed
	}
	got, ok := decodeSig(info.SignatureEncoding, sigRaw)
	if !ok || len(got) == 0 {
		return AuthResult{}, ErrAuthFailed
	}
	tsRaw := h.Get(info.TimestampHeader)
	var msg []byte
	if info.SignedContent == "TIMESTAMP_DOT_BODY" {
		if tsRaw == "" {
			return AuthResult{}, ErrAuthFailed
		}
		msg = append(append([]byte(strings.TrimSpace(tsRaw)), '.'), body...)
	} else {
		msg = body
	}
	matched := -1
	for i, k := range keys {
		if len(k) == 0 {
			continue
		}
		m := hmac.New(hasher(info.SignatureAlgorithm), k)
		m.Write(msg)
		if subtle.ConstantTimeCompare(m.Sum(nil), got) == 1 && matched < 0 {
			matched = i // keep scanning: the time taken must not depend on which key matched
		}
	}
	if matched < 0 {
		return AuthResult{}, ErrAuthFailed
	}
	res := AuthResult{Mechanism: "HMAC", KeyLabel: labels[matched]}
	ts, ok := parseTimestamp(info.TimestampFormat, tsRaw)
	window := time.Duration(info.ReplayWindowSeconds) * time.Second
	if !ok || now.Sub(ts) > window || ts.Sub(now) > window {
		return res, ErrReplayWindow
	}
	return res, nil
}

// VerifyAPIKey verifies a shared API key carried in a header, in constant time, against each key offered.
func VerifyAPIKey(info registry.AuthInfo, h http.Header, keys [][]byte, labels []string) (AuthResult, error) {
	v := strings.TrimSpace(h.Get(info.APIKeyHeader))
	if v == "" {
		return AuthResult{}, ErrAuthFailed
	}
	matched := -1
	for i, k := range keys {
		if len(k) > 0 && subtle.ConstantTimeCompare([]byte(v), []byte(strings.TrimSpace(string(k)))) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return AuthResult{}, ErrAuthFailed
	}
	return AuthResult{Mechanism: "API_KEY_HEADER", KeyLabel: labels[matched]}, nil
}

// Edge headers: a TLS-terminating edge that has verified a client certificate forwards its fingerprint, attested
// with a secret shared between the edge and this service. The gateway never trusts a bare certificate header.
const (
	HeaderCertFingerprint = "X-Client-Cert-Fingerprint"
	HeaderEdgeTimestamp   = "X-Edge-Timestamp"
	HeaderEdgeAttestation = "X-Edge-Attestation"
	edgeTolerance         = 60 * time.Second
)

// EdgeAttestation computes the attestation an edge attaches: HMAC-SHA256(secret, fingerprint "." timestamp).
func EdgeAttestation(secret []byte, fingerprint, timestamp string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strings.ToLower(fingerprint) + "." + timestamp))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyMTLS verifies a client certificate presented to the edge: the edge's attestation is checked first, then the
// attested fingerprint must equal one of the fingerprints bound to THIS connection (NP-04: a certificate valid for
// another connection or tenant is just another unknown certificate here).
func VerifyMTLS(h http.Header, edgeSecret []byte, bound [][]byte, labels []string, now time.Time) (AuthResult, error) {
	fp := strings.ToLower(strings.TrimSpace(h.Get(HeaderCertFingerprint)))
	ts := strings.TrimSpace(h.Get(HeaderEdgeTimestamp))
	att := strings.TrimSpace(h.Get(HeaderEdgeAttestation))
	if len(edgeSecret) == 0 || fp == "" || ts == "" || att == "" {
		return AuthResult{}, ErrAuthFailed
	}
	want := EdgeAttestation(edgeSecret, fp, ts)
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(att))) != 1 {
		return AuthResult{}, ErrAuthFailed
	}
	t, ok := parseTimestamp("UNIX_SECONDS", ts)
	if !ok || now.Sub(t) > edgeTolerance || t.Sub(now) > edgeTolerance {
		return AuthResult{}, ErrAuthFailed
	}
	matched := -1
	for i, b := range bound {
		for _, line := range strings.Fields(string(b)) {
			if subtle.ConstantTimeCompare([]byte(strings.ToLower(line)), []byte(fp)) == 1 && matched < 0 {
				matched = i
			}
		}
	}
	if matched < 0 {
		return AuthResult{}, ErrAuthFailed
	}
	return AuthResult{Mechanism: "MTLS", KeyLabel: labels[matched]}, nil
}

func zero(bs [][]byte) {
	for _, b := range bs {
		for i := range b {
			b[i] = 0
		}
	}
}
