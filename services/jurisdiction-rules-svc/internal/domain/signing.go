package domain

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ZS-JUR-001 Wave 1: artifact signing and verification (s5, s21, s28).
//
// The signing algorithm and key custody are controlled decisions (s38 "Signing
// / key hierarchy for pack releases") that the specification leaves to
// Security. What is implemented is the smallest mechanism that makes the
// documented invariants real, behind an interface so the custody can change
// without touching the compiler or the verifier:
//
//   - Ed25519 over the artifact digest, domain separated.
//   - The private key is loaded from a FILE (a mounted secret), never from an
//     environment variable, a database row or the artifact.
//   - Only PUBLIC keys are registered in the database, with a status. A runtime
//     loader can therefore verify but not forge.

// SigningDomain separates pack-artifact signatures from any other use of the key.
const SigningDomain = "ZS-JUR-001/pack-artifact/v1\n"

const AlgorithmEd25519 = "ED25519"

// Signing key statuses.
const (
	KeyActive  = "ACTIVE"
	KeyRetired = "RETIRED"
	KeyRevoked = "REVOKED"
)

// TrustedKey is a registered public key.
type TrustedKey struct {
	KeyRef       string  `json:"key_ref"`
	Algorithm    string  `json:"algorithm"`
	PublicKey    []byte  `json:"-"`
	PublicKeyB64 string  `json:"public_key"`
	Status       string  `json:"status"`
	StatusReason *string `json:"status_reason"`
	CreatedBy    string  `json:"created_by_principal_id"`
}

// Signer produces signatures for artifact digests.
type Signer interface {
	KeyRef() string
	PublicKey() []byte
	Sign(message []byte) ([]byte, error)
}

// SigningMessage is the exact byte string that is signed and verified.
func SigningMessage(artifactDigest string) []byte { return []byte(SigningDomain + artifactDigest) }

// Ed25519Signer signs with a key held in process memory, loaded from a file.
type Ed25519Signer struct {
	keyRef string
	priv   ed25519.PrivateKey
}

// NewEd25519Signer wraps an existing private key.
func NewEd25519Signer(keyRef string, priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if keyRef == "" {
		return nil, errors.New("signing key_ref is required")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("ed25519 private key must be %d bytes", ed25519.PrivateKeySize)
	}
	return &Ed25519Signer{keyRef: keyRef, priv: priv}, nil
}

