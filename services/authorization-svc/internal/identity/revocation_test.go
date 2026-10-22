package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	idenv "zoiko.io/identity-envelope"
)

// statusSource answers each session from a map and counts the questions.
type statusSource struct {
	live  map[string]bool
	err   error
	asked []string
}

func (s *statusSource) SessionStatus(_ context.Context, _ string, c *idenv.Claims) (idenv.SessionStatus, error) {
	s.asked = append(s.asked, c.SessionContextID())
	if s.err != nil {
		return idenv.SessionStatus{}, s.err
	}
	return idenv.SessionStatus{Active: s.live[c.SessionContextID()], SessionContextID: c.SessionContextID()}, nil
}

func gateWith(e *env, src *statusSource, fail idenv.FailMode) *Gate {
	return e.gate(ModeRequire).WithRevocation(idenv.NewRevocation(idenv.RevocationOptions{Source: src, Fail: fail}))
}

// AU-01: after a logout the same envelope opened the admin routes until it
// expired. The gate now asks identity-context whether the session still
// stands.
func TestGate_RevokedSessionIsRefused(t *testing.T) {
	e := newEnv(t)
	src := &statusSource{live: map[string]bool{"SC-1": true}}
	g := gateWith(e, src, idenv.FailClosed)

	if w, s := serve(g, adminReq(e.envelope(t, nil, testKid), nil)); w.Code != http.StatusOK || !s.called {
		t.Fatalf("live session refused: %d %s", w.Code, w.Body)
	}
	revoked := e.envelope(t, func(c jwt.MapClaims) {
		c["session_trust_posture"] = map[string]any{"mfa_verified": true, "session_context_id": "SC-ENDED"}
	}, testKid)
	w, s := serve(g, adminReq(revoked, nil))
	if w.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("revoked session admitted: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session_revoked") {
		t.Fatalf("body = %s", w.Body)
	}
}

// An envelope that names no session cannot be revoked, so it is refused.
func TestGate_EnvelopeWithoutASessionIsRefused(t *testing.T) {
	e := newEnv(t)
	g := gateWith(e, &statusSource{live: map[string]bool{}}, idenv.FailClosed)
	tok := e.envelope(t, func(c jwt.MapClaims) {
		c["session_trust_posture"] = map[string]any{"mfa_verified": true}
	}, testKid)
	if w, _ := serve(g, adminReq(tok, nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// identity-context unreachable: fail closed by default (503), open only by
// explicit configuration.
func TestGate_RevocationUnknownFailsClosed(t *testing.T) {
	e := newEnv(t)
	down := &statusSource{err: errors.New("connection refused")}
	if w, s := serve(gateWith(e, down, idenv.FailClosed), adminReq(e.envelope(t, nil, testKid), nil)); w.Code != http.StatusServiceUnavailable || s.called {
		t.Fatalf("closed: %d called=%v", w.Code, s.called)
	}
	if w, s := serve(gateWith(e, down, idenv.FailOpen), adminReq(e.envelope(t, nil, testKid), nil)); w.Code != http.StatusOK || !s.called {
		t.Fatalf("open: %d called=%v", w.Code, s.called)
	}
}

// A delegate over verified mTLS carries no envelope and no session; the
// revocation check does not apply to it.
func TestGate_DelegateIsNotAskedAboutASession(t *testing.T) {
	e := newEnv(t)
	src := &statusSource{live: map[string]bool{}}
	g := e.gate(ModeRequire, "access-control-svc").WithRevocation(idenv.NewRevocation(idenv.RevocationOptions{Source: src}))
	req := withCert(adminReq("", map[string]string{"X-Principal-Id": "P-BOB", "X-Tenant-Id": tenantA}), "access-control-svc")
	if w, s := serve(g, req); w.Code != http.StatusOK || !s.called || len(src.asked) != 0 {
		t.Fatalf("delegate: %d called=%v asked=%v", w.Code, s.called, src.asked)
	}
}
