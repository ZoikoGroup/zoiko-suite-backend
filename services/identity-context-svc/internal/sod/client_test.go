package sod_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/sod"
)

// DoD gate 4: "Authorization/SoD enforcement verified server-side."
//
// Every test here is about one property: this client FAILS CLOSED. Treating
// "we could not ask" as "no conflict" means a GOV-04 outage silently disables
// the control rather than blocking on it, and nobody finds out until an audit.

func serving(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/sod/evaluate", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("X-Tenant-Id"), "the tenant scope must travel with the question")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func req() sod.Request {
	return sod.Request{
		TenantID:           "tenant-a",
		ActionType:         "IDENTITY_SUPPORT_CONTEXT_ATTACH",
		MakerPrincipalID:   "lead-1",
		CheckerPrincipalID: "lead-1",
		SubjectPrincipalID: "eng-2",
		CorrelationID:      "corr-1",
	}
}

func TestNoConflictIsPermitted(t *testing.T) {
	srv := serving(t, http.StatusOK, `{"result":"NO_CONFLICT","rule_version":"v7"}`)
	c := sod.NewHTTPClient(srv.URL, zap.NewNop())

	decision, err := c.CheckConflict(context.Background(), req())

	require.NoError(t, err)
	assert.Equal(t, "v7", decision.RuleVersion,
		"the deciding rule version is recorded so the decision stays reproducible")
}

func TestConflictIsRefused(t *testing.T) {
	srv := serving(t, http.StatusOK, `{"result":"CONFLICT","reason":"maker and checker share a role"}`)
	c := sod.NewHTTPClient(srv.URL, zap.NewNop())

	_, err := c.CheckConflict(context.Background(), req())

	require.ErrorIs(t, err, sod.ErrConflict)
	// Deliberately NOT folded into an authorization denial: a caller told
	// "authorization denied" will go and ask for a permission grant, which
	// cannot fix a segregation conflict and may well be granted — leaving the
	// conflict in place and the caller convinced it was resolved.
	assert.NotErrorIs(t, err, sod.ErrUnavailable)
}

// TestApprovedCompensatingControlClearsAConflict: only GOV-04 can say an
// exception applies, and its presence in the response IS that statement.
func TestApprovedCompensatingControlClearsAConflict(t *testing.T) {
	srv := serving(t, http.StatusOK,
		`{"result":"CONFLICT","reason":"shared role","exception_ref":"SODX-2026-11","rule_version":"v7"}`)
	c := sod.NewHTTPClient(srv.URL, zap.NewNop())

	decision, err := c.CheckConflict(context.Background(), req())

	require.NoError(t, err)
	assert.Equal(t, "SODX-2026-11", decision.ExceptionRef)
}

// ── Fail-closed ──────────────────────────────────────────────────────────────

func TestUnreachableServiceFailsClosed(t *testing.T) {
	// A port nothing is listening on.
	c := sod.NewHTTPClient("http://127.0.0.1:1", zap.NewNop())

	_, err := c.CheckConflict(context.Background(), req())
	require.ErrorIs(t, err, sod.ErrUnavailable)
}

func TestNon200FailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound} {
		srv := serving(t, status, `{"result":"NO_CONFLICT"}`)
		c := sod.NewHTTPClient(srv.URL, zap.NewNop())

		_, err := c.CheckConflict(context.Background(), req())
		require.ErrorIs(t, err, sod.ErrUnavailable,
			"status %d must not be read as approval even with a permissive body", status)
	}
}

func TestUndecodableBodyFailsClosed(t *testing.T) {
	srv := serving(t, http.StatusOK, `not json at all`)
	c := sod.NewHTTPClient(srv.URL, zap.NewNop())

	_, err := c.CheckConflict(context.Background(), req())
	require.ErrorIs(t, err, sod.ErrUnavailable)
}

// TestUnrecognisedVerdictFailsClosed guards the future.
//
// A later GOV-04 adding a third outcome must not have it read as approval
// here. Defaulting an unknown verdict to "permitted" is how a new deny state
// ships as a no-op.
func TestUnrecognisedVerdictFailsClosed(t *testing.T) {
	srv := serving(t, http.StatusOK, `{"result":"REQUIRES_DUAL_APPROVAL"}`)
	c := sod.NewHTTPClient(srv.URL, zap.NewNop())

	_, err := c.CheckConflict(context.Background(), req())
	require.ErrorIs(t, err, sod.ErrUnavailable)
}

func TestIncompleteRequestIsRefusedWithoutCallingOut(t *testing.T) {
	c := sod.NewHTTPClient("http://127.0.0.1:1", zap.NewNop())

	_, err := c.CheckConflict(context.Background(), sod.Request{ActionType: "X"})
	require.ErrorIs(t, err, sod.ErrUnavailable)
}

// ── Wiring guard ─────────────────────────────────────────────────────────────

// TestStubIsRefusedInProduction pairs with authz.NewClient's equivalent.
//
// A deployment that wired a real GOV-03 but a stubbed GOV-04 would satisfy
// "authorization enforced" while having no segregation control at all.
func TestStubIsRefusedInProduction(t *testing.T) {
	for _, env := range []string{"production", "staging", "PRODUCTION"} {
		_, err := sod.NewChecker(env, "", zap.NewNop())
		require.Error(t, err, "%s must not run without a real GOV-04", env)

		_, err = sod.NewChecker(env, "http://localhost:9999", zap.NewNop())
		require.Error(t, err, "%s must not accept a localhost placeholder", env)
	}
}

func TestStubIsAllowedInDevelopment(t *testing.T) {
	checker, err := sod.NewChecker("development", "", zap.NewNop())
	require.NoError(t, err)

	decision, err := checker.CheckConflict(context.Background(), req())
	require.NoError(t, err)
	assert.Equal(t, "NO_CONFLICT", decision.Result)
}

func TestRealClientIsUsedWhenAURLIsGivenInDevelopment(t *testing.T) {
	srv := serving(t, http.StatusOK, `{"result":"CONFLICT","reason":"nope"}`)
	checker, err := sod.NewChecker("development", srv.URL, zap.NewNop())
	require.NoError(t, err)

	_, err = checker.CheckConflict(context.Background(), req())
	require.ErrorIs(t, err, sod.ErrConflict,
		"a configured URL must be used even in development — the stub is a fallback, not a preference")
}
