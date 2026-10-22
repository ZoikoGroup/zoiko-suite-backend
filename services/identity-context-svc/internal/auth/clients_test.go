package auth_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
)

func TestClientRegistry(t *testing.T) {
	reg := auth.NewClientRegistry(map[string]string{"zoiko-console": "s3cret-value"}, false)

	id, err := reg.Authenticate("zoiko-console", "s3cret-value")
	require.NoError(t, err)
	require.Equal(t, "zoiko-console", id)

	for name, c := range map[string][2]string{
		"wrong secret":   {"zoiko-console", "nope"},
		"empty secret":   {"zoiko-console", ""},
		"unknown client": {"rogue", "s3cret-value"},
		"secret, no id":  {"", "s3cret-value"},
	} {
		_, err := reg.Authenticate(c[0], c[1])
		require.ErrorIs(t, err, auth.ErrClientInvalid, name)
	}

	id, err = reg.Authenticate("", "")
	require.NoError(t, err)
	require.Empty(t, id, "no client presented and none required")

	_, err = auth.NewClientRegistry(map[string]string{"zoiko-console": "x"}, true).Authenticate("", "")
	require.True(t, errors.Is(err, auth.ErrClientRequired))
}

func TestLoadClientRegistry(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "clients.json")
	// Any well-formed 32-byte hex digest; only the file's shape is under test.
	require.NoError(t, os.WriteFile(good, []byte(`{"zoiko-console":"sha256:e0b6cde1e07b3e9c5d0b24a2c2f55e5ab1d0aa69a7ea0d3cbd6b1c6cdb5bb1d0"}`), 0o600))
	reg, err := auth.LoadClientRegistry(good, false)
	require.NoError(t, err)
	require.Equal(t, 1, reg.Count())

	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"zoiko-console":"plaintext"}`), 0o600))
	_, err = auth.LoadClientRegistry(bad, false)
	require.Error(t, err)

	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte(`{}`), 0o600))
	_, err = auth.LoadClientRegistry(empty, true)
	require.Error(t, err, "required with no clients")
}
