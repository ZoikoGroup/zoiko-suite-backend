// Package crypto encrypts token-vault values at rest: AES-256-GCM under one
// master key supplied by configuration (VAULT_MASTER_KEY_HEX).
//
// This is the same "real crypto now, better key infrastructure later"
// compromise document-vault-svc's storage.LocalFileBackend made and disclosed:
// the same cipher, the same 32-byte hex key from the environment, the same
// random-nonce-prefixed ciphertext, the same refusal to start without a valid
// key. It is NOT an HSM or a KMS-held key, which is what Security Standard
// S3/S4 handling ultimately calls for (SPEC_DEVIATIONS.md, section 2).
//
// Two additions over document-vault's backend, both small:
//
//   - Associated data. Encrypt and Decrypt take the context a ciphertext
//     belongs to (the vault passes tenant and token_id). GCM authenticates it
//     without storing it, so a ciphertext copied onto another tenant's row —
//     by anyone with write access to the table — fails to decrypt instead of
//     handing that tenant someone else's bank details.
//   - A one-byte format version in front of the nonce, so a later move to
//     per-tenant or KMS-wrapped keys can tell old ciphertexts from new ones
//     without guessing.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// EnvVarMasterKey is the configuration variable the key is read from.
const EnvVarMasterKey = "VAULT_MASTER_KEY_HEX"

// formatV1 marks a ciphertext as: version byte, 12-byte nonce, GCM sealed box,
// under the single configured master key.
const formatV1 byte = 0x01

// ErrDecrypt is returned for any ciphertext that does not open: wrong key,
// wrong associated data, truncation, tampering or an unknown format. The
// causes are deliberately not distinguished to callers.
var ErrDecrypt = errors.New("crypto: ciphertext could not be decrypted")

// Cipher is an AES-256-GCM AEAD under the configured master key. Safe for
// concurrent use.
type Cipher struct {
	gcm cipher.AEAD
}

// New builds a Cipher from a hex-encoded 32-byte key. It fails on a missing,
// non-hex or wrong-length key rather than running with weak or no
// encryption; cmd/server treats that failure as fatal.
func New(masterKeyHex string) (*Cipher, error) {
	if masterKeyHex == "" {
		return nil, fmt.Errorf("crypto: %s must be set (32-byte AES-256 key, hex-encoded)", EnvVarMasterKey)
	}
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("crypto: %s is not valid hex: %w", EnvVarMasterKey, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: %s must decode to 32 bytes (AES-256), got %d", EnvVarMasterKey, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to construct AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to construct GCM mode: %w", err)
	}
	return &Cipher{gcm: gcm}, nil
}

// Encrypt seals plaintext, binding associatedData into the authentication tag.
// The output is version || nonce || ciphertext+tag, with a fresh random nonce
// per call, so encrypting the same value twice gives different bytes.
func (c *Cipher) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: failed to generate nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+c.gcm.Overhead())
	out = append(out, formatV1)
	out = append(out, nonce...)
	return c.gcm.Seal(out, nonce, plaintext, associatedData), nil
}

// Decrypt opens a ciphertext produced by Encrypt with the same associated
// data. Any failure is ErrDecrypt.
func (c *Cipher) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	n := c.gcm.NonceSize()
	if len(ciphertext) < 1+n+c.gcm.Overhead() || ciphertext[0] != formatV1 {
		return nil, ErrDecrypt
	}
	nonce, sealed := ciphertext[1:1+n], ciphertext[1+n:]
	plaintext, err := c.gcm.Open(nil, nonce, sealed, associatedData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
