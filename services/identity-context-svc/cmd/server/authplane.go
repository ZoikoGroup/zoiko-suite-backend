package main

// Construction of the auth plane added on 14–15 Oct 2026 (the OIDC provider,
// login throttling, the multi-key keyring, DPoP at resolve and the subject
// lifecycle's status command). All of it was compiled and none of it was
// constructed until 19 Oct (Group 1 re-audit F-5); main.go calls these.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/oidc"
	"zoiko.io/identity-context-svc/internal/ratelimit"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/store"
)

// Compile-time proof that the Postgres store is everything the auth plane
// asks of it. A missing method fails the build here, not a request.
var (
	_ oidc.Store      = (*store.PgStore)(nil)
	_ lifecycle.Store = (*store.PgStore)(nil)
)

// loadKeyring builds the signing keyring. JWT_SIGNING_KEYS wins when set;
// otherwise the single key at JWT_SIGNING_PRIVATE_KEY_PATH is the ring.
func loadKeyring(cfg *config.Config) (*keys.Keyring, error) {
	opts := keys.Options{
		Signing:   []keys.Spec{{Kid: cfg.JWTKeyID, Source: cfg.JWTSigningPrivateKeyPath}},
		ActiveKid: cfg.JWTKeyID,
	}
	if strings.TrimSpace(cfg.AuthPlane.SigningKeys) != "" {
		specs, err := keys.ParseSpecs(cfg.AuthPlane.SigningKeys)
		if err != nil {
			return nil, err
		}
		opts.Signing, opts.ActiveKid = specs, cfg.AuthPlane.SigningActiveKid
	}
	verifyOnly, err := keys.ParseSpecs(cfg.AuthPlane.VerifyOnlyKeys)
	if err != nil {
		return nil, err
	}
	opts.VerifyOnly = verifyOnly
	for _, s := range opts.Signing {
		if strings.HasPrefix(s.Source, "gcpkms://") {
			opts.KMS = keys.NewGCPKMS()
			break
		}
	}
	return keys.Load(opts)
}

// newLoginLimiter is AU-04's source-level throttle, shared by
// /v1/authenticate, /v1/authenticate/step-up and the hosted login. Counters
// live in Redis so every replica sees one budget; the limiter falls back to
// process memory when Redis errors rather than failing open.
func newLoginLimiter(cfg *config.Config, rdb *redis.Client, log *zap.Logger) *ratelimit.Limiter {
	ap := cfg.AuthPlane
	return ratelimit.New(ratelimit.NewRedisCounter(rdb), ratelimit.Limits{
		AttemptsPerIPPerMinute: ap.AuthRatePerIPMin,
		FailuresPerIP:          ap.AuthFailIPLimit,
		FailuresPerTenant:      ap.AuthFailTenantMax,
		FailureWindow:          ap.AuthFailWindow,
	}, func(err error) {
		log.Warn("login throttle counter unavailable — using process memory", zap.Error(err))
	})
}

// mergeClientChannels adds each OIDC client's source_channel to
// IDP_CLIENT_CHANNELS, so a login through a registered client resolves to a
// VERIFIED channel. An explicit IDP_CLIENT_CHANNELS entry wins.
func mergeClientChannels(cfg *config.Config, reg *oidc.Registry) {
	if cfg.IDPClientChannels == nil {
		cfg.IDPClientChannels = map[string]string{}
	}
	for id, ch := range reg.Channels() {
		if _, set := cfg.IDPClientChannels[id]; !set {
			cfg.IDPClientChannels[id] = ch
		}
	}
}

// protocolExempt replaces the envelope policy's exemption: the OAuth/OIDC
// endpoints speak RFC 6749 to browsers and OAuth clients, not the §4 envelope
// (their replay protection is the protocol's own), and setting Exempt
// replaces the built-in probe list, so the probes and the policy's own
// ExemptPaths are listed again here.
func protocolExempt(exemptPaths []string) func(*http.Request) bool {
	exact := map[string]bool{"/healthz": true, "/readyz": true, "/health": true}
	for _, p := range exemptPaths {
		exact[p] = true
	}
	return func(r *http.Request) bool {
		return exact[r.URL.Path] || oidc.IsProtocolPath(r.URL.Path)
	}
}