// LoadEd25519SignerFromFile reads a base64 32-byte seed or 64-byte private key
// from path (a secret mounted by the secrets platform).
func LoadEd25519SignerFromFile(keyRef, path string) (*Ed25519Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key file: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("signing key file is not base64: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return NewEd25519Signer(keyRef, ed25519.NewKeyFromSeed(raw))
	case ed25519.PrivateKeySize:
		return NewEd25519Signer(keyRef, ed25519.PrivateKey(raw))
	default:
		return nil, fmt.Errorf("signing key must be a %d-byte seed or %d-byte key, got %d bytes", ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
}

func (s *Ed25519Signer) KeyRef() string { return s.keyRef }
func (s *Ed25519Signer) PublicKey() []byte {
	return append([]byte(nil), s.priv.Public().(ed25519.PublicKey)...)
}
func (s *Ed25519Signer) Sign(message []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, message), nil
}

// Verification reasons. A result is verified only when Reasons is empty.
const (
	ReasonUnsigned          = "unsigned"
	ReasonDigestMismatch    = "digest_mismatch"
	ReasonKeyUnknown        = "signing_key_unknown"
	ReasonKeyRevoked        = "signing_key_revoked"
	ReasonSignatureInvalid  = "signature_invalid"
	ReasonUnsupportedAlg    = "unsupported_algorithm"
	ReasonSignatureNotB64   = "signature_not_base64"
	ReasonRecordedDigestBad = "recorded_digest_mismatch"
)

// VerifyInput is what a loader has: the stored artifact bytes, the digest the
// registry recorded for them, the signature and the key it names.
type VerifyInput struct {
	ArtifactJSON   string
	StoredDigest   string // digest recorded when compiled
	VersionDigest  string // digest recorded on the pack version row ("" if none)
	Signature      *string
	SignatureKeyID *string
	Key            *TrustedKey // nil if the named key is not registered
}

// VerificationResult is the outcome of verifying a pack artifact.
type VerificationResult struct {
	Verified       bool     `json:"verified"`
	Reasons        []string `json:"reasons"`
	ComputedDigest string   `json:"computed_digest"`
	StoredDigest   string   `json:"stored_digest"`
	SignatureKeyID *string  `json:"signature_key_ref"`
}

// VerifyArtifact fails closed: any missing or inconsistent element is a
// reason, and the artifact is trusted only when there are none (JUR-NEG-03,
// JUR-NEG-04).
func VerifyArtifact(in VerifyInput) VerificationResult {
	res := VerificationResult{StoredDigest: in.StoredDigest, SignatureKeyID: in.SignatureKeyID, Reasons: []string{}}

	computed, err := DigestOf([]byte(in.ArtifactJSON))
	if err != nil {
		res.Reasons = append(res.Reasons, ReasonDigestMismatch)
	} else {
		res.ComputedDigest = computed
		if computed != in.StoredDigest {
			res.Reasons = append(res.Reasons, ReasonDigestMismatch)
		}
	}
	if in.VersionDigest != "" && in.VersionDigest != in.StoredDigest {
		res.Reasons = append(res.Reasons, ReasonRecordedDigestBad)
	}

	if in.Signature == nil || in.SignatureKeyID == nil || *in.Signature == "" {
		res.Reasons = append(res.Reasons, ReasonUnsigned)
		return res
	}
	if in.Key == nil {
		res.Reasons = append(res.Reasons, ReasonKeyUnknown)
		return res
	}
	if in.Key.Status == KeyRevoked {
		res.Reasons = append(res.Reasons, ReasonKeyRevoked)
		return res
	}
	if in.Key.Algorithm != AlgorithmEd25519 || len(in.Key.PublicKey) != ed25519.PublicKeySize {
		res.Reasons = append(res.Reasons, ReasonUnsupportedAlg)
		return res
	}
	sig, err := base64.StdEncoding.DecodeString(*in.Signature)
	if err != nil {
		res.Reasons = append(res.Reasons, ReasonSignatureNotB64)
		return res
	}
	// Verify against the digest the registry recorded, not a recomputed one:
	// a tampered artifact is already reported above, and this keeps the
	// signature check about the signature alone.
	if !ed25519.Verify(ed25519.PublicKey(in.Key.PublicKey), SigningMessage(in.StoredDigest), sig) {
		res.Reasons = append(res.Reasons, ReasonSignatureInvalid)
	}
	res.Verified = len(res.Reasons) == 0
	return res
}

// Errors for the compile/sign flow.
var (
	ErrCompileFailed        = errorString("pack compile failed; see the report")
	ErrAlreadyCompiled      = errorString("this pack version already has a compiled artifact with different content")
	ErrNotCompiled          = errorString("the pack version has no compiled artifact")
	ErrNotUnderReview       = errorString("a pack version can only be compiled while under REVIEW")
	ErrSigningNotConfigured = errorString("no signing key is configured for this service")
	ErrSigningKeyNotActive  = errorString("the configured signing key is not registered and ACTIVE with the same public key")
	ErrAlreadySigned        = errorString("the artifact is already signed")
	ErrKeyNotFound          = errorString("signing key not found")
	ErrKeyMismatch          = errorString("a key with this key_ref is already registered with a different public key")
)

// VerifyCertification checks a certification report signature against a
// registered key, fail-closed, with the same rules as VerifyArtifact.
func VerifyCertification(reportJSON, reportDigest string, signature, keyRef *string, key *TrustedKey) VerificationResult {
	res := VerificationResult{StoredDigest: reportDigest, SignatureKeyID: keyRef, Reasons: []string{}}
	computed, err := DigestOf([]byte(reportJSON))
	if err != nil || computed != reportDigest {
		res.Reasons = append(res.Reasons, ReasonDigestMismatch)
	}
	res.ComputedDigest = computed
	switch {
	case signature == nil || keyRef == nil || *signature == "":
		res.Reasons = append(res.Reasons, ReasonUnsigned)
	case key == nil:
		res.Reasons = append(res.Reasons, ReasonKeyUnknown)
	case key.Status == KeyRevoked:
		res.Reasons = append(res.Reasons, ReasonKeyRevoked)
	case key.Algorithm != AlgorithmEd25519 || len(key.PublicKey) != ed25519.PublicKeySize:
		res.Reasons = append(res.Reasons, ReasonUnsupportedAlg)
	default:
		sig, derr := base64.StdEncoding.DecodeString(*signature)
		if derr != nil {
			res.Reasons = append(res.Reasons, ReasonSignatureNotB64)
		} else if !ed25519.Verify(ed25519.PublicKey(key.PublicKey), CertificationSigningMessage(reportDigest), sig) {
			res.Reasons = append(res.Reasons, ReasonSignatureInvalid)
		}
	}
	res.Verified = len(res.Reasons) == 0
	return res
}

// VerifyLoadedPack applies every trust check a consumer of a pack must apply:
// the artifact (digest, recorded digests, registered non-revoked key,
// signature) AND a certification for exactly that digest whose own signature
// verifies. An empty result means the pack is trustworthy. It is the single
// definition used by the runtime resolver and by publication, so the two can
// never disagree about what "trusted" means.
func VerifyLoadedPack(lp *LoadedPack) []string {
	vr := VerifyArtifact(VerifyInput{ArtifactJSON: lp.ArtifactJSON, StoredDigest: lp.ArtifactDigest,
		VersionDigest: lp.VersionDigest, Signature: lp.Signature, SignatureKeyID: lp.KeyRef, Key: lp.Key})
	reasons := append([]string{}, vr.Reasons...)
	switch {
	case lp.Cert == nil:
		reasons = append(reasons, "not_certified")
	case lp.Cert.ArtifactDigest != lp.ArtifactDigest:
		reasons = append(reasons, "certification_artifact_mismatch")
	default:
		sig, kref := lp.Cert.Signature, lp.Cert.KeyRef
		cv := VerifyCertification(lp.Cert.ReportJSON, lp.Cert.ReportDigest, &sig, &kref, lp.Cert.Key)
		for _, x := range cv.Reasons {
			reasons = append(reasons, "certification_"+x)
		}
	}
	return reasons
}
