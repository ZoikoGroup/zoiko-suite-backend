package unsubscribe

import (
	"encoding/base64"
	"strings"
	"testing"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func codec(t *testing.T) *Codec {
	t.Helper()
	c, err := New(secret, "https://notify.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTripNormalizesAddress(t *testing.T) {
	c := codec(t)
	tok, err := c.Seal("tenant-a", "  Bob@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := c.Open(tok)
	if err != nil {
		t.Fatal(err)
	}
	if cl.TenantID != "tenant-a" || cl.Email != "bob@example.com" {
		t.Fatalf("got %+v", cl)
	}
}

func TestTokenDoesNotRevealAddress(t *testing.T) {
	tok, _ := codec(t).Seal("tenant-a", "bob@example.com")
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	if strings.Contains(tok, "bob") || strings.Contains(string(raw), "bob") || strings.Contains(string(raw), "tenant-a") {
		t.Fatal("the token must not carry the address or tenant in clear (§11.2)")
	}
}

func TestForgedEditedAndForeignTokensAreRefused(t *testing.T) {
	c := codec(t)
	tok, _ := c.Seal("tenant-a", "bob@example.com")

	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	raw[len(raw)-1] ^= 0x01
	edited := base64.RawURLEncoding.EncodeToString(raw)

	other, _ := New([]byte("ffffffffffffffffffffffffffffffff"), "https://x.test")
	foreign, _ := other.Seal("tenant-a", "bob@example.com")

	// The shape the receiver used to accept: tenant and address in clear.
	plain := base64.RawURLEncoding.EncodeToString([]byte("tenant-a.UNSUBSCRIBE.x.y"))

	for name, bad := range map[string]string{"edited": edited, "foreign key": foreign, "plain": plain, "empty": "", "garbage": "%%%"} {
		if _, err := c.Open(bad); err != ErrInvalidToken {
			t.Errorf("%s: want ErrInvalidToken, got %v", name, err)
		}
	}
}

func TestNewRefusesWeakSecretAndRelativeBase(t *testing.T) {
	if _, err := New([]byte("short"), "https://x.test"); err == nil {
		t.Error("a short secret must be refused")
	}
	if _, err := New(secret, "/v1"); err == nil {
		t.Error("a relative base URL must be refused: List-Unsubscribe needs an absolute URI")
	}
}

func TestHeadersAreOneClickWithNoMailto(t *testing.T) {
	h, err := codec(t).Headers("tenant-a", "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	u := h["List-Unsubscribe"]
	if !strings.HasPrefix(u, "<https://notify.example.test/v1/notifications/unsubscribe?token=") || strings.Contains(u, "mailto:") || strings.Contains(u, "bob") {
		t.Fatalf("List-Unsubscribe = %q", u)
	}
	if h["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post = %q", h["List-Unsubscribe-Post"])
	}
}
