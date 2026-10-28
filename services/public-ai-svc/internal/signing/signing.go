// Package signing signs and verifies release artifacts with Ed25519
// (ADR-004 dev default; D08-03 signing infrastructure is still open).
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

var ErrBadSignature = errors.New("release signature is invalid")

// GenerateKey returns a new key pair, base64 (std) encoded.
func GenerateKey() (pub, priv string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k), nil
}

func decode(b64 string, size int, what string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != size {
		return nil, fmt.Errorf("invalid %s key", what)
	}
	return b, nil
}

// Sign signs payload with a base64 private key.
func Sign(privB64 string, payload []byte) ([]byte, error) {
	k, err := decode(privB64, ed25519.PrivateKeySize, "private")
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(ed25519.PrivateKey(k), payload), nil
}

// Verify checks sig over payload with a base64 public key.
func Verify(pubB64 string, payload, sig []byte) error {
	p, err := decode(pubB64, ed25519.PublicKeySize, "public")
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(p), payload, sig) {
		return ErrBadSignature
	}
	return nil
}

// Digest is the hex SHA-256 of payload, used in events and release records.
func Digest(payload []byte) string {
	s := sha256.Sum256(payload)
	return hex.EncodeToString(s[:])
}

// File is the on-disk form of a signed release. Manifest is base64 so the
// signed bytes survive JSON round-trips unchanged.
type File struct {
	ReleaseID string `json:"release_id"`
	Kind      string `json:"kind"`
	Manifest  string `json:"manifest_b64"`
	Signature string `json:"signature_b64"`
	Digest    string `json:"sha256"`
}

// WriteFile writes a signed release file.
func WriteFile(path, releaseID, kind string, manifest, sig []byte) error {
	f := File{ReleaseID: releaseID, Kind: kind, Manifest: base64.StdEncoding.EncodeToString(manifest),
		Signature: base64.StdEncoding.EncodeToString(sig), Digest: Digest(manifest)}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadFile reads a signed release file and returns its parts.
func ReadFile(path string) (File, []byte, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, nil, nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return File{}, nil, nil, err
	}
	m, err := base64.StdEncoding.DecodeString(f.Manifest)
	if err != nil {
		return File{}, nil, nil, err
	}
	s, err := base64.StdEncoding.DecodeString(f.Signature)
	if err != nil {
		return File{}, nil, nil, err
	}
	return f, m, s, nil
}
