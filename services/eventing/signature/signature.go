// Package signature is the shared message-authenticity and replay framework of
// ZS-SVC-M-001 (INT-CTRL-010, INT-CTRL-023). Outbound webhooks sign with it
// (notification-svc) and inbound provider callbacks verify with it (the adapters),
// so one profile, one replay rule and one set of failure codes cover both.
//
// Profile "v1": HMAC-SHA256 over "<unix-timestamp>.<event-id>.<body>", carried as
//
//	Zoiko-Signature: t=<unix-timestamp>,v1=<hex>[,v1=<hex>]
//
// The timestamp and the event identity are inside the signed bytes, so a captured
// message cannot be re-labelled as a different event or re-dated. More than one v1
// value is allowed so a key can be rotated without a flag day: the sender signs with
// the current and the previous key and the recipient accepts either. The profile name
// is part of the header, so a later profile (for example RFC 9421) can be added
// without changing this one.
//
// The package never holds a key it was not handed and never logs one. A sender whose
// key lives in a vault supplies a Signer that asks the vault; a verifier supplies the
// secrets it holds.
package signature

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"zoiko.io/eventing/integration"
)

// HeaderName is the HTTP header that carries the signature.
const HeaderName = "Zoiko-Signature"

// Profile is the signature profile implemented here.
const Profile = "v1"

// Verification failures. Each maps to a stable INT-* code through Code.
var (
	// ErrMalformed: the header is absent or not a v1 signature header.
	ErrMalformed = errors.New("signature header is missing or malformed")
	// ErrInvalid: no supplied key reproduces any signature in the header.
	ErrInvalid = errors.New("signature does not match")
	// ErrStale: the timestamp is outside the accepted window (too old or too far ahead).
	ErrStale = errors.New("signature timestamp is outside the accepted window")
	// ErrReplay: this exact message was already accepted inside the window.
	ErrReplay = errors.New("message was already accepted (replay)")
)

// Code maps a verification error to its INT-* code.
func Code(err error) integration.Code {
	switch {
	case errors.Is(err, ErrStale), errors.Is(err, ErrReplay):
		return integration.CodeCallbackConflict
	case errors.Is(err, ErrMalformed), errors.Is(err, ErrInvalid):
		return integration.CodeSignatureInvalid
	}
	return integration.CodeSignatureInvalid
}

// SignedBytes is the exact byte string that is signed.
func SignedBytes(ts int64, eventID string, body []byte) []byte {
	prefix := strconv.FormatInt(ts, 10) + "." + eventID + "."
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	return append(out, body...)
}

// Signer computes the hex HMAC-SHA256 of msg with one key. A vault-backed sender
// implements it by calling the vault; the key never needs to be in this process.
type Signer func(ctx context.Context, msg []byte) (hexSig string, err error)

// LocalSigner signs with a key held in memory (tests and verifiers; a sender that
// stores keys in a vault must not use it).
func LocalSigner(key []byte) Signer {
	k := append([]byte(nil), key...)
	return func(_ context.Context, msg []byte) (string, error) {
		return hexHMAC(k, msg), nil
	}
}

func hexHMAC(key, msg []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign builds the header value for one message using every supplied signer (the
// current key first, then any key still inside its rotation grace period).
func Sign(ctx context.Context, ts time.Time, eventID string, body []byte, signers ...Signer) (string, error) {
	if eventID == "" || strings.ContainsAny(eventID, ".,= \r\n") {
		return "", errors.New("event id is empty or contains a reserved character")
	}
	if len(signers) == 0 {
		return "", errors.New("at least one signer is required")
	}
	unix := ts.UTC().Unix()
	msg := SignedBytes(unix, eventID, body)
	var b strings.Builder
	b.WriteString("t=" + strconv.FormatInt(unix, 10))
	for _, s := range signers {
		sig, err := s(ctx, msg)
		if err != nil {
			return "", err
		}
		b.WriteString("," + Profile + "=" + sig)
	}
	return b.String(), nil
}

// Parsed is a decoded header.
type Parsed struct {
	Timestamp  int64
	Signatures []string // lower-case hex, one per v1 entry
}

// maxSignatures bounds how many v1 entries a header may carry, so a hostile header
// cannot make verification do unbounded work.
const maxSignatures = 4

// Parse decodes a header value. Unknown parameters are ignored so a later profile
// can be carried alongside v1.
func Parse(header string) (Parsed, error) {
	var p Parsed
	haveT := false
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return Parsed{}, ErrMalformed
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 || haveT {
				return Parsed{}, ErrMalformed
			}
			p.Timestamp, haveT = n, true
		case Profile:
			if len(v) != sha256.Size*2 {
				return Parsed{}, ErrMalformed
			}
			if _, err := hex.DecodeString(v); err != nil {
				return Parsed{}, ErrMalformed
			}
			if len(p.Signatures) >= maxSignatures {
				return Parsed{}, ErrMalformed
			}
			p.Signatures = append(p.Signatures, strings.ToLower(v))
		}
	}
	if !haveT || len(p.Signatures) == 0 {
		return Parsed{}, ErrMalformed
	}
	return p, nil
}

