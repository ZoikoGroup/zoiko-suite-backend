package senderauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func rsaPEM(t *testing.T, bits int) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func signer(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner("zoiko.example", "s2026", rsaPEM(t, 2048))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const msg = "From: Zoiko <no-reply@zoiko.example>\r\nTo: pat@example.test\r\nSubject: Offers\r\n" +
	"Date: Mon, 05 Oct 2026 10:00:00 +0000\r\nMessage-ID: <1@zoiko.example>\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"List-Unsubscribe: <https://notify.example.test/v1/notifications/unsubscribe?token=abc>\r\n" +
	"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\n<p>hello</p>\r\n"

// dns serves fixed TXT records; a name maps to records, or to an error.
type dns map[string]any

func (d dns) lookup(_ context.Context, name string) ([]string, error) {
	switch v := d[name].(type) {
	case []string:
		return v, nil
	case error:
		return nil, v
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func healthyDNS(t *testing.T, s *Signer) dns {
	t.Helper()
	p, err := s.PublicKeyRecord()
	if err != nil {
		t.Fatal(err)
	}
	return dns{
		"s2026._domainkey.zoiko.example": []string{"v=DKIM1; k=rsa; p=" + p},
		"_dmarc.zoiko.example":           []string{"v=DMARC1; p=reject; rua=mailto:dmarc@zoiko.example"},
		"zoiko.example":                  []string{"v=spf1 include:_spf.provider.example -all"},
	}
}

// The signature verifies against the record the monitor checks for, and it
// covers the RFC 8058 headers (RFC 8058 §4).
func TestSignedMessageVerifiesAndCoversOneClickHeaders(t *testing.T) {
	s := signer(t)
	signed, err := s.Sign([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	records := healthyDNS(t, s)
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) { return records.lookup(context.Background(), name) },
	})
	if err != nil || len(vs) != 1 || vs[0].Err != nil || vs[0].Domain != "zoiko.example" {
		t.Fatalf("signature must verify: %+v, %v", vs, err)
	}
	covered := strings.ToLower(strings.Join(vs[0].HeaderKeys, ":"))
	for _, h := range []string{"from", "subject", "list-unsubscribe", "list-unsubscribe-post"} {
		if !strings.Contains(covered, h) {
			t.Errorf("%s must be covered by the signature; h=%s", h, covered)
		}
	}

	// Editing a signed header breaks the signature.
	tampered := bytes.Replace(signed, []byte("token=abc"), []byte("token=xyz"), 1)
	vs, _ = dkim.VerifyWithOptions(bytes.NewReader(tampered), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) { return records.lookup(context.Background(), name) },
	})
	if len(vs) != 1 || vs[0].Err == nil {
		t.Fatal("a changed List-Unsubscribe must fail verification")
	}
}

func TestNewSignerRefusesWeakOrMissingKeys(t *testing.T) {
	if _, err := NewSigner("zoiko.example", "s", rsaPEM(t, 1024)); err == nil {
		t.Error("a 1024-bit RSA key must be refused")
	}
	if _, err := NewSigner("", "s", rsaPEM(t, 2048)); err == nil {
		t.Error("a missing domain must be refused")
	}
	if _, err := NewSigner("zoiko.example", "s", []byte("not pem")); err == nil {
		t.Error("a non-PEM key must be refused")
	}
}

// NP-55: when DNS stops authenticating the mail, the stream is held; when it
// is fixed, it is released; a resolver failure changes nothing.
func TestMonitorSuspendsOnBrokenAuthenticationOnly(t *testing.T) {
	s := signer(t)
	records := healthyDNS(t, s)
	m := NewMonitor(s, records.lookup)
	ctx := context.Background()

	if err := m.Check(ctx); err != nil {
		t.Fatalf("healthy DNS: %v", err)
	}
	if ok, _ := m.Healthy(); !ok {
		t.Fatal("healthy DNS must leave mail flowing")
	}

	cases := map[string]func(dns){
		"rotated DKIM key":  func(d dns) { d["s2026._domainkey.zoiko.example"] = []string{"v=DKIM1; k=rsa; p=AAAA"} },
		"DKIM record gone":  func(d dns) { delete(d, "s2026._domainkey.zoiko.example") },
		"DMARC record gone": func(d dns) { delete(d, "_dmarc.zoiko.example") },
		"SPF record gone":   func(d dns) { d["zoiko.example"] = []string{"google-site-verification=x"} },
	}
	for name, breakIt := range cases {
		broken := healthyDNS(t, s)
		breakIt(broken)
		m.lookup = broken.lookup
		if err := m.Check(ctx); err == nil {
			t.Errorf("%s: Check must report the problem", name)
		}
		if ok, why := m.Healthy(); ok || why == "" {
			t.Errorf("%s: mail must be held, got healthy=%v", name, ok)
		}

		// A resolver failure neither releases nor holds: the verdict stands.
		m.lookup = dns{"s2026._domainkey.zoiko.example": errors.New("i/o timeout")}.lookup
		_ = m.Check(ctx)
		if ok, _ := m.Healthy(); ok {
			t.Errorf("%s: a resolver timeout must not release a held stream", name)
		}

		m.lookup = records.lookup
		if err := m.Check(ctx); err != nil {
			t.Errorf("%s: fixed DNS must release the stream: %v", name, err)
		}
	}

	m.lookup = dns{"s2026._domainkey.zoiko.example": errors.New("i/o timeout")}.lookup
	_ = m.Check(ctx)
	if ok, _ := m.Healthy(); !ok {
		t.Error("a resolver timeout must not hold a healthy stream")
	}
}

// A key published with the record split across TXT strings and spaces still
// matches (RFC 6376 §3.2 tag-list whitespace).
func TestHasKeyToleratesWhitespace(t *testing.T) {
	if !hasKey([]string{"v=DKIM1; k=rsa; p=AB CD\tEF "}, "ABCDEF") {
		t.Error("whitespace inside p= must be ignored")
	}
}
