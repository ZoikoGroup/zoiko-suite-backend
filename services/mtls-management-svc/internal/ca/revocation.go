package ca

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Revocation distribution (Security & Trust §8.6 #2 / §11.3 #2, Governance
// audit register M-03, 13 Oct 2026).
//
// Revoking a certificate used to flip a status field in an in-memory map and
// nothing else. The certificate stayed cryptographically valid, no peer could
// learn that it had been revoked, and the flag itself was lost on the next
// restart. A revoked PEP certificate was therefore admitted by
// authorization-svc's mTLS listener until it expired.
//
// The ledger below records every revoked serial number in the CA's own data
// directory — the volume that already persists the CA key, so a revocation
// lives exactly as long as the CA that issued the certificate — and the CA
// signs an X.509 CRL from it (RFC 5280 §5). A CRL is public by design: it
// names serial numbers and nothing else, and its signature is what a verifier
// trusts, not the channel it came over. Verifiers fetch it from
// GET /v1/mtls/crl with no principal, which is what "a certificate-status
// endpoint a service can call without a principal" meant in M-03.

const revocationFile = "revocations.json"

// CRLValidity is how long a CRL stays current (its NextUpdate). A verifier
// holding a CRL older than this must treat revocation status as unknown.
// Verifiers refresh far more often than this; the window only bounds how long
// a verifier may keep admitting certificates while this service is down.
const CRLValidity = time.Hour

// RevokedSerial is one ledger entry.
type RevokedSerial struct {
	SerialNumber string    `json:"serial_number"` // hex, as IssuedCertificate.SerialNumber
	RevokedAt    time.Time `json:"revoked_at"`
	CertID       string    `json:"cert_id,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

// Ledger is the durable list of revoked serial numbers.
type Ledger struct {
	mu      sync.RWMutex
	path    string
	entries map[string]RevokedSerial
}

// OpenLedger loads (or starts) the ledger kept in dir.
func OpenLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ledger directory %q: %w", dir, err)
	}
	l := &Ledger{path: filepath.Join(dir, revocationFile), entries: map[string]RevokedSerial{}}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read revocation ledger: %w", err)
	}
	var list []RevokedSerial
	if err := json.Unmarshal(data, &list); err != nil {
		// Refuse to start rather than start with an empty list: an unreadable
		// ledger silently read as "nothing revoked" would un-revoke everything.
		return nil, fmt.Errorf("parse revocation ledger %s: %w", l.path, err)
	}
	for _, e := range list {
		l.entries[e.SerialNumber] = e
	}
	return l, nil
}

// Revoke records serials as revoked and persists the ledger before returning.
// An already-revoked serial keeps its original time. Nothing is recorded in
// memory unless the file write succeeds, so a failed write never reports a
// revocation that a restart would forget.
func (l *Ledger) Revoke(entries ...RevokedSerial) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make(map[string]RevokedSerial, len(l.entries)+len(entries))
	for k, v := range l.entries {
		next[k] = v
	}
	for _, e := range entries {
		if e.SerialNumber == "" {
			continue
		}
		if _, done := next[e.SerialNumber]; done {
			continue
		}
		if e.RevokedAt.IsZero() {
			e.RevokedAt = time.Now().UTC()
		}
		next[e.SerialNumber] = e
	}
	if err := writeLedger(l.path, next); err != nil {
		return err
	}
	l.entries = next
	return nil
}

// IsRevoked reports whether serial (hex) is on the ledger.
func (l *Ledger) IsRevoked(serial string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.entries[serial]
	return ok
}

// Entries returns the ledger sorted by revocation time.
func (l *Ledger) Entries() []RevokedSerial {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]RevokedSerial, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RevokedAt.Before(out[j].RevokedAt) })
	return out
}

func writeLedger(path string, entries map[string]RevokedSerial) error {
	list := make([]RevokedSerial, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SerialNumber < list[j].SerialNumber })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("encode revocation ledger: %w", err)
	}
	// Write-then-rename so a crash mid-write leaves the previous ledger, never
	// a truncated one (which OpenLedger would refuse).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write revocation ledger: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace revocation ledger: %w", err)
	}
	return nil
}

// SignCRL issues a CRL over entries, current from now for CRLValidity.
// The CRL number only has to increase between CRLs; nanoseconds do, across
// restarts too.
func (c *CA) SignCRL(entries []RevokedSerial, now time.Time) ([]byte, error) {
	revoked := make([]x509.RevocationListEntry, 0, len(entries))
	for _, e := range entries {
		serial, ok := new(big.Int).SetString(e.SerialNumber, 16)
		if !ok {
			return nil, fmt.Errorf("ledger holds a malformed serial %q", e.SerialNumber)
		}
		revoked = append(revoked, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: e.RevokedAt})
	}
	tmpl := &x509.RevocationList{
		RevokedCertificateEntries: revoked,
		Number:                    big.NewInt(now.UnixNano()),
		ThisUpdate:                now.Add(-time.Minute),
		NextUpdate:                now.Add(CRLValidity),
	}
	der, err := x509.CreateRevocationList(rand.Reader, tmpl, c.cert, c.key)
	if err != nil {
		return nil, fmt.Errorf("sign CRL: %w", err)
	}
	return der, nil
}

// CRLPEM wraps a DER CRL as PEM ("X509 CRL").
func CRLPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// Certificate is the CA's parsed certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }
