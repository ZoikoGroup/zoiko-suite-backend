package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/eventing/egress"
	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/authz"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/store"
)

// sessionEvictor ends every live session of a principal, recording why.
type sessionEvictor struct {
	cache interface {
		EvictAllForPrincipal(ctx context.Context, principalID, tenantID string, reason domain.InvalidationReason) (int, error)
	}
}

func (e sessionEvictor) EvictAllForPrincipal(ctx context.Context, principalID, tenantID string) (int, error) {
	return e.cache.EvictAllForPrincipal(ctx, principalID, tenantID, domain.InvalidationReasonAdminRevoke)
}

// mountFederation wires INT-10 onto the router when FEDERATION_ENABLED, and returns the handler the server
// should serve plus a function that stops its background worker. It must be called after the platform
// middleware is installed (chi requires middleware before routes).
//
// The SCIM endpoints are served by a separate router that is NOT behind the envelope or replay middleware: a
// directory has no platform identity. They are reached through a thin prefix split in front of the main router.
func mountFederation(r chi.Router, fc config.FederationConfig, pc config.ProvisioningConfig, cfg *config.Config, repo *store.PgStore,
	issuer *auth.IdPTokenIssuer, authzClient authz.Client, siemClient *siem.Client, sessions sessionEvictor, log *zap.Logger) (http.Handler, func()) {
	if !fc.Enabled {
		log.Info("federation disabled (FEDERATION_ENABLED not set)")
		return r, func() {}
	}
	nets, err := egress.ParseCIDRs(fc.PrivateCIDRs)
	if err != nil {
		log.Fatal("FEDERATION_EGRESS_PRIVATE_CIDRS is not a list of CIDRs", zap.Error(err))
	}
	policy := egress.Policy{AllowHTTP: fc.AllowHTTP, AllowedPorts: fc.AllowedPorts, PrivateNets: nets}

	keys := federation.NewKeyManager(repo, &federation.HTTPFetcher{Client: policy.NewHTTPClient(8 * time.Second)}, repo, log)
	keys.RefreshAfter = time.Duration(fc.JWKSRefreshSeconds) * time.Second
	keys.MinRefresh = time.Duration(fc.JWKSMinRefreshSeconds) * time.Second

	svc := federation.NewService(repo, policy, keys, log)
	login := federation.NewLogin(repo, federation.NewVerifier(keys), repo, issuer, siemClient, log)
	handler := federation.NewHandler(svc, login, authzClient, log)

	stop := func() {}
	var scim http.Handler
	if pc.Enabled {
		ac := federation.NewACClient(cfg.AccessControlURL, pc.ServicePrincipalID, 5*time.Second)
		prov, err := federation.NewProvisioning(repo, repo, ac, sessions, log)
		if err != nil {
			log.Fatal("federation provisioning could not be wired", zap.Error(err))
		}
		if pc.RetryBaseSeconds > 0 {
			prov.BackoffBase = time.Duration(pc.RetryBaseSeconds) * time.Second
		}
		if pc.RevokeRetryMaxSeconds > 0 {
			prov.RevokeBackoffMax = time.Duration(pc.RevokeRetryMaxSeconds) * time.Second
		}
		handler.WithProvisioning(prov)
		login.WithProvisioner(prov)
		scim = handler.SCIMRouter()
		if pc.WorkerIntervalSeconds > 0 {
			wctx, cancel := context.WithCancel(context.Background())
			go prov.Run(wctx, time.Duration(pc.WorkerIntervalSeconds)*time.Second)
			stop = cancel
		} else {
			log.Warn("federation worker is disabled: queued assignment work and late deprovisioning will not be driven")
		}
	}
	handler.Register(r)
	log.Info("federation enabled", zap.Bool("allow_http", fc.AllowHTTP), zap.Int("private_nets", len(nets)), zap.Bool("provisioning", pc.Enabled))

	if scim == nil {
		return r, stop
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/scim/v2/") {
			scim.ServeHTTP(w, req)
			return
		}
		r.ServeHTTP(w, req)
	}), stop
}
