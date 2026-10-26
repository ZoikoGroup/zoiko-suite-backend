// Package keys holds identity-context-svc's token signing keys (AU-05).
//
// Before it existed the envelope was signed with one PEM file under one kid,
// and the JWKS published that one key. Rotating it broke every live envelope
// at once, because a relying party seeing a new kid had no old key to check
// the tokens already in flight, and the login token was a different key
// altogether (an HS256 secret from the environment).
//
// A Keyring holds several keys:
//
//   - one ACTIVE key signs everything this service mints: envelopes, OAuth
//     access tokens, ID tokens;
//   - every key in the ring is PUBLISHED in the JWKS, so a rotation is
//     add-next → wait one JWKS cache TTL → switch active → wait one maximum
//     token lifetime → remove the old key, with no envelope failing to verify
//     at any step (RUNBOOK "Signing key rotation").
//
// A key is a Signer: a PEM file for local and CI, or a Cloud KMS asymmetric
// key version in GCP, where the private half never leaves the HSM/KMS and
// this process only ever holds the public key (ZS-SEC-001 §10–13).
package keys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"

	idenv "zoiko.io/identity-envelope"
)

// Signer is one signing key.
type Signer interface {
	Kid() string
	// Alg is the JWS algorithm: RS256 or ES256.
	Alg() string
	Public() crypto.PublicKey
	// SignDigest signs a SHA-256 digest and returns the JWS signature bytes
	// (PKCS#1 v1.5 for RS256, r||s for ES256).
	SignDigest(digest []byte) ([]byte, error)
}

// Keyring is the set of keys, one of them active.
type Keyring struct {
	mu     sync.RWMutex
	active Signer
	keys   map[string]crypto.PublicKey
	order  []string
}

// NewKeyring builds a ring. active signs; published are verification-only
// keys (a key being rotated in or out) and are also in the JWKS.
func NewKeyring(active Signer, published ...Signer) (*Keyring, error) {
	if active == nil {
		return nil, errors.New("keys: no active signing key")
	}
	k := &Keyring{active: active, keys: map[string]crypto.PublicKey{}}
	for _, s := range append([]Signer{active}, published...) {
		if s == nil {
			continue
		}
		if _, dup := k.keys[s.Kid()]; dup {
			return nil, fmt.Errorf("keys: duplicate kid %q", s.Kid())
		}
		k.keys[s.Kid()] = s.Public()
		k.order = append(k.order, s.Kid())
	}
	return k, nil
}

// AddPublic publishes a verification-only public key (a retired key whose
// tokens may still be live).
func (k *Keyring) AddPublic(kid string, pub crypto.PublicKey) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, dup := k.keys[kid]; dup {
		return fmt.Errorf("keys: duplicate kid %q", kid)
	}
	k.keys[kid] = pub
	k.order = append(k.order, kid)
	return nil
}

// ActiveKid is the kid new tokens are signed with.
func (k *Keyring) ActiveKid() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active.Kid()
}

// Kids are every published kid, active first.
func (k *Keyring) Kids() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return append([]string(nil), k.order...)
}

// KeySource is the ring as a verification key source.
func (k *Keyring) KeySource() idenv.StaticKeys {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := idenv.StaticKeys{}
	for kid, pub := range k.keys {
		out[kid] = pub
	}
	return out
}

// ActivePublic is the active key's public half.
func (k *Keyring) ActivePublic() crypto.PublicKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active.Public()
}

