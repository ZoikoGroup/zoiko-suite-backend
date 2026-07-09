// Package handler implements the ForwardAuth endpoint Traefik calls before
// routing any gated request to a backend service.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/jwks"
)

type Handler struct {
	cfg  *config.Config
	jwks *jwks.Client
	log  *zap.Logger
}

func New(cfg *config.Config, jwksClient *jwks.Client, log *zap.Logger) *Handler {
	return &Handler{cfg: cfg, jwks: jwksClient, log: log}
}

// envelopeClaims mirrors only the fields this gateway needs to propagate
// downstream. The full IdentityContextEnvelope shape is owned by
// identity-context-svc — this service reads a signed token, it never mints
// one.
type envelopeClaims struct {
	Principal struct {
		PrincipalID string `json:"principal_id"`
	} `json:"principal"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	CorrelationID string `json:"correlation_id"`
	jwt.RegisteredClaims
}

// Verify is called by Traefik's ForwardAuth middleware on every gated
// request. A 2xx response grants access and its headers are copied into the
// forwarded request (see authResponseHeaders in docker-compose.yml); any
// other status is returned to the client verbatim — fail-closed, the
// protected backend never sees an unverified request.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	rawToken, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		h.deny(w, "missing bearer token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	claims := &envelopeClaims{}
	_, err := jwt.ParseWithClaims(rawToken, claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, errors.New("unexpected signing method")
		}
		kid, ok := tok.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("token missing kid header")
		}
		return h.jwks.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(h.cfg.ExpectedIssuer),
		jwt.WithAudience(h.cfg.ExpectedAudience),
	)
	if err != nil {
		h.log.Info("gateway rejected request",
			zap.Error(err),
			zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
		)
		h.deny(w, "invalid token")
		return
	}

	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		h.deny(w, "invalid token")
		return
	}

	w.Header().Set("X-Principal-Id", claims.Principal.PrincipalID)
	w.Header().Set("X-Tenant-Id", claims.TenantID)
	w.Header().Set("X-Legal-Entity-Id", claims.LegalEntityID)
	if claims.CorrelationID != "" {
		w.Header().Set("X-Correlation-Id", claims.CorrelationID)
	}
	w.WriteHeader(http.StatusOK)
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	tok := strings.TrimPrefix(header, prefix)
	if tok == "" {
		return "", false
	}
	return tok, true
}

func (h *Handler) deny(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(msg))
}
