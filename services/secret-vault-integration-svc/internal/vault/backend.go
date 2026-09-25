// Package vault defines the boundary between this service and wherever
// secret material actually lives.
//
// context.md §7.6: this service brokers access to secrets, it does not
// become a second copy of them. Backend is deliberately narrow — Get
// returns an opaque lease token, never the raw secret value, so that
// contract holds true regardless of which implementation is behind it.
//
// LocalFileVaultBackend is the v1 implementation: real AES-256-GCM
// encryption at rest against a local file, not a fake stub. It mirrors
// identity-context-svc's envelope_signing_key.pem — a real but
// local-only stand-in for what should eventually be KMS-backed.
// Production replaces this whole implementation with a real HashiCorp
// Vault or cloud KMS client behind the same Backend interface; nothing
// above this package needs to change when that happens.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrSecretMaterialNotFound is returned by Get/Rotate when no material
// has ever been Put for the given secretPath.
var ErrSecretMaterialNotFound = errors.New("secret material not found in vault backend")

// ErrLeaseTokenInvalid is returned by Verify when a token cannot be
// trusted: wrong prefix, undecodable payload, bad signature, or a
// secret_path that does not match the one it was minted for.
var ErrLeaseTokenInvalid = errors.New("vault: lease token is invalid")

// ErrLeaseTokenExpired is returned by Verify when the token was otherwise
// valid but its embedded expiry has passed. This is the "real" expiry the
// audit gap demanded: the token itself stops being valid, not just a row
// in a database.
var ErrLeaseTokenExpired = errors.New("vault: lease token has expired")

// Backend is the narrow interface every caller in this service depends
// on. Get returns an opaque lease token — never the raw secret value —
// so the "no service may store long-lived sensitive credentials" and
// "never return the raw secret value" constraints (context.md §1, §7.2)
// hold regardless of which implementation is behind this interface.
//
// Since the audit, the lease token is no longer an opaque random string:
// it is bound to the secret path and carries the lease's own expiry,
// HMAC-signed with the backend key. An expired token is rejected by
// Verify with zero database reads, and the service-side verify path
// additionally refuses tokens whose lease has been revoked.
type Backend interface {
	// Get verifies material exists for secretPath and mints a fresh lease
	// token bound to that path and to expiresAt. It does not return the
	// secret value itself.
	Get(ctx context.Context, secretPath string, expiresAt time.Time) (leaseToken string, err error)

	// Verify checks a lease token's signature, binding and embedded expiry.
	// It returns the claims an offline verifier needs: the secret path and
	// the expiry the token was minted for. It deliberately does not check
	// lease state — who may use the token and whether the lease still
	// exists is the caller's (service-side) decision against the lease
	// register.
	Verify(ctx context.Context, leaseToken string) (LeaseTokenInfo, error)

	// GetMaterial decrypts and returns the raw secret material for
	// secretPath. Used ONLY by the emergency retention pathway (§13 break
	// glass) — every ordinary path in this service returns a lease token,
	// never material. Named distinctly from Get so the broker path can
	// never accidentally receive plaintext.
	GetMaterial(ctx context.Context, secretPath string) ([]byte, error)

	// Put stores material for secretPath, encrypted at rest. Only ever
	// called by administrative/seeding paths, never by the broker flow.
	Put(ctx context.Context, secretPath string, material []byte) error

	// Rotate replaces the material at secretPath with freshly generated
	// material and re-encrypts it. Callers (the handler, via
	// context.md §7.2's rotate endpoint) are responsible for
	// invalidating any leases that referenced the old material —
	// Rotate itself only touches the backend, not lease state.
	Rotate(ctx context.Context, secretPath string) error
}

// LeaseTokenInfo is what Verify vouches for. ExpiresAt is the lease's own
// expiration carried inside the token, so a holder of an expired token is
// rejected before any lease state is consulted.
type LeaseTokenInfo struct {
	SecretPath string
	ExpiresAt  time.Time
}

// record is the on-disk shape for one secret's encrypted material.
type record struct {
	NonceB64      string `json:"nonce_b64"`
	CiphertextB64 string `json:"ciphertext_b64"`
}

// LocalFileVaultBackend implements Backend against a single local file,
// encrypted with AES-256-GCM. Not safe for multi-process use (no file
// locking) — acceptable for v1 since this is explicitly a local-dev
// stand-in, not a production secrets store (context.md §7.6/§7.7).
type LocalFileVaultBackend struct {
	mu       sync.Mutex
	filePath string
	gcm      cipher.AEAD
	// key is the raw 32-byte AES-256 key, kept for HMAC lease-token
	// signing. The master key now doubles as the token signing key: any
	// process that can mint a token can also decrypt the store, so no
	// extra keys to manage. A real vault backend would issue its own
	// time-bound tokens instead of deriving them here.
	key []byte
}

