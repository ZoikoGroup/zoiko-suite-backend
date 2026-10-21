package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/gateway-auth-svc/internal/carta"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/siem"
	"zoiko.io/gateway-auth-svc/internal/tenantctx"
)

type sessions struct {
	live map[string]bool
	err  error
}

func (s sessions) SessionStatus(_ context.Context, _ string, c *idenv.Claims) (idenv.SessionStatus, error) {
	if s.err != nil {
		return idenv.SessionStatus{}, s.err
	}
	return idenv.SessionStatus{Active: s.live[c.SessionContextID()], SessionContextID: c.SessionContextID()}, nil
}

func withSession(id string) jwt.MapClaims {
	c := validClaims()
	c["session_trust_posture"] = map[string]any{"mfa_verified": true, "session_context_id": id}
	return c
}

func verify(h *handler.Handler, tok, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Forwarded-Method", method)
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	return rec
}

// AU-01: after a logout the same envelope passed /verify until it expired.
func TestVerify_RevokedSessionIsRefused(t *testing.T) {
	h, key, _ := newTestEnv(t)
	h.UseRevocation(idenv.NewRevocation(idenv.RevocationOptions{Source: sessions{live: map[string]bool{"SC-LIVE": true}}}))

	if rec := verify(h, mintEnvelope(t, key, testKid, withSession("SC-LIVE")), http.MethodGet); rec.Code != http.StatusOK {
		t.Fatalf("live session refused: %d", rec.Code)
	}
	rec := verify(h, mintEnvelope(t, key, testKid, withSession("SC-ENDED")), http.MethodGet)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "session_revoked", rec.Header().Get("X-Auth-Denial-Reason"))
	assert.Empty(t, rec.Header().Get("X-Principal-Id"), "a refused request must forward no identity")
}

func TestVerify_SessionStatusUnknownFailsClosed(t *testing.T) {
	h, key, _ := newTestEnv(t)
	h.UseRevocation(idenv.NewRevocation(idenv.RevocationOptions{Source: sessions{err: errors.New("identity-context down")}}))
	rec := verify(h, mintEnvelope(t, key, testKid, withSession("SC-1")), http.MethodGet)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "session_status_unavailable", rec.Header().Get("X-Auth-Denial-Reason"))
}

// AU-23 (decision 21 Oct 2026): carta-svc configured but unreachable fails
// closed on a material write and open on a read.
func TestVerify_CartaUnreachable_ClosedOnWritesOpenOnReads(t *testing.T) {
	_, key, cfg := newTestEnv(t)
	log := zap.NewNop()
	dead := handler.New(cfg, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL), carta.New("http://127.0.0.1:1", log),
		siem.New("", "gateway-auth-svc", log), tenantctx.New("", cfg.TenantContextTTL, cfg.TenantContextStaleGrace), log)
	tok := mintEnvelope(t, key, testKid, validClaims())

	write := verify(dead, tok, http.MethodPost)
	assert.Equal(t, http.StatusServiceUnavailable, write.Code)
	assert.Equal(t, "carta_unavailable", write.Header().Get("X-Auth-Denial-Reason"))

	read := verify(dead, tok, http.MethodGet)
	assert.Equal(t, http.StatusOK, read.Code, "a read proceeds unscored when carta-svc is down")
}
