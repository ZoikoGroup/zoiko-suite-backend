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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrSecretMaterialNotFound is returned by Get/Rotate when no material
// has ever been Put for the given secretPath.
var ErrSecretMaterialNotFound = errors.New("secret material not found in vault backend")

// Backend is the narrow interface every caller in this service depends
// on. Get returns an opaque lease token — never the raw secret value —
// so the "no service may store long-lived sensitive credentials" and
// "never return the raw secret value" constraints (context.md §1, §7.2)
// hold regardless of which implementation is behind this interface.
type Backend interface {
	// Get verifies material exists for secretPath and mints a fresh
	// opaque lease token. It does not return the secret value itself.
	Get(ctx context.Context, secretPath string) (leaseToken string, err error)

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

	return &LocalFileVaultBackend{filePath: filePath, gcm: gcm}, nil
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
// integrity check, not a no-op), then mints a fresh random opaque lease
// token unrelated to the material's own bytes — in production this
// would be a real Vault/KMS-issued lease token, not derived locally.
func (b *LocalFileVaultBackend) Get(_ context.Context, secretPath string) (string, error) {
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

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("vault: failed to generate lease token: %w", err)
	}
	return "local-lease:" + base64.RawURLEncoding.EncodeToString(tokenBytes), nil
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
