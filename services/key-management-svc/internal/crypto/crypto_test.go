package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const testKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func mustNew(t *testing.T, key string) *Cipher {
	t.Helper()
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew_RefusesAMissingOrWrongKey(t *testing.T) {
	for name, key := range map[string]string{
		"missing":    "",
		"not hex":    strings.Repeat("zz", 32),
		"16 bytes":   testKey[:32],
		"33 bytes":   testKey + "20",
		"odd length": testKey[:63],
	} {
		if _, err := New(key); err == nil {
			t.Errorf("%s: New accepted %q", name, key)
		}
	}
}

func TestRoundTripIsExact(t *testing.T) {
	c := mustNew(t, testKey)
	for _, plain := range [][]byte{[]byte("GB29NWBK60161331926819"), {}, bytes.Repeat([]byte{0xff, 0x00}, 2048)} {
		ct, err := c.Encrypt(plain, []byte("tenant|token"))
		if err != nil {
			t.Fatal(err)
		}
		if len(plain) > 0 && bytes.Contains(ct, plain) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := c.Decrypt(ct, []byte("tenant|token"))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("round trip = %q, %v; want %q", got, err, plain)
		}
	}
}

func TestEncryptIsRandomised(t *testing.T) {
	c := mustNew(t, testKey)
	a, _ := c.Encrypt([]byte("12345678"), nil)
	b, _ := c.Encrypt([]byte("12345678"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("the same value encrypted twice gave identical ciphertext (nonce reuse)")
	}
}

// Every way a ciphertext can fail to belong here is the same refusal.
func TestDecrypt_RefusesWhatItShouldNotOpen(t *testing.T) {
	c := mustNew(t, testKey)
	ct, _ := c.Encrypt([]byte("GB29NWBK60161331926819"), []byte("tenant-a|tok-1"))
	other := mustNew(t, strings.Repeat("ab", 32))
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01
	badVersion := append([]byte{0x02}, ct[1:]...)

	for name, try := range map[string]func() ([]byte, error){
		"wrong key":                func() ([]byte, error) { return other.Decrypt(ct, []byte("tenant-a|tok-1")) },
		"another tenant's context": func() ([]byte, error) { return c.Decrypt(ct, []byte("tenant-b|tok-1")) },
		"another token's context":  func() ([]byte, error) { return c.Decrypt(ct, []byte("tenant-a|tok-2")) },
		"tampered":                 func() ([]byte, error) { return c.Decrypt(tampered, []byte("tenant-a|tok-1")) },
		"truncated":                func() ([]byte, error) { return c.Decrypt(ct[:10], []byte("tenant-a|tok-1")) },
		"unknown format version":   func() ([]byte, error) { return c.Decrypt(badVersion, []byte("tenant-a|tok-1")) },
		"empty":                    func() ([]byte, error) { return c.Decrypt(nil, nil) },
	} {
		if got, err := try(); !errors.Is(err, ErrDecrypt) || got != nil {
			t.Errorf("%s: Decrypt = %q, %v; want ErrDecrypt and nothing", name, got, err)
		}
	}
}
