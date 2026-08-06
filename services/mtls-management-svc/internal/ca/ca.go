// Package ca implements a real internal Certificate Authority for
// mtls-management-svc.
//
// This replaces the service's previous stub, which fabricated a
// SerialNumber and Fingerprint from time.Now().UnixNano() math and never
// generated an actual key pair or certificate — see the removed
// GenerateCertificate function in internal/domain/types.go. That satisfied
// the *policy* layer the original Security Architecture spec calls for
// (docs/original_doc/zoiko_suite_doc5.txt) but not the underlying
// cryptography: a "certificate" with no real key pair cannot actually
// authenticate anything over TLS.
//
// Scope of this fix: a real self-signed root CA issues real leaf
// certificates via crypto/x509, with real serial numbers (crypto/rand, not
// time-based), real SHA-256 fingerprints of the actual DER bytes, and a
// real private key per leaf certificate.
//
// Documented limitation, not hidden: the CA's own private key is persisted
// to a local PEM file (path configurable, defaults under the service's data
// directory), not to a proper secrets vault. This project already has
// secret-vault-integration-svc and key-management-svc; backing this CA's
// key with one of those — rather than a local file — is the natural next
// step, and is deliberately NOT done here to keep this fix scoped to "make
// the cryptography real" rather than also wiring a new cross-service
// dependency without being asked to.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	caCommonName = "ZoikoSuite Internal CA"
	caCertFile   = "ca-cert.pem"
	caKeyFile    = "ca-key.pem"

	// caValidYears is deliberately long relative to leaf certificate
	// lifetimes (rotation_days on a leaf is typically 30-90 days) — the
	// root should long outlive any individual leaf it issues.
	caValidYears = 10

	// serialBits sizes the random serial number space. 128 bits makes an
	// accidental collision astronomically unlikely — this is the same
	// order of magnitude publicly-trusted CAs use.
	serialBits = 128
)

// CA holds a loaded (or freshly generated) internal certificate authority:
// its private key, in-memory certificate, and the certificate's PEM
// encoding (safe to hand out — it's the public root, not the private key).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

// LoadOrCreate loads a CA from dir (ca-cert.pem + ca-key.pem) if both files
// exist, or generates a new self-signed root and persists it to dir if not.
// dir is created if it doesn't exist.
//
// Loading (rather than always generating fresh) matters: every leaf
// certificate issued by a given CA is only trusted by verifiers that trust
// THAT CA's public cert. If the CA regenerated on every service restart,
// every previously issued leaf certificate would instantly become
// untrustable — a self-inflicted mass revocation on every deploy.
func LoadOrCreate(dir string) (*CA, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	if fileExists(certPath) && fileExists(keyPath) {
		return load(certPath, keyPath)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA directory %q: %w", dir, err)
	}
	return generate(certPath, keyPath)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func load(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEMBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s does not contain a valid PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEMBytes)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("%s does not contain a valid PEM EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA private key: %w", err)
	}

	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

func generate(certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{"ZoikoSuite"}},
		NotBefore:             now.Add(-5 * time.Minute), // small clock-skew tolerance
		NotAfter:              now.AddDate(caValidYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse freshly-signed CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// 0o600: the CA private key must not be world-readable even on a local
	// filesystem — this is the one file in this fix that actually matters
	// from a compromise-blast-radius perspective.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("persist CA key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("persist CA cert: %w", err)
	}

	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

// IssuedCertificate is the result of issuing (or rotating) a leaf
// certificate: everything the caller needs to hand the private key to the
// requesting service exactly once, and everything safe to persist/return
// on subsequent reads (which must NEVER include the private key).
type IssuedCertificate struct {
	CertificatePEM []byte // safe to store and return on GET — it's public
	PrivateKeyPEM  []byte // one-time only — the caller must not persist this
	SerialNumber   string
	Fingerprint    string // hex SHA-256 of the DER-encoded certificate
	NotBefore      time.Time
	NotAfter       time.Time
}

// IssueLeaf issues a new leaf certificate signed by this CA, for use as a
// service's mTLS identity. commonName is typically the service name;
// dnsNames lets the same cert be valid for additional SANs (e.g. a
// container's internal DNS name) if the caller has any — an empty slice is
// fine, CommonName alone is a valid (if legacy-style) identity.
func (c *CA) IssueLeaf(commonName string, dnsNames []string, validFor time.Duration) (*IssuedCertificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	notBefore := now.Add(-5 * time.Minute)
	notAfter := now.Add(validFor)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"ZoikoSuite"}},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// Both ClientAuth and ServerAuth: mTLS means this service's
		// certificate is presented as a SERVER cert to inbound callers and
		// as a CLIENT cert when it calls other services — the same leaf
		// cert plays both roles, per the platform's CommunicationPolicy
		// model (internal/domain/types.go).
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf certificate for %q: %w", commonName, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	sum := sha256.Sum256(der)

	return &IssuedCertificate{
		CertificatePEM: certPEM,
		PrivateKeyPEM:  keyPEM,
		SerialNumber:   serial.Text(16),
		Fingerprint:    "SHA256:" + hex.EncodeToString(sum[:]),
		NotBefore:      notBefore,
		NotAfter:       notAfter,
	}, nil
}

// CertificatePEM returns the CA's own public certificate, PEM-encoded —
// this is what every service verifying a peer's mTLS certificate needs as
// its trust root.
func (c *CA) CertificatePEM() []byte { return c.certPEM }

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialBits)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate random serial number: %w", err)
	}
	return serial, nil
}