// JWKS renders every published key.
func (k *Keyring) JWKS() ([]idenv.JWK, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]idenv.JWK, 0, len(k.order))
	for _, kid := range k.order {
		j, err := idenv.JWKFromPublicKey(kid, k.keys[kid])
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// Sign produces a compact JWS over claims with the active key. typ sets the
// JOSE typ header ("JWT" when empty; "at+jwt" for access tokens, RFC 9068).
func (k *Keyring) Sign(claims any, typ string) (string, error) {
	k.mu.RLock()
	s := k.active
	k.mu.RUnlock()
	if typ == "" {
		typ = "JWT"
	}
	hdr, err := json.Marshal(map[string]string{"alg": s.Alg(), "kid": s.Kid(), "typ": typ})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := s.SignDigest(digest[:])
	if err != nil {
		return "", fmt.Errorf("sign with %s: %w", s.Kid(), err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ── PEM-file signer ─────────────────────────────────────────────────────────

type fileSigner struct {
	kid  string
	alg  string
	priv crypto.Signer
}

func (f *fileSigner) Kid() string              { return f.kid }
func (f *fileSigner) Alg() string              { return f.alg }
func (f *fileSigner) Public() crypto.PublicKey { return f.priv.Public() }

func (f *fileSigner) SignDigest(digest []byte) ([]byte, error) {
	switch p := f.priv.(type) {
	case *rsa.PrivateKey:
		return rsa.SignPKCS1v15(rand.Reader, p, crypto.SHA256, digest)
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, p, digest)
		if err != nil {
			return nil, err
		}
		return joseECDSA(r, s), nil
	default:
		return nil, fmt.Errorf("unsupported key type %T", f.priv)
	}
}

// NewPEMSigner reads a PKCS#1, PKCS#8 or SEC1 private key.
func NewPEMSigner(kid string, pemBytes []byte) (Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("key %q: no PEM block", kid)
	}
	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("key %q: %w", kid, err)
	}
	switch p := key.(type) {
	case *rsa.PrivateKey:
		if p.N.BitLen() < 2048 {
			return nil, fmt.Errorf("key %q: RSA keys must be at least 2048 bits", kid)
		}
		return &fileSigner{kid: kid, alg: "RS256", priv: p}, nil
	case *ecdsa.PrivateKey:
		if p.Curve.Params().Name != "P-256" {
			return nil, fmt.Errorf("key %q: only P-256 EC keys are supported", kid)
		}
		return &fileSigner{kid: kid, alg: "ES256", priv: p}, nil
	default:
		return nil, fmt.Errorf("key %q: unsupported key type %T", kid, key)
	}
}

// ParsePublicPEM reads a PKIX public key (a retired key published for
// verification only).
func ParsePublicPEM(pemBytes []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if block.Type == "RSA PUBLIC KEY" {
		return x509.ParsePKCS1PublicKey(block.Bytes)
	}
	return x509.ParsePKIXPublicKey(block.Bytes)
}

func joseECDSA(r, s *big.Int) []byte {
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

// ── Loading ─────────────────────────────────────────────────────────────────

// Spec is one configured key: kid=source where source is a file path, or
// gcpkms://projects/…/cryptoKeyVersions/N.
type Spec struct {
	Kid    string
	Source string
}

// ParseSpecs reads "kid=source,kid=source".
func ParseSpecs(s string) ([]Spec, error) {
	var out []Spec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kid, src, ok := strings.Cut(part, "=")
		kid, src = strings.TrimSpace(kid), strings.TrimSpace(src)
		if !ok || kid == "" || src == "" {
			return nil, fmt.Errorf("signing key %q: want kid=source", part)
		}
		out = append(out, Spec{Kid: kid, Source: src})
	}
	return out, nil
}

// Options are the loader's inputs.
type Options struct {
	// Signing keys; ActiveKid picks the signer (default: the first).
	Signing   []Spec
	ActiveKid string
	// VerifyOnly are public-key PEM files of retired keys still published.
	VerifyOnly []Spec
	// KMS signs for gcpkms:// sources.
	KMS KMSClient
}

// Load builds a ring from opts.
func Load(opts Options) (*Keyring, error) {
	if len(opts.Signing) == 0 {
		return nil, errors.New("keys: no signing key configured")
	}
	signers := make([]Signer, 0, len(opts.Signing))
	for _, sp := range opts.Signing {
		var s Signer
		var err error
		if strings.HasPrefix(sp.Source, "gcpkms://") {
			if opts.KMS == nil {
				return nil, fmt.Errorf("key %q: gcpkms source needs a KMS client", sp.Kid)
			}
			s, err = NewKMSSigner(sp.Kid, strings.TrimPrefix(sp.Source, "gcpkms://"), opts.KMS)
		} else {
			var b []byte
			b, err = os.ReadFile(sp.Source)
			if err == nil {
				s, err = NewPEMSigner(sp.Kid, b)
			}
		}
		if err != nil {
			return nil, err
		}
		signers = append(signers, s)
	}
	active := signers[0]
	if opts.ActiveKid != "" {
		active = nil
		for _, s := range signers {
			if s.Kid() == opts.ActiveKid {
				active = s
			}
		}
		if active == nil {
			return nil, fmt.Errorf("keys: active kid %q is not a configured signing key", opts.ActiveKid)
		}
	}
	var rest []Signer
	for _, s := range signers {
		if s != active {
			rest = append(rest, s)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Kid() < rest[j].Kid() })
	ring, err := NewKeyring(active, rest...)
	if err != nil {
		return nil, err
	}
	for _, v := range opts.VerifyOnly {
		b, err := os.ReadFile(v.Source)
		if err != nil {
			return nil, fmt.Errorf("verify-only key %q: %w", v.Kid, err)
		}
		pub, err := ParsePublicPEM(b)
		if err != nil {
			return nil, fmt.Errorf("verify-only key %q: %w", v.Kid, err)
		}
		if err := ring.AddPublic(v.Kid, pub); err != nil {
			return nil, err
		}
	}
	return ring, nil
}
