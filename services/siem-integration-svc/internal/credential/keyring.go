// Package credential seals SIEM exporter credentials at rest.
//
// This is the platform's existing at-rest mechanism for a secret the server
// must be able to use again (identity-context-svc internal/mfa/keyring.go,
// which seals TOTP shared secrets): AES-256-GCM, a key that lives outside the
// database, a key id recorded with each ciphertext so the key can be rotated,
// and additional authenticated data that binds a ciphertext to its row. It is
// copied, not reinvented — the same construction, parameters and encoding.
//
// Why: an exporter's auth_token is a live credential for the tenant's SIEM
// platform. With the records now in Postgres, a plaintext column would put it
// in every dump and backup. Doc 05 §13 ultimately calls for a vault reference
// instead (tracker row 8p-a, blocked on secret-vault-integration-svc returning
// material — row 81); this closes the plaintext-at-rest exposure until then.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Keyring encrypts and decrypts exporter credentials.
type Keyring struct {
	current string
	keys    map[string]cipher.AEAD
}

// ErrUnknownKey is a ciphertext sealed under a key id the ring does not hold.
var ErrUnknownKey = errors.New("credential sealed under an unknown key")

// NewKeyring builds a ring whose current key is keys[currentID]. Every key must
// be exactly 32 bytes (AES-256).
func NewKeyring(currentID string, keys map[string][]byte) (*Keyring, error) {
	if currentID == "" {
		return nil, errors.New("credential keyring: current key id is required")
	}
	if _, ok := keys[currentID]; !ok {
		return nil, fmt.Errorf("credential keyring: current key %q not provided", currentID)
	}
	kr := &Keyring{current: currentID, keys: make(map[string]cipher.AEAD, len(keys))}
	for id, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("credential keyring: key %q is %d bytes, want 32", id, len(k))
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("credential keyring: key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("credential keyring: key %q: %w", id, err)
		}
		kr.keys[id] = aead
	}
	return kr, nil
}

// ParseKeys reads the keyring configuration in identity-context-svc's format:
// current = base64 32-byte key, previous = "<id>:<base64>;<id>:<base64>".
// An empty current key returns (nil, nil): no keyring configured.
func ParseKeys(currentID, current, previous string) (*Keyring, error) {
	current = strings.TrimSpace(current)
	if current == "" {
		return nil, nil
	}
	if currentID == "" {
		currentID = "k1"
	}
	keys := map[string][]byte{}
	k, err := base64.StdEncoding.DecodeString(current)
	if err != nil {
		return nil, fmt.Errorf("credential key: not base64: %w", err)
	}
	keys[currentID] = k
	for _, part := range strings.Split(previous, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, b64, ok := strings.Cut(part, ":")
		if !ok || id == "" {
			return nil, errors.New("credential previous keys: want <id>:<base64>;…")
		}
		pk, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("credential previous key %q: not base64: %w", id, err)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("credential key id %q given twice", id)
		}
		keys[id] = pk
	}
	return NewKeyring(currentID, keys)
}

// CurrentKeyID is the id new credentials are sealed under.
func (k *Keyring) CurrentKeyID() string { return k.current }

// Seal encrypts plain under the current key, bound to aad. The result is
// base64(nonce || ciphertext || tag).
func (k *Keyring) Seal(plain []byte, aad string) (ciphertext, keyID string, err error) {
	aead := k.keys[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("credential seal nonce: %w", err)
	}
	out := aead.Seal(nonce, nonce, plain, []byte(aad))
	return base64.StdEncoding.EncodeToString(out), k.current, nil
}

// Open decrypts a ciphertext Seal produced under keyID with the same aad.
func (k *Keyring) Open(ciphertext, keyID, aad string) ([]byte, error) {
	aead, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, keyID)
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("credential open: decode: %w", err)
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("credential open: ciphertext too short")
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("credential open: %w", err)
	}
	return plain, nil
}

// ExporterAAD binds a sealed token to its exporter row, so a ciphertext copied
// onto another tenant's or exporter's row fails to open.
func ExporterAAD(tenantID, exporterID string) string {
	return "siem-exporter-token|" + tenantID + "|" + exporterID
}
