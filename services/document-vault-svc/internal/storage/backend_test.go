package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/storage"
)

const testKeyHex = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

func TestNewLocalFileBackend_MissingKey_Errors(t *testing.T) {
	_, err := storage.NewLocalFileBackend(t.TempDir(), "")
	require.Error(t, err)
}

func TestNewLocalFileBackend_WrongLengthKey_Errors(t *testing.T) {
	_, err := storage.NewLocalFileBackend(t.TempDir(), "abcd")
	require.Error(t, err)
}

func TestPutGet_RoundTrip_MatchesOriginalBytesAndChecksum(t *testing.T) {
	b, err := storage.NewLocalFileBackend(t.TempDir(), testKeyHex)
	require.NoError(t, err)

	original := []byte("the quick brown fox jumps over the lazy dog")
	checksum, err := b.Put(context.Background(), "key-1", original)
	require.NoError(t, err)
	require.NotEmpty(t, checksum)

	got, err := b.Get(context.Background(), "key-1", checksum)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestGet_WrongChecksum_ReturnsIntegrityFailure(t *testing.T) {
	b, err := storage.NewLocalFileBackend(t.TempDir(), testKeyHex)
	require.NoError(t, err)

	_, err = b.Put(context.Background(), "key-1", []byte("real content"))
	require.NoError(t, err)

	_, err = b.Get(context.Background(), "key-1", "0000000000000000000000000000000000000000000000000000000000000")
	assert.ErrorIs(t, err, storage.ErrIntegrityFailure)
}

func TestGet_TamperedFileOnDisk_DetectedAsIntegrityFailureOrDecryptError(t *testing.T) {
	// Proves tamper detection is REAL, not just a checksum-string comparison:
	// corrupt the actual encrypted bytes on disk (simulating an attacker or
	// storage corruption) and confirm Get refuses to return the content.
	dir := t.TempDir()
	b, err := storage.NewLocalFileBackend(dir, testKeyHex)
	require.NoError(t, err)

	checksum, err := b.Put(context.Background(), "key-1", []byte("original content"))
	require.NoError(t, err)

	// Corrupt the on-disk ciphertext directly.
	path := filepath.Join(dir, "key-1.enc")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xFF // flip the last byte
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	_, err = b.Get(context.Background(), "key-1", checksum)
	require.Error(t, err, "tampered ciphertext must never be returned as valid content")
}

func TestGet_UnknownKey_ReturnsNotFound(t *testing.T) {
	b, err := storage.NewLocalFileBackend(t.TempDir(), testKeyHex)
	require.NoError(t, err)

	_, err = b.Get(context.Background(), "does-not-exist", "irrelevant")
	assert.ErrorIs(t, err, storage.ErrObjectNotFound)
}

func TestPut_DifferentKeys_ProduceDifferentChecksumsForDifferentContent(t *testing.T) {
	b, err := storage.NewLocalFileBackend(t.TempDir(), testKeyHex)
	require.NoError(t, err)

	c1, err := b.Put(context.Background(), "a", []byte("content A"))
	require.NoError(t, err)
	c2, err := b.Put(context.Background(), "b", []byte("content B"))
	require.NoError(t, err)

	assert.NotEqual(t, c1, c2)
}