// Verify checks a received message against the keys the verifier holds. It checks
// the signature first (constant time, against every supplied key and every header
// value) and the timestamp window second, so a forged message is reported as invalid
// rather than as stale. tolerance bounds the accepted age in both directions.
func Verify(header, eventID string, body []byte, keys [][]byte, now time.Time, tolerance time.Duration) error {
	p, err := Parse(header)
	if err != nil {
		return err
	}
	msg := SignedBytes(p.Timestamp, eventID, body)
	ok := false
	for _, key := range keys {
		want := hexHMAC(key, msg)
		for _, got := range p.Signatures {
			if hmac.Equal([]byte(want), []byte(got)) { // no early exit: both loops run to completion
				ok = true
			}
		}
	}
	if !ok {
		return ErrInvalid
	}
	age := now.UTC().Sub(time.Unix(p.Timestamp, 0).UTC())
	if age > tolerance || age < -tolerance {
		return fmt.Errorf("%w", ErrStale)
	}
	return nil
}

// ReplayGuard remembers accepted messages for the replay window. A shared (database)
// implementation makes the guard hold across replicas; MemoryGuard is per process.
type ReplayGuard interface {
	// FirstSeen records key until expires and reports whether this is the first time
	// it was recorded inside that period.
	FirstSeen(ctx context.Context, key string, expires time.Time) (bool, error)
}

// ReplayKey names a message for replay purposes: the scope (tenant plus source), the
// event identity and the timestamp. A different body under the same identity and
// timestamp carries a different signature, so signature + identity together is the
// replay identity.
func ReplayKey(scope, eventID string, p Parsed) string {
	return scope + "|" + eventID + "|" + strconv.FormatInt(p.Timestamp, 10) + "|" + p.Signatures[0]
}

// VerifyOnce is Verify plus the replay rule: a message that verifies is accepted at
// most once inside the window.
func VerifyOnce(ctx context.Context, g ReplayGuard, scope, header, eventID string, body []byte,
	keys [][]byte, now time.Time, tolerance time.Duration) error {
	if err := Verify(header, eventID, body, keys, now, tolerance); err != nil {
		return err
	}
	p, _ := Parse(header) // already validated
	first, err := g.FirstSeen(ctx, ReplayKey(scope, eventID, p), now.Add(2*tolerance))
	if err != nil {
		return err
	}
	if !first {
		return ErrReplay
	}
	return nil
}

// MemoryGuard is a bounded in-process ReplayGuard. When it is full it drops expired
// entries first and then the oldest; the bound only ever makes it forget old
// messages, which the timestamp window rejects anyway.
type MemoryGuard struct {
	mu      sync.Mutex
	max     int
	entries map[string]time.Time
	order   []string
	clock   func() time.Time
}

// NewMemoryGuard builds a guard remembering at most max messages (minimum 1).
func NewMemoryGuard(max int, clock func() time.Time) *MemoryGuard {
	if max < 1 {
		max = 1
	}
	if clock == nil {
		clock = time.Now
	}
	return &MemoryGuard{max: max, entries: make(map[string]time.Time, max), clock: clock}
}

// FirstSeen implements ReplayGuard.
func (g *MemoryGuard) FirstSeen(_ context.Context, key string, expires time.Time) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if exp, ok := g.entries[key]; ok && now.Before(exp) {
		return false, nil
	}
	if _, ok := g.entries[key]; !ok {
		for len(g.entries) >= g.max {
			g.evictOne(now)
		}
		g.order = append(g.order, key)
	}
	g.entries[key] = expires
	return true, nil
}

func (g *MemoryGuard) evictOne(now time.Time) {
	for i, k := range g.order {
		if exp, ok := g.entries[k]; !ok || !now.Before(exp) {
			delete(g.entries, k)
			g.order = append(g.order[:i], g.order[i+1:]...)
			return
		}
	}
	if len(g.order) > 0 {
		delete(g.entries, g.order[0])
		g.order = g.order[1:]
		return
	}
	g.entries = map[string]time.Time{}
}
