package mfa

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

var rfcSecret = []byte("12345678901234567890")

// RFC 4226 Appendix D: HOTP values for counters 0..9.
func TestCode_RFC4226Vectors(t *testing.T) {
	want := []string{"755224", "287082", "359152", "969429", "338314",
		"254676", "287922", "162583", "399871", "520489"}
	for i, w := range want {
		if got := Code(rfcSecret, int64(i)); got != w {
			t.Fatalf("counter %d: got %s want %s", i, got, w)
		}
	}
}

// RFC 6238 Appendix B (SHA1 column), truncated to 6 digits.
func TestCode_RFC6238Vectors(t *testing.T) {
	cases := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for unix, w := range cases {
		if got := Code(rfcSecret, Step(time.Unix(unix, 0))); got != w {
			t.Fatalf("t=%d: got %s want %s", unix, got, w)
		}
	}
}

func TestMatch_WindowAndStep(t *testing.T) {
	now := time.Unix(1111111111, 0)
	cur := Step(now)

	step, ok := Match(rfcSecret, Code(rfcSecret, cur), now, 1)
	if !ok || step != cur {
		t.Fatalf("current code: ok=%v step=%d want %d", ok, step, cur)
	}
	if step, ok = Match(rfcSecret, Code(rfcSecret, cur-1), now, 1); !ok || step != cur-1 {
		t.Fatalf("previous window should match with skew 1: ok=%v step=%d", ok, step)
	}
	if _, ok = Match(rfcSecret, Code(rfcSecret, cur-2), now, 1); ok {
		t.Fatal("a code two windows old must not match with skew 1")
	}
	if _, ok = Match(rfcSecret, Code(rfcSecret, cur-1), now, 0); ok {
		t.Fatal("skew 0 must accept only the current window")
	}
	if _, ok = Match([]byte("another-secret-entirely"), Code(rfcSecret, cur), now, 1); ok {
		t.Fatal("a code for a different secret must not match")
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"123456": "123456", " 123 456 ": "123456", "123-456": "123456"} {
		got, err := NormalizeCode(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", "١٢٣٤٥٦"} {
		if _, err := NormalizeCode(bad); !errors.Is(err, ErrMalformedCode) {
			t.Fatalf("%q: want ErrMalformedCode, got %v", bad, err)
		}
	}
}

func TestSecretAndURI(t *testing.T) {
	s, err := NewSecret()
	if err != nil || len(s) != SecretBytes {
		t.Fatalf("NewSecret: %d bytes, %v", len(s), err)
	}
	s2, _ := NewSecret()
	if bytes.Equal(s, s2) {
		t.Fatal("two secrets must differ")
	}
	uri := OTPAuthURI("ZoikoSuite", "a.user@example.com", EncodeSecret(s))
	for _, part := range []string{"otpauth://totp/ZoikoSuite:a.user@example.com?", "issuer=ZoikoSuite", "digits=6", "period=30", "algorithm=SHA1", "secret=" + EncodeSecret(s)} {
		if !strings.Contains(uri, part) {
			t.Fatalf("uri %q missing %q", uri, part)
		}
	}
}

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestKeyring_SealOpenAndBinding(t *testing.T) {
	kr, err := NewKeyring("k1", map[string][]byte{"k1": testKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	aad := FactorAAD("t1", "p1", "f1")
	ct, kid, err := kr.Seal(rfcSecret, aad)
	if err != nil || kid != "k1" {
		t.Fatalf("seal: %v kid=%s", err, kid)
	}
	if strings.Contains(ct, string(rfcSecret)) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := kr.Open(ct, kid, aad)
	if err != nil || !bytes.Equal(got, rfcSecret) {
		t.Fatalf("open: %v", err)
	}
	// The same ciphertext on another principal's row must not open.
	if _, err := kr.Open(ct, kid, FactorAAD("t1", "p2", "f1")); err == nil {
		t.Fatal("ciphertext moved to another principal opened")
	}
	if _, err := kr.Open(ct, "k9", aad); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key id: %v", err)
	}
}

func TestKeyring_Rotation(t *testing.T) {
	old, _ := NewKeyring("k1", map[string][]byte{"k1": testKey(1)})
	aad := FactorAAD("t", "p", "f")
	ct, kid, _ := old.Seal(rfcSecret, aad)

	rotated, err := NewKeyring("k2", map[string][]byte{"k1": testKey(1), "k2": testKey(2)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(ct, kid, aad); err != nil || !bytes.Equal(got, rfcSecret) {
		t.Fatalf("old ciphertext after rotation: %v", err)
	}
	if _, kid2, _ := rotated.Seal(rfcSecret, aad); kid2 != "k2" {
		t.Fatalf("new secrets must use the current key, got %s", kid2)
	}
}

func TestKeyring_RejectsBadKeys(t *testing.T) {
	if _, err := NewKeyring("k1", map[string][]byte{"k1": []byte("short")}); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewKeyring("k2", map[string][]byte{"k1": testKey(1)}); err == nil {
		t.Fatal("missing current key accepted")
	}
	if _, err := NewKeyring("", map[string][]byte{"k1": testKey(1)}); err == nil {
		t.Fatal("empty current id accepted")
	}
}
