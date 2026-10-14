package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Keyring encrypts TOTP shared secrets at rest with AES-256-GCM.
//
// A TOTP secret cannot be hashed the way a password is: the server needs the
// secret itself to compute the expected code. So it is encrypted instead, and
// the key lives outside the database (MFA_SECRET_KEY), which means a dump of
// principal_credentials alone yields nothing that produces a valid code.
//
// Each ciphertext records the id of the key that sealed it, so a key can be
// rotated: the new key becomes current and the old one stays in the ring for
// decryption until every factor has been re-enrolled.
//
// Additional authenticated data binds a ciphertext to its row (tenant,
// principal, factor). Copying one principal's ciphertext onto another
// principal's row therefore fails to decrypt instead of silently giving the
// second principal the first one's factor.
type Keyring struct {
	current string
	keys    map[string]cipher.AEAD
}

// ErrUnknownKey is a ciphertext sealed under a key id the ring does not hold.
var ErrUnknownKey = errors.New("mfa secret sealed under an unknown key")

// NewKeyring builds a ring whose current key is keys[currentID]. Every key must
// be exactly 32 bytes (AES-256).
func NewKeyring(currentID string, keys map[string][]byte) (*Keyring, error) {
	if currentID == "" {
		return nil, errors.New("mfa keyring: current key id is required")
	}
	if _, ok := keys[currentID]; !ok {
		return nil, fmt.Errorf("mfa keyring: current key %q not provided", currentID)
	}
	kr := &Keyring{current: currentID, keys: make(map[string]cipher.AEAD, len(keys))}
	for id, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("mfa keyring: key %q is %d bytes, want 32", id, len(k))
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("mfa keyring: key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("mfa keyring: key %q: %w", id, err)
		}
		kr.keys[id] = aead
	}
	return kr, nil
}

// CurrentKeyID is the id new secrets are sealed under.
func (k *Keyring) CurrentKeyID() string { return k.current }

// Seal encrypts plain under the current key, bound to aad. The result is
// base64(nonce || ciphertext || tag).
func (k *Keyring) Seal(plain []byte, aad string) (ciphertext, keyID string, err error) {
	aead := k.keys[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("mfa seal nonce: %w", err)
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
		return nil, fmt.Errorf("mfa open: decode: %w", err)
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("mfa open: ciphertext too short")
	}
	nonce, body := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("mfa open: %w", err)
	}
	return plain, nil
}

// FactorAAD is the additional authenticated data a factor's secret is sealed
// with: the row it belongs to.
func FactorAAD(tenantID, principalID, factorID string) string {
	return "totp|" + tenantID + "|" + principalID + "|" + factorID
}