// NewLocalFileVaultBackend constructs a LocalFileVaultBackend.
// masterKeyHex must decode to exactly 32 bytes (AES-256) — this function
// fails fast rather than silently running with weak/wrong-length key
// material.
func NewLocalFileVaultBackend(filePath, masterKeyHex string) (*LocalFileVaultBackend, error) {
	if masterKeyHex == "" {
		return nil, errors.New("vault: VAULT_MASTER_KEY_HEX must be set (32-byte AES-256 key, hex-encoded)")
	}
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("vault: VAULT_MASTER_KEY_HEX is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("vault: VAULT_MASTER_KEY_HEX must decode to 32 bytes (AES-256), got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: failed to construct AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: failed to construct GCM mode: %w", err)
	}

	return &LocalFileVaultBackend{filePath: filePath, gcm: gcm, key: key}, nil
}

// NewLocalFileVaultBackendFromFile constructs a LocalFileVaultBackend with
// the AES-256 key read from a dedicated key file rather than an env var —
// the delegated-key pattern that satisfies SEC-INV-07's "master key
// isolated from application workloads" as far as this v1 local backend can,
// the same file-key posture identity-context-svc's
// JWT_SIGNING_PRIVATE_KEY_PATH already uses. The file must be readable by
// the owner only (0600); a group- or world-readable key file is refused,
// not tolerated, because a key with looser permissions is a key already
// exposed.
func NewLocalFileVaultBackendFromFile(storePath, keyPath string) (*LocalFileVaultBackend, error) {
	info, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("vault: cannot read key file: %w", err)
	}
	// 0600 owner-only enforcement is meaningful on POSIX file permissions.
	// On Windows the mode bits are not enforced by the OS and the file's
	// ACL governs access, so the check is refused there (Go reports 0666
	// for every new file regardless); key-file protection on a Windows
	// deployment is the deployment's ACL responsibility.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("vault: key file %s is not owner-only (mode %o) — refusing to run with an exposed key", keyPath, info.Mode().Perm())
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("vault: cannot read key file: %w", err)
	}
	masterKeyHex := strings.TrimSpace(string(raw))
	b, err := NewLocalFileVaultBackend(storePath, masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("vault: key file did not yield a usable AES-256 key: %w", err)
	}
	return b, nil
}

func (b *LocalFileVaultBackend) loadAll() (map[string]record, error) {
	data, err := os.ReadFile(b.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vault: failed to read local store: %w", err)
	}
	if len(data) == 0 {
		return map[string]record{}, nil
	}
	var records map[string]record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("vault: local store is corrupt: %w", err)
	}
	return records, nil
}

func (b *LocalFileVaultBackend) saveAll(records map[string]record) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: failed to marshal local store: %w", err)
	}
	// 0600: this file holds encrypted material — restrict to owner only,
	// same posture as identity-context-svc's envelope_signing_key.pem.
	if err := os.WriteFile(b.filePath, data, 0600); err != nil {
		return fmt.Errorf("vault: failed to write local store: %w", err)
	}
	return nil
}

func (b *LocalFileVaultBackend) encrypt(plaintext []byte) (record, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return record{}, fmt.Errorf("vault: failed to generate nonce: %w", err)
	}
	ciphertext := b.gcm.Seal(nil, nonce, plaintext, nil)
	return record{
		NonceB64:      base64.StdEncoding.EncodeToString(nonce),
		CiphertextB64: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

func (b *LocalFileVaultBackend) decrypt(rec record) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(rec.NonceB64)
	if err != nil {
		return nil, fmt.Errorf("vault: corrupt nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(rec.CiphertextB64)
	if err != nil {
		return nil, fmt.Errorf("vault: corrupt ciphertext: %w", err)
	}
	plaintext, err := b.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("vault: decryption failed (wrong key or corrupt data): %w", err)
	}
	return plaintext, nil
}

// Get verifies material exists and can actually be decrypted (a real
// integrity check, not a no-op), then mints a lease token bound to the
// secret path and to the lease's expiry. An expired lease's token is
// rejected by Verify with no database read, and the token carries its own
// path so it can never be presented against another secret.
func (b *LocalFileVaultBackend) Get(_ context.Context, secretPath string, expiresAt time.Time) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	records, err := b.loadAll()
	if err != nil {
		return "", err
	}
	rec, ok := records[secretPath]
	if !ok {
		return "", ErrSecretMaterialNotFound
	}
	if _, err := b.decrypt(rec); err != nil {
		return "", err
	}

	return b.mintLeaseToken(secretPath, expiresAt)
}

