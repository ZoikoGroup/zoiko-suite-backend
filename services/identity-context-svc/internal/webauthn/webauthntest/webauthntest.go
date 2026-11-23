// Package webauthntest is a software authenticator and an in-memory
// credential store for tests of the passkey factor. The authenticator signs
// real ES256 assertions over a real P-256 key with CBOR "none" attestation,
// so a test that uses it runs the WebAuthn library's whole verification path.
package webauthntest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

var b64u = base64.RawURLEncoding

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

// Authenticator is one platform authenticator holding one credential.
type Authenticator struct {
	key *ecdsa.PrivateKey
	// CredID is the credential id; Counter the signature counter, advanced
	// by Get.
	CredID  []byte
	Counter uint32
	// Origin is what the browser reports in clientDataJSON; RPID is hashed
	// into authenticatorData.
	Origin string
	RPID   string
}

// New builds an authenticator for rpID answering from origin.
func New(t testing.TB, rpID, origin string) *Authenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &Authenticator{key: k, CredID: id, Origin: origin, RPID: rpID}
}

// CredentialID is the id as the service stores it.
func (a *Authenticator) CredentialID() string { return b64u.EncodeToString(a.CredID) }

func (a *Authenticator) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte(a.RPID))
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, a.Counter)
	out = append(out, cnt...)
	return append(out, attested...)
}

func (a *Authenticator) cosePublicKey(t testing.TB) []byte {
	t.Helper()
	x, y := make([]byte, 32), make([]byte, 32)
	a.key.PublicKey.X.FillBytes(x)
	a.key.PublicKey.Y.FillBytes(y)
	b, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: 2, Algorithm: -7},
		Curve:         1, XCoord: x, YCoord: y,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func clientData(t testing.TB, typ, challenge, origin string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Challenge extracts publicKey.challenge from begin options.
func Challenge(t testing.TB, opts json.RawMessage) string {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(opts, &o); err != nil || o.PublicKey.Challenge == "" {
		t.Fatalf("options carry no challenge: %s", opts)
	}
	return o.PublicKey.Challenge
}

// Create answers navigator.credentials.create for opts.
func (a *Authenticator) Create(t testing.TB, opts json.RawMessage) json.RawMessage {
	t.Helper()
	cdj := clientData(t, "webauthn.create", Challenge(t, opts), a.Origin)
	attested := make([]byte, 16) // AAGUID: zero
	idLen := make([]byte, 2)
	binary.BigEndian.PutUint16(idLen, uint16(len(a.CredID)))
	attested = append(attested, idLen...)
	attested = append(attested, a.CredID...)
	attested = append(attested, a.cosePublicKey(t)...)
	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(flagUP|flagUV|flagAT, attested),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": a.CredentialID(), "rawId": a.CredentialID(), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64u.EncodeToString(cdj), "attestationObject": b64u.EncodeToString(attObj),
			"transports": []string{"internal"},
		},
	})
	return b
}

// Get answers navigator.credentials.get for opts; the counter advances.
func (a *Authenticator) Get(t testing.TB, opts json.RawMessage, userHandle string) json.RawMessage {
	t.Helper()
	a.Counter++
	return a.Sign(t, Challenge(t, opts), userHandle, nil)
}

// Sign builds an assertion for challenge at the current counter; tamper, when
// set, may alter the signature before it is encoded.
func (a *Authenticator) Sign(t testing.TB, challenge, userHandle string, tamper func([]byte)) json.RawMessage {
	t.Helper()
	cdj := clientData(t, "webauthn.get", challenge, a.Origin)
	ad := a.authData(flagUP|flagUV, nil)
	h := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if tamper != nil {
		tamper(sig)
	}
	b, _ := json.Marshal(map[string]any{
		"id": a.CredentialID(), "rawId": a.CredentialID(), "type": "public-key",
		"response": map[string]any{
			"authenticatorData": b64u.EncodeToString(ad), "clientDataJSON": b64u.EncodeToString(cdj),
			"signature": b64u.EncodeToString(sig), "userHandle": b64u.EncodeToString([]byte(userHandle)),
		},
	})
	return b
}

// Decision is one decision row the store was asked to append.
type Decision struct{ Principal, Action, Outcome, Basis string }

// MemoryStore satisfies webauthn.Store in memory.
type MemoryStore struct {
	mu        sync.Mutex
	Creds     map[string]domain.WebAuthnCredential
	Decisions []Decision
	Events    []outbox.Record
	// FailList makes credential lookups fail, as a database outage would.
	FailList bool
}

// NewMemoryStore builds one.
func NewMemoryStore() *MemoryStore { return &MemoryStore{Creds: map[string]domain.WebAuthnCredential{}} }

// ErrDuplicate is what InsertWebAuthnCredential answers for a known id.
var ErrDuplicate = errors.New("duplicate credential")

func (f *MemoryStore) InsertWebAuthnCredential(_ context.Context, c domain.WebAuthnCredential, _ string, recs ...outbox.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Creds[c.CredentialID]; ok {
		return ErrDuplicate
	}
	f.Creds[c.CredentialID] = c
	f.Decisions = append(f.Decisions, Decision{c.PrincipalID, "MFA_ACTIVATE", "ALLOWED", "passkey_registered"})
	f.Events = append(f.Events, recs...)
	return nil
}

func (f *MemoryStore) ListWebAuthnCredentials(_ context.Context, tenantID, principalID string) ([]domain.WebAuthnCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailList {
		return nil, errors.New("db down")
	}
	var out []domain.WebAuthnCredential
	for _, c := range f.Creds {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Status == "ACTIVE" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *MemoryStore) UseWebAuthnCredential(_ context.Context, tenantID, principalID, credentialID string, signCount uint32, backupState bool,
	action, _ string, recs ...outbox.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Creds[credentialID]
	if !ok || c.TenantID != tenantID || c.PrincipalID != principalID || c.Status != "ACTIVE" {
		return domain.ErrCredentialNotFound
	}
	c.SignCount, c.BackupState = signCount, backupState
	f.Creds[credentialID] = c
	f.Decisions = append(f.Decisions, Decision{principalID, action, "ALLOWED", "passkey_assertion"})
	f.Events = append(f.Events, recs...)
	return nil
}

func (f *MemoryStore) RetireWebAuthnCredential(_ context.Context, tenantID, principalID, credentialID, _, action, _ string,
	recs ...outbox.Record) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, c := range f.Creds {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Status == "ACTIVE" && (credentialID == "" || id == credentialID) {
			c.Status = "RETIRED"
			f.Creds[id] = c
			n++
		}
	}
	if n > 0 {
		f.Decisions = append(f.Decisions, Decision{principalID, action, "ALLOWED", "passkey_retired"})
		f.Events = append(f.Events, recs...)
	}
	return n, nil
}

func (f *MemoryStore) HasActiveWebAuthn(ctx context.Context, tenantID, principalID string) (bool, error) {
	out, err := f.ListWebAuthnCredentials(ctx, tenantID, principalID)
	return len(out) > 0, err
}

func (f *MemoryStore) RecordMFADecision(_ context.Context, principalID, _, action, outcome, basis, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Decisions = append(f.Decisions, Decision{principalID, action, outcome, basis})
	return nil
}

// LastDecision is the most recent decision row.
func (f *MemoryStore) LastDecision() Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Decisions) == 0 {
		return Decision{}
	}
	return f.Decisions[len(f.Decisions)-1]
}
