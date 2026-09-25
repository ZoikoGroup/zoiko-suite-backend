package vault

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testKeyHex(t *testing.T) string {
	t.Helper()
	// 64 hex chars = 32 bytes AES-256.
	return "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
}

func TestLocalFileVaultBackend_LeaseTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	b, err := NewLocalFileVaultBackend(store, testKeyHex(t))
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}

	material := []byte("s3cr3t-material")
	if err := b.Put(ctx, "kv/db", material); err != nil {
		t.Fatalf("put: %v", err)
	}

	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	token, err := b.Get(ctx, "kv/db", expiresAt)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.HasPrefix(token, leaseTokenPrefix) {
		t.Fatalf("token has wrong prefix: %q", token)
	}

	info, err := b.Verify(ctx, token)
	if err != nil {
		t.Fatalf("verify valid token: %v", err)
	}
	if info.SecretPath != "kv/db" {
		t.Fatalf("verify path = %q, want kv/db", info.SecretPath)
	}
	if !info.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("verify expiry = %v, want %v", info.ExpiresAt, expiresAt)
	}
}

func TestLocalFileVaultBackend_ExpiredTokenRejected(t *testing.T) {
	b, err := NewLocalFileVaultBackend(filepath.Join(t.TempDir(), "s.json"), testKeyHex(t))
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	ctx := context.Background()
	if err := b.Put(ctx, "kv/db", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}

	past := time.Now().UTC().Add(-1 * time.Minute)
	token, err := b.Get(ctx, "kv/db", past)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := b.Verify(ctx, token); err != ErrLeaseTokenExpired {
		t.Fatalf("verify expired token = %v, want ErrLeaseTokenExpired", err)
	}
}

func TestLocalFileVaultBackend_TokenContextMissingPath(t *testing.T) {
	b, err := NewLocalFileVaultBackend(filepath.Join(t.TempDir(), "s.json"), testKeyHex(t))
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	ctx := context.Background()
	if _, err := b.Get(ctx, "kv/missing", time.Now().Add(time.Hour)); err != ErrSecretMaterialNotFound {
		t.Fatalf("get missing = %v, want ErrSecretMaterialNotFound", err)
	}
}

func TestLocalFileVaultBackend_VerifyRejectsForgeryAndTamper(t *testing.T) {
	b, err := NewLocalFileVaultBackend(filepath.Join(t.TempDir(), "s.json"), testKeyHex(t))
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	ctx := context.Background()

	// Hand-made payload with a WRONG signature must fail.
	tampered := "ltk:v2:" + "eyJzZWNyZXRfcGF0aCI6Im12L2RiIiwib2N0dWxsIjo" + "." + strings.Repeat("00", 64)
	if _, err := b.Verify(ctx, tampered); err != ErrLeaseTokenInvalid {
		t.Fatalf("verify forged token = %v, want ErrLeaseTokenInvalid", err)
	}

	if err := b.Put(ctx, "kv/db", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	valid, err := b.Get(ctx, "kv/db", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Flip a payload byte: signature stops validating.
	mangled := valid[:len(valid)-1] + "0"
	if valid[len(valid)-1] == '0' {
		mangled = valid[:len(valid)-1] + "1"
	}
	if _, err := b.Verify(ctx, mangled); err != ErrLeaseTokenInvalid {
		t.Fatalf("verify mangled token = %v, want ErrLeaseTokenInvalid", err)
	}
}

func TestLocalFileVaultBackend_RotateChangesMaterial(t *testing.T) {
	b, err := NewLocalFileVaultBackend(filepath.Join(t.TempDir(), "s.json"), testKeyHex(t))
	if err != nil {
		t.Fatalf("construct backend: %v", err)
	}
	ctx := context.Background()
	if err := b.Put(ctx, "kv/db", []byte("v1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	before, err := b.GetMaterial(ctx, "kv/db")
	if err != nil {
		t.Fatalf("get material before: %v", err)
	}
	if err := b.Rotate(ctx, "kv/db"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err := b.GetMaterial(ctx, "kv/db")
	if err != nil {
		t.Fatalf("get material after: %v", err)
	}
	if string(before) == string(after) {
		t.Fatalf("rotate produced identical material: %q", before)
	}
}

func TestNewLocalFileVaultBackendFromFile_RejectsGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced by the Windows OS; ACL governs key-file access here")
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	keyFile := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyFile, []byte(testKeyHex(t)), 0o644); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if _, err := NewLocalFileVaultBackendFromFile(store, keyFile); err == nil {
		t.Fatalf("expected refusal for group-readable key file (0644)")
	}
}

func TestNewLocalFileVaultBackendFromFile_AcceptsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	keyFile := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyFile, []byte(testKeyHex(t)+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	b, err := NewLocalFileVaultBackendFromFile(store, keyFile)
	if err != nil {
		t.Fatalf("construct from file: %v", err)
	}
	ctx := context.Background()
	if err := b.Put(ctx, "kv/db", []byte("y")); err != nil {
		t.Fatalf("put with file key: %v", err)
	}
}

func TestNewLocalFileVaultBackend_MasterKeyMustBe32Bytes(t *testing.T) {
	if _, err := NewLocalFileVaultBackend(t.TempDir()+"/s.json", hex.EncodeToString([]byte("short"))); err == nil {
		t.Fatalf("expected refusal for non-32-byte hex key")
	}
	if _, err := NewLocalFileVaultBackend(t.TempDir()+"/s.json", ""); err == nil {
		t.Fatalf("expected refusal for empty key")
	}
}