// skipProtocolPaths keeps the idempotency replay store off the protocol
// endpoints. A token response recorded against an Idempotency-Key would be a
// stored credential handed back to whoever repeats the key.
func skipProtocolPaths(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oidc.IsProtocolPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

// principalSessionEnder ends every session a principal holds: each live
// login session (with its refresh tokens and the session contexts resolved
// under it) and then any session context left over. It is what a disable or
// suspension calls (termination priority, IAM §17).
func principalSessionEnder(st *store.PgStore, provider *oidc.Provider, sessions *session.DurableCache) lifecycle.SessionEnder {
	return func(ctx context.Context, tenantID, principalID, reason, actor, correlationID string) (int, error) {
		ended := 0
		sids, err := st.LiveAuthSessionIDs(ctx, tenantID, principalID, time.Now().UTC())
		if err != nil {
			return 0, err
		}
		for _, sid := range sids {
			n, err := provider.EndLoginSession(ctx, tenantID, sid, reason, actor, correlationID)
			if err != nil {
				return ended, err
			}
			ended += n
		}
		n, err := sessions.EvictAllForPrincipal(ctx, principalID, tenantID, domain.InvalidationReasonAdminRevoke)
		return ended + n, err
	}
}

// newLifecycle builds the subject lifecycle service. Only its status command
// is reachable today (PUT /v1/principals/{id}/status); the invite, activation
// and password-reset commands have no HTTP surface yet.
func newLifecycle(cfg *config.Config, st *store.PgStore, hasher *credential.Hasher, end lifecycle.SessionEnder, limiter *ratelimit.Limiter, log *zap.Logger) (*lifecycle.Service, error) {
	ap := cfg.AuthPlane
	var breach lifecycle.BreachChecker
	switch ap.BreachCheck {
	case "local", "hibp":
		local, err := lifecycle.NewLocalBreach(ap.BreachListFile)
		if err != nil {
			return nil, err
		}
		breach = local
		if ap.BreachCheck == "hibp" {
			breach = &lifecycle.HIBPBreach{URL: ap.HIBPURL, Client: &http.Client{Timeout: 3 * time.Second}, Fallback: local, Log: log}
		}
	}
	var notifier lifecycle.Notifier
	if cfg.Environment == "local" || cfg.Environment == "development" {
		dev, err := lifecycle.NewDevNotifier(cfg.Environment)
		if err != nil {
			return nil, err
		}
		notifier = dev
	}
	budget := func(ctx context.Context, name, key string, max int, window time.Duration) bool {
		return limiter.Budget(ctx, name, key, max, window).Allowed
	}
	return lifecycle.New(st, hasher, lifecycle.Policy{MinLength: ap.PasswordMinLength, Breach: breach}, notifier, end,
		lifecycle.Config{
			InviteTTL: ap.InviteTTL, ResetTTL: ap.ResetTTL, PasswordHistory: ap.PasswordHistory,
			ResetApprovals: ap.ResetApprovals, PublicAppURL: ap.PublicAppURL,
			ActivationPath: ap.ActivationLinkPath, ResetPath: ap.PasswordResetPath,
			ForgotPerIPPerHr: ap.ResetRequestPerIPH,
		}, budget, log), nil
}

// statusChanger adapts lifecycle.Service to the handler's StatusChanger.
type statusChanger struct{ svc *lifecycle.Service }

func (s statusChanger) ChangeStatus(ctx context.Context, tenantID, actorID, principalID string,
	status domain.PrincipalStatus, reason, correlationID string) error {
	return s.svc.SetStatus(ctx, lifecycle.Actor{PrincipalID: actorID, TenantID: tenantID, CorrelationID: correlationID},
		principalID, status, reason)
}

var _ identityctx.StatusChanger = statusChanger{}
