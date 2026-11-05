package currency_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/currency"
)

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/currencies:validate", r.URL.Path)
		assert.Equal(t, "read", r.URL.Query().Get("operation"), "a rate is a reference fact, not a new posting")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestHTTPValidatorAnswersFromTheRegistry(t *testing.T) {
	ok := serve(t, 200, `{"code":"EUR","supported":true,"minor_unit":2}`)
	defer ok.Close()
	res, err := currency.NewHTTP(ok.URL, zap.NewNop()).Validate(context.Background(), "EUR")
	require.NoError(t, err)
	assert.True(t, res.Supported)
	require.NotNil(t, res.MinorUnit)
	assert.Equal(t, 2, *res.MinorUnit)

	no := serve(t, 200, `{"code":"XXZ","supported":false,"reason":"UNKNOWN_CURRENCY"}`)
	defer no.Close()
	res, err = currency.NewHTTP(no.URL, zap.NewNop()).Validate(context.Background(), "XXZ")
	require.NoError(t, err, "an unknown currency is an answer, not a failure")
	assert.False(t, res.Supported)
	assert.Equal(t, "UNKNOWN_CURRENCY", res.Reason)
}

func TestHTTPValidatorFailsClosed(t *testing.T) {
	for name, srv := range map[string]*httptest.Server{
		"server error": serve(t, 500, `{}`),
		"not found":    serve(t, 404, `{}`),
		"unreadable":   serve(t, 200, `not json`),
		"unauthorised": serve(t, 401, `{}`),
	} {
		_, err := currency.NewHTTP(srv.URL, zap.NewNop()).Validate(context.Background(), "EUR")
		assert.ErrorIs(t, err, currency.ErrUnavailable, name)
		srv.Close()
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err := currency.NewHTTP(url, zap.NewNop()).Validate(context.Background(), "EUR")
	assert.ErrorIs(t, err, currency.ErrUnavailable, "an unreachable registry is not an answer")
}

func TestMalformedCodesNeverReachTheRegistry(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	for _, bad := range []string{"", "eur", "EU", "EURO", "E1R", "EUR&operation=post"} {
		res, err := currency.NewHTTP(srv.URL, zap.NewNop()).Validate(context.Background(), bad)
		require.NoError(t, err)
		assert.False(t, res.Supported, bad)
	}
	assert.False(t, called)
}

func TestStubAndUnavailable(t *testing.T) {
	stub := currency.NewStub(zap.NewNop())
	res, err := stub.Validate(context.Background(), "EUR")
	require.NoError(t, err)
	assert.True(t, res.Supported)
	res, _ = stub.Validate(context.Background(), "eur")
	assert.False(t, res.Supported, "the development stub still checks shape")

	_, err = currency.Unavailable{}.Validate(context.Background(), "EUR")
	assert.ErrorIs(t, err, currency.ErrUnavailable, "with no registry configured outside development, nothing is accepted")
}
