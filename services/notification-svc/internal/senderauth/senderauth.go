// Package senderauth signs outbound mail with DKIM and watches the DNS that
// makes the signature meaningful (ZS-SVC-Y-001 §11.1, §13.2, NP-55).
//
// The service used to send unsigned mail and had no idea whether the sending
// domain's DKIM, DMARC or SPF records existed. §11.1 asks for signed streams
// with controlled keys; NP-55 asks that when authentication breaks, the email
// stream is suspended "rather than continue in a degraded, spoofable mode".
//
// Both are optional, switched on by configuring a DKIM key: many hosted
// providers sign on the sender's behalf, and then this service has nothing to
// sign with and no key to check DNS against.
package senderauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// signedHeaders are covered by the signature. List-Unsubscribe and
// List-Unsubscribe-Post are included because RFC 8058 §4 requires a one-click
// unsubscribe to be covered by a valid DKIM signature. A listed header the
// message does not carry is simply absent from the hash.
var signedHeaders = []string{
	"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type",
	"Content-Transfer-Encoding", "List-Unsubscribe", "List-Unsubscribe-Post",
}

// Signer DKIM-signs raw RFC 5322 messages.
type Signer struct {
	domain   string
	selector string
	key      crypto.Signer
}

// NewSigner parses a PEM private key (PKCS#8, or PKCS#1 RSA) for d=domain,
// s=selector. RSA keys must be at least 2048 bits.
func NewSigner(domain, selector string, privateKeyPEM []byte) (*Signer, error) {
	domain, selector = strings.TrimSpace(strings.ToLower(domain)), strings.TrimSpace(selector)
	if domain == "" || selector == "" {
		return nil, errors.New("dkim: domain and selector are required")
	}
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("dkim: private key is not PEM")
	}
	var key crypto.Signer
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		s, ok := k.(crypto.Signer)
		if !ok {
			return nil, errors.New("dkim: unsupported private key type")
		}
		key = s
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("dkim: private key is neither PKCS#8 nor PKCS#1")
	}
	switch k := key.Public().(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, fmt.Errorf("dkim: RSA key is %d bits; at least 2048 are required", k.N.BitLen())
		}
	case ed25519.PublicKey:
	default:
		return nil, errors.New("dkim: key must be RSA or Ed25519")
	}
	return &Signer{domain: domain, selector: selector, key: key}, nil
}

// Domain and Selector name the DNS record the signature points at.
func (s *Signer) Domain() string   { return s.domain }
func (s *Signer) Selector() string { return s.selector }

// Sign returns raw with a DKIM-Signature header prepended.
func (s *Signer) Sign(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	err := dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{
		Domain: s.domain, Selector: s.selector, Signer: s.key, Hash: crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: signedHeaders,
	})
	if err != nil {
		return nil, fmt.Errorf("dkim sign: %w", err)
	}
	return out.Bytes(), nil
}

// PublicKeyRecord is the value of p= the selector's TXT record must publish.
func (s *Signer) PublicKeyRecord() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(s.key.Public())
	if err != nil {
		return "", err
	}
	if pk, ok := s.key.Public().(ed25519.PublicKey); ok {
		// RFC 8463: an Ed25519 p= is the raw 32-byte key, not SPKI.
		return base64.StdEncoding.EncodeToString(pk), nil
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ── NP-55 monitor ───────────────────────────────────────────────────────────

// LookupTXT resolves TXT records; net.DefaultResolver in production.
type LookupTXT func(ctx context.Context, name string) ([]string, error)

// Monitor checks that DNS still authenticates the mail this service signs and
// holds the email stream while it does not.
type Monitor struct {
	signer *Signer
	lookup LookupTXT

	mu     sync.RWMutex
	broken string // empty when healthy
}

// NewMonitor returns a monitor for signer's domain. It starts healthy: the
// first Check settles it, and only a definite answer ever suspends mail.
func NewMonitor(signer *Signer, lookup LookupTXT) *Monitor {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	return &Monitor{signer: signer, lookup: lookup}
}

// Healthy reports whether mail may be sent, and if not, why.
func (m *Monitor) Healthy() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.broken == "", m.broken
}

// errTransient marks a lookup that did not get an answer (timeout, SERVFAIL):
// it says nothing about whether authentication is broken.
var errTransient = errors.New("dns lookup did not complete")

// Check re-reads DNS and updates the verdict. A lookup that could not complete
// leaves the previous verdict standing — a resolver blip must not stop mail,
// and must not restart it either. It returns the problem found, if any.
func (m *Monitor) Check(ctx context.Context) error {
	problem, err := m.evaluate(ctx)
	if errors.Is(err, errTransient) {
		return err
	}
	m.mu.Lock()
	m.broken = problem
	m.mu.Unlock()
	if problem != "" {
		return errors.New(problem)
	}
	return nil
}

func (m *Monitor) evaluate(ctx context.Context) (string, error) {
	d := m.signer.domain

	want, err := m.signer.PublicKeyRecord()
	if err != nil {
		return "", err
	}
	recs, err := m.txt(ctx, m.signer.selector+"._domainkey."+d)
	if err != nil {
		return "", err
	}
	if !hasKey(recs, want) {
		return fmt.Sprintf("DKIM record %s._domainkey.%s does not publish the signing key", m.signer.selector, d), nil
	}

	recs, err = m.txt(ctx, "_dmarc."+d)
	if err != nil {
		return "", err
	}
	if !hasPrefix(recs, "v=DMARC1") {
		return "no DMARC record at _dmarc." + d, nil
	}

	recs, err = m.txt(ctx, d)
	if err != nil {
		return "", err
	}
	if !hasPrefix(recs, "v=spf1") {
		return "no SPF record at " + d, nil
	}
	return "", nil
}

// txt resolves name; "no such record" is an empty answer, anything that did
// not complete is errTransient.
func (m *Monitor) txt(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recs, err := m.lookup(ctx, name)
	if err == nil {
		return recs, nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil, nil
	}
	return nil, fmt.Errorf("%w: %s: %v", errTransient, name, err)
}

// Run checks every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, interval time.Duration, onChange func(healthy bool, why string)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		before, _ := m.Healthy()
		_ = m.Check(ctx)
		if after, why := m.Healthy(); after != before && onChange != nil {
			onChange(after, why)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hasKey reports whether any record is a DKIM key record publishing want.
// TXT strings may be split, and tags are whitespace-tolerant (RFC 6376 §3.2).
func hasKey(recs []string, want string) bool {
	for _, r := range recs {
		for _, tag := range strings.Split(r, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(tag), "=")
			if ok && strings.TrimSpace(k) == "p" && strings.Join(strings.Fields(v), "") == want {
				return true
			}
		}
	}
	return false
}

func hasPrefix(recs []string, prefix string) bool {
	for _, r := range recs {
		if strings.HasPrefix(strings.TrimSpace(r), prefix) {
			return true
		}
	}
	return false
}
