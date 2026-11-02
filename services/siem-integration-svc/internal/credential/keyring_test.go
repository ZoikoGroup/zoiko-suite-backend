package credential

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func TestSealOpenRoundTripAndAADBinding(t *testing.T) {
	kr, err := ParseKeys("k1", key(1), "")
	if err != nil {
		t.Fatal(err)
	}
	ct, kid, err := kr.Seal([]byte("splunk-hec-token"), ExporterAAD("t1", "e1"))
	if err != nil || kid != "k1" {
		t.Fatalf("seal: %v %s", err, kid)
	}
	if strings.Contains(ct, "splunk-hec-token") {
		t.Fatal("ciphertext contains the plaintext")
	}
	plain, err := kr.Open(ct, kid, ExporterAAD("t1", "e1"))
	if err != nil || string(plain) != "splunk-hec-token" {
		t.Fatalf("open: %v %q", err, plain)
	}
	if _, err := kr.Open(ct, kid, ExporterAAD("t2", "e1")); err == nil {
		t.Fatal("a ciphertext moved to another tenant's row must not open")
	}
}

func TestRotationKeepsOldKeyForOpening(t *testing.T) {
	old, _ := ParseKeys("k1", key(1), "")
	ct, kid, _ := old.Seal([]byte("x"), "aad")
	rotated, err := ParseKeys("k2", key(2), "k1:"+key(1))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := rotated.Open(ct, kid, "aad"); err != nil || string(p) != "x" {
		t.Fatalf("old ciphertext after rotation: %v", err)
	}
	if _, err := rotated.Open(ct, "k9", "aad"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key id: %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	if kr, err := ParseKeys("", "", ""); kr != nil || err != nil {
		t.Fatalf("unset key must mean no keyring: %v %v", kr, err)
	}
	if _, err := ParseKeys("k1", base64.StdEncoding.EncodeToString([]byte("short")), ""); err == nil {
		t.Fatal("a non-32-byte key must be refused")
	}
	if _, err := ParseKeys("k1", "%%%", ""); err == nil {
		t.Fatal("non-base64 must be refused")
	}
}
