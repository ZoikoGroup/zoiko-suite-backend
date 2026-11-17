package govdecision

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// AlgEdDSA is the only integrity algorithm of contract 1.0.
const AlgEdDSA = "EdDSA"

var (
	// ErrUnsigned: the decision carries no integrity assertion.
	ErrUnsigned = errors.New("decision is not signed")
	// ErrUnknownKey: the assertion names a key the verifier does not hold.
	ErrUnknownKey = errors.New("decision signed by an unknown key")
	// ErrBadSignature: the signature does not verify over the decision.
	ErrBadSignature = errors.New("decision signature does not verify")
)

// Canonical returns v as canonical JSON: object keys sorted, numbers kept as
// their exact text, no HTML escaping. Any implementation that decodes the
// same JSON into a generic value and re-encodes it this way gets the same
// bytes, so a verifier need not share this package's Go types.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(raw)
}

func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Fingerprint is "sha256:" and the hex SHA-256 of v's canonical JSON: the
// input_fingerprint of a request (Doc 02 §2.2).
func Fingerprint(v any) (string, error) {
	c, err := Canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SigningInput is the canonical JSON of a decision without its integrity
// member: what the signature covers.
func SigningInput(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("decision is not a JSON object")
	}
	delete(obj, "integrity")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Signer signs decisions with one Ed25519 key.
type Signer struct {
	KeyID string
	key   ed25519.PrivateKey
}

// NewSigner returns a signer for a 32-byte Ed25519 seed. keyID "" derives one
// from the public key, so a rotated key gets a new id.
func NewSigner(seed []byte, keyID string) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("decision signing key must be a %d-byte Ed25519 seed, got %d bytes", ed25519.SeedSize, len(seed))
	}
	key := ed25519.NewKeyFromSeed(seed)
	if keyID == "" {
		sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
		keyID = "ed25519-" + hex.EncodeToString(sum[:6])
	}
	return &Signer{KeyID: keyID, key: key}, nil
}

// NewRandomSigner returns a signer with a fresh random key: right only for
// tests and developer tiers, whose decisions nobody verifies after a restart.
func NewRandomSigner() (*Signer, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return NewSigner(seed, "")
}

// Public returns the signer's public key.
func (s *Signer) Public() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// Sign sets d.Integrity to a signature over d and returns the signed
// decision's JSON, which is exactly what should be stored and served.
func (s *Signer) Sign(d *Decision) ([]byte, error) {
	d.Integrity = nil
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	input, err := SigningInput(raw)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(s.key, input)
	d.Integrity = &Integrity{Alg: AlgEdDSA, KeyID: s.KeyID, Value: base64.RawURLEncoding.EncodeToString(sig)}
	return json.Marshal(d)
}

// KeySet holds the public keys a verifier trusts, by key id.
type KeySet map[string]ed25519.PublicKey

// Verify checks raw's integrity assertion against keys.
func (k KeySet) Verify(raw []byte) error {
	var head struct {
		Integrity *Integrity `json:"integrity"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if head.Integrity == nil || head.Integrity.Value == "" {
		return ErrUnsigned
	}
	if head.Integrity.Alg != AlgEdDSA {
		return fmt.Errorf("%w: algorithm %q", ErrBadSignature, head.Integrity.Alg)
	}
	pub, ok := k[head.Integrity.KeyID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, head.Integrity.KeyID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(head.Integrity.Value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	input, err := SigningInput(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if !ed25519.Verify(pub, input, sig) {
		return ErrBadSignature
	}
	return nil
}

// JWK is one public key in JSON Web Key form (RFC 8037 OKP).
type JWK struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	X       string `json:"x"`
	KeyID   string `json:"kid"`
	Alg     string `json:"alg"`
	Use     string `json:"use"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK returns the signer's public key as a JWK.
func (s *Signer) JWK() JWK {
	return JWK{KeyType: "OKP", Curve: "Ed25519", X: base64.RawURLEncoding.EncodeToString(s.Public()), KeyID: s.KeyID, Alg: AlgEdDSA, Use: "sig"}
}

// ParseJWKS reads the Ed25519 keys of a JWKS document. Keys of other types
// are skipped; a set with no usable key is an error.
func ParseJWKS(raw []byte) (KeySet, error) {
	var set JWKS
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, err
	}
	out := KeySet{}
	for _, k := range set.Keys {
		if k.KeyType != "OKP" || k.Curve != "Ed25519" || k.KeyID == "" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		out[k.KeyID] = ed25519.PublicKey(x)
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS holds no Ed25519 key")
	}
	return out, nil
}
