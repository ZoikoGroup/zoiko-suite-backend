package ar05

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSignatureRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"provider_event_id":"e1","status":"ACCEPTED"}`)
	now := testNow
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := Sign(secret, ts, "n-1", body)
	require.True(t, strings.HasPrefix(sig, "sha256="))
	got, err := VerifyCallback(secret, sig, ts, "n-1", body, now, DefaultCallbackWindow)
	require.NoError(t, err)
	require.Equal(t, now.Unix(), got.Unix())
}

func TestSignatureRejections(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"status":"ACCEPTED"}`)
	now := testNow
	ts := strconv.FormatInt(now.Unix(), 10)
	good := Sign(secret, ts, "n-1", body)

	cases := map[string]struct {
		secret         []byte
		sig, ts, nonce string
		body           []byte
		now            time.Time
		want           error
	}{
		"tampered body":      {secret, good, ts, "n-1", []byte(`{"status":"REJECTED"}`), now, ErrSignatureMismatch},
		"appended byte":      {secret, good, ts, "n-1", append(append([]byte{}, body...), ' '), now, ErrSignatureMismatch},
		"wrong secret":       {[]byte("other"), good, ts, "n-1", body, now, ErrSignatureMismatch},
		"swapped nonce":      {secret, good, ts, "n-2", body, now, ErrSignatureMismatch},
		"swapped timestamp":  {secret, good, strconv.FormatInt(now.Unix()+1, 10), "n-1", body, now, ErrSignatureMismatch},
		"missing signature":  {secret, "", ts, "n-1", body, now, ErrSignatureMissing},
		"missing timestamp":  {secret, good, "", "n-1", body, now, ErrSignatureMissing},
		"missing nonce":      {secret, good, ts, "", body, now, ErrSignatureMissing},
		"no prefix":          {secret, strings.TrimPrefix(good, "sha256="), ts, "n-1", body, now, ErrSignatureMalform},
		"not hex":            {secret, "sha256=zzzz", ts, "n-1", body, now, ErrSignatureMalform},
		"short digest":       {secret, "sha256=abcd", ts, "n-1", body, now, ErrSignatureMalform},
		"no secret":          {nil, good, ts, "n-1", body, now, ErrNoSecret},
		"stale (old)":        {secret, good, ts, "n-1", body, now.Add(DefaultCallbackWindow + time.Second), ErrCallbackStale},
		"stale (future)":     {secret, good, ts, "n-1", body, now.Add(-DefaultCallbackWindow - time.Second), ErrCallbackStale},
		"oversized nonce":    {secret, good, ts, strings.Repeat("n", 129), body, now, ErrSignatureMissing},
		"non numeric ts sig": {secret, Sign(secret, "abc", "n-1", body), "abc", "n-1", body, now, ErrSignatureMalform},
	}
	for name, c := range cases {
		_, err := VerifyCallback(c.secret, c.sig, c.ts, c.nonce, c.body, c.now, DefaultCallbackWindow)
		require.ErrorIs(t, err, c.want, name)
	}
}

func TestSignatureWindowEdges(t *testing.T) {
	secret := []byte("k")
	ts := strconv.FormatInt(testNow.Unix(), 10)
	sig := Sign(secret, ts, "n", nil)
	_, err := VerifyCallback(secret, sig, ts, "n", nil, testNow.Add(DefaultCallbackWindow), DefaultCallbackWindow)
	require.NoError(t, err, "exactly at the window edge is accepted")
	_, err = VerifyCallback(secret, sig, ts, "n", nil, testNow.Add(DefaultCallbackWindow+time.Second), DefaultCallbackWindow)
	require.ErrorIs(t, err, ErrCallbackStale)
}

func TestEnvSecrets(t *testing.T) {
	require.Equal(t, "AR_EINVOICE_CALLBACK_SECRET_ACME_PEPPOL_1", EnvSecretName("acme.peppol-1"))
	t.Setenv("AR_EINVOICE_CALLBACK_SECRET_ACME_PEPPOL_1", "topsecret")
	s, ok := EnvSecrets{}.Secret("acme.peppol-1")
	require.True(t, ok)
	require.Equal(t, []byte("topsecret"), s)
	_, ok = EnvSecrets{}.Secret("unknown")
	require.False(t, ok)
	_, ok = EnvSecrets{}.Secret("bad provider!") // not a valid provider id: never reaches the environment
	require.False(t, ok)
	t.Setenv("AR_EINVOICE_CALLBACK_SECRET_EMPTY", "")
	_, ok = EnvSecrets{}.Secret("empty")
	require.False(t, ok, "an empty secret is no secret")
	_, ok = MapSecrets{"p": []byte("x")}.Secret("p")
	require.True(t, ok)
	_, ok = MapSecrets{"p": nil}.Secret("p")
	require.False(t, ok)
}