// leaseTokenPrefix is the versioned lead-in for the signed token format,
// so an old random opaque token (or a token from a future format) can
// never be mistaken for a valid current one.
const leaseTokenPrefix = "ltk:v2:"

// tokenPayload is the signed body of a lease token. Kept private — the
// whole point of the signature is that a holder cannot forge or alter the
// binding.
type tokenPayload struct {
	SecretPath string    `json:"secret_path"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (b *LocalFileVaultBackend) mintLeaseToken(secretPath string, expiresAt time.Time) (string, error) {
	payload, err := json.Marshal(tokenPayload{SecretPath: secretPath, ExpiresAt: expiresAt.UTC()})
	if err != nil {
		return "", fmt.Errorf("vault: failed to encode lease token payload: %w", err)
	}
	mac := hmac.New(sha256.New, b.key)
	if _, err := mac.Write(payload); err != nil {
		return "", fmt.Errorf("vault: failed to sign lease token: %w", err)
	}
	return leaseTokenPrefix + base64.RawURLEncoding.EncodeToString(payload) + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks a lease token's signature, format and embedded expiry.
// It returns the claims the service needs to pair the token with a live
// lease: the secret path it was minted for and the expiry it carries.
func (b *LocalFileVaultBackend) Verify(_ context.Context, leaseToken string) (LeaseTokenInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.verifyLeaseToken(leaseToken)
}

func (b *LocalFileVaultBackend) verifyLeaseToken(leaseToken string) (LeaseTokenInfo, error) {
	if !strings.HasPrefix(leaseToken, leaseTokenPrefix) {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	rest := strings.TrimPrefix(leaseToken, leaseTokenPrefix)
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	payloadB64, sigHex := rest[:dot], rest[dot+1:]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	mac := hmac.New(sha256.New, b.key)
	if _, err := mac.Write(payload); err != nil {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sigHex)) {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	var tp tokenPayload
	if err := json.Unmarshal(payload, &tp); err != nil || tp.SecretPath == "" || tp.ExpiresAt.IsZero() {
		return LeaseTokenInfo{}, ErrLeaseTokenInvalid
	}
	if !tp.ExpiresAt.After(time.Now().UTC()) {
		return LeaseTokenInfo{}, ErrLeaseTokenExpired
	}
	return LeaseTokenInfo{SecretPath: tp.SecretPath, ExpiresAt: tp.ExpiresAt}, nil
}

// GetMaterial decrypts and returns the raw secret material for secretPath.
// Used only by the §13 emergency retention pathway (break glass): ordinary
// use returns a lease token, never material, and this method split from
// Get is what keeps that separation mechanical rather than a discipline.
func (b *LocalFileVaultBackend) GetMaterial(_ context.Context, secretPath string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	records, err := b.loadAll()
	if err != nil {
		return nil, err
	}
	rec, ok := records[secretPath]
	if !ok {
		return nil, ErrSecretMaterialNotFound
	}
	return b.decrypt(rec)
}

// Put stores material for secretPath, encrypted at rest, overwriting
// any prior material for the same path.
func (b *LocalFileVaultBackend) Put(_ context.Context, secretPath string, material []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	records, err := b.loadAll()
	if err != nil {
		return err
	}
	rec, err := b.encrypt(material)
	if err != nil {
		return err
	}
	records[secretPath] = rec
	return b.saveAll(records)
}

// Rotate replaces the material at secretPath with freshly generated
// random material. There is no real upstream credential source to
// re-fetch a rotated value from in v1 (that's what a real Vault/KMS
// client would do) — generating new random bytes still exercises real
// encryption, real file I/O, and a real state change, which is what
// this v1 needs to prove rotation actually works end to end. Returns
// ErrSecretMaterialNotFound if nothing was ever Put for this path.
func (b *LocalFileVaultBackend) Rotate(_ context.Context, secretPath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	records, err := b.loadAll()
	if err != nil {
		return err
	}
	if _, ok := records[secretPath]; !ok {
		return ErrSecretMaterialNotFound
	}

	newMaterial := make([]byte, 32)
	if _, err := rand.Read(newMaterial); err != nil {
		return fmt.Errorf("vault: failed to generate rotated material: %w", err)
	}
	rec, err := b.encrypt(newMaterial)
	if err != nil {
		return err
	}
	records[secretPath] = rec
	return b.saveAll(records)
}

// ─── compile-time interface check ──────────────────────────────────────────

var _ Backend = (*LocalFileVaultBackend)(nil)
