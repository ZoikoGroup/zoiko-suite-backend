package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RevocationChecker refuses client certificates mtls-management-svc has
// revoked (Governance audit N-11 / register M-03, 13 Oct 2026).
//
// The mTLS listener verified a caller's certificate chain and nothing else, so
// a PEP certificate revoked in mtls-management-svc was admitted until it
// expired — up to 90 days. mtls-management-svc now publishes a CRL signed by
// its CA at GET /v1/mtls/crl. This checker fetches it on an interval, accepts
// it only if the CA that issued this listener's trust root signed it and it is
// not older than the one already held, and is consulted on every handshake.
//
// Fail closed: without a current CRL (none fetched yet, or the held one is
// past its NextUpdate) revocation status is unknown and the handshake is
// refused. A plaintext caller in permit mode is unaffected; a caller that has
// moved to mTLS is refused rather than admitted on a certificate whose status
// nobody can vouch for.
type RevocationChecker struct {
	url     string
	client  *http.Client
	issuers func() []*x509.Certificate

	mu         sync.RWMutex
	number     *big.Int
	nextUpdate time.Time
	revoked    map[string]bool // serial, base 16
}

// NewRevocationChecker fetches from crlURL. issuers returns the CA
// certificates a CRL must be signed by — the listener's current trust roots.
func NewRevocationChecker(crlURL string, issuers func() []*x509.Certificate) *RevocationChecker {
	return &RevocationChecker{url: crlURL, client: &http.Client{Timeout: 10 * time.Second}, issuers: issuers}
}

// ErrRevocationUnknown is returned while no current CRL is held.
var ErrRevocationUnknown = errors.New("certificate revocation status unknown: no current CRL from mtls-management-svc")

// Refresh fetches and installs the CRL.
func (rc *RevocationChecker) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rc.url, nil)
	if err != nil {
		return err
	}
	resp, err := rc.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch CRL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch CRL: mtls-management-svc returned %d", resp.StatusCode)
	}
	der, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read CRL: %w", err)
	}
	return rc.Install(der, time.Now())
}

// Install verifies and installs a DER CRL. Exported for tests.
func (rc *RevocationChecker) Install(der []byte, now time.Time) error {
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		return fmt.Errorf("parse CRL: %w", err)
	}
	signed := false
	for _, issuer := range rc.issuers() {
		if crl.CheckSignatureFrom(issuer) == nil {
			signed = true
			break
		}
	}
	if !signed {
		return errors.New("CRL is not signed by this listener's CA")
	}
	if !crl.NextUpdate.IsZero() && now.After(crl.NextUpdate) {
		return fmt.Errorf("CRL expired at %s", crl.NextUpdate)
	}
	revoked := make(map[string]bool, len(crl.RevokedCertificateEntries))
	for _, e := range crl.RevokedCertificateEntries {
		revoked[e.SerialNumber.Text(16)] = true
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// No rollback: a replayed older CRL could un-revoke a certificate.
	if rc.number != nil && crl.Number != nil && crl.Number.Cmp(rc.number) < 0 {
		return fmt.Errorf("CRL number %s is older than the held %s", crl.Number, rc.number)
	}
	rc.number, rc.nextUpdate, rc.revoked = crl.Number, crl.NextUpdate, revoked
	return nil
}

// Check refuses a revoked certificate, or any certificate while status is
// unknown.
func (rc *RevocationChecker) Check(cert *x509.Certificate) error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if rc.revoked == nil || (!rc.nextUpdate.IsZero() && time.Now().After(rc.nextUpdate)) {
		return ErrRevocationUnknown
	}
	if rc.revoked[cert.SerialNumber.Text(16)] {
		return fmt.Errorf("client certificate %s (%s) is revoked", cert.SerialNumber.Text(16), cert.Subject.CommonName)
	}
	return nil
}

// VerifyConnection is the tls.Config hook: it checks the verified leaf.
func (rc *RevocationChecker) VerifyConnection(cs tls.ConnectionState) error {
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return nil // no client certificate verified; ClientAuth decides
	}
	return rc.Check(cs.VerifiedChains[0][0])
}

// Run refreshes every interval until ctx ends. Failures are logged; the held
// CRL keeps serving until its NextUpdate, then every handshake is refused.
func (rc *RevocationChecker) Run(ctx context.Context, interval time.Duration, log *zap.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := rc.Refresh(ctx); err != nil && ctx.Err() == nil {
			log.Warn("mtls: CRL refresh failed — the held CRL serves until its NextUpdate, then mTLS handshakes are refused", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
