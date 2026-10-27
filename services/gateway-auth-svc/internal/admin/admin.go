// Package admin is the read-only query surface for the gateway's own decisions
// (ZS-SVC-M-001 INT-01: GetGatewayDecision, GetClientUsage, GetRouteVersionStatus,
// ListDeniedRequests, GetRateLimitState).
//
// It is served on a second port of this same service, only when an address AND
// a token file are configured, and every route needs the bearer token. The token
// is read from a file, never from the environment. Nothing here can change
// state: route and policy publication stay governed configuration.
package admin

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/gateway-auth-svc/internal/admission"
	"zoiko.io/gateway-auth-svc/internal/problem"
)

// MinTokenLength refuses a token too short to be a credential.
const MinTokenLength = 24

// LoadToken reads the admin bearer token from path.
func LoadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) < MinTokenLength {
		return "", errors.New("admin token file holds a token shorter than 24 characters")
	}
	return tok, nil
}

// Source is what the query surface reads.
type Source struct {
	Recorder      *admission.Recorder
	IPLimiter     *admission.Limiter
	TenantLimiter *admission.Limiter
	Policy        admission.Policy
}

// New builds the query handler. token must be at least MinTokenLength long.
func New(src Source, token string) (http.Handler, error) {
	if len(token) < MinTokenLength {
		return nil, errors.New("admin token is too short")
	}
	if src.Recorder == nil {
		return nil, errors.New("admin needs a decision recorder")
	}
	a := &api{src: src, token: []byte(token)}
	r := chi.NewRouter()
	r.Use(a.authenticate)
	r.Route("/internal/v1/gateway", func(r chi.Router) {
		r.Get("/decisions/{id}", a.getDecision)
		r.Get("/denied", a.listDenied)
		r.Get("/usage/{tenantID}", a.getUsage)
		r.Get("/rate-limit/{scope}/{key}", a.getRateLimit)
		r.Get("/routes/versions", a.getVersions)
	})
	return r, nil
}

type api struct {
	src   Source
	token []byte
}

// authenticate compares in constant time and answers 401 for anything else. The
// failure does not say whether the token was missing or wrong.
func (a *api) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const p = "bearer "
		if len(h) < len(p) || !strings.EqualFold(h[:len(p)], p) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(p):])), a.token) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem.Write(w, problem.Spec{Code: problem.CodeAuthDenied, Status: http.StatusUnauthorized,
				Detail: "a valid administrative credential is required", Instance: r.URL.Path})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *api) getDecision(w http.ResponseWriter, r *http.Request) {
	d, ok := a.src.Recorder.Get(chi.URLParam(r, "id"))
	if !ok {
		problem.WriteStatus(w, http.StatusNotFound,
			"no such decision in the recent window; older decisions are in SIEM and the logs")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *api) listDenied(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			problem.WriteStatus(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	list := a.src.Recorder.ListDenied(r.URL.Query().Get("tenant_id"), limit)
	if list == nil {
		list = []admission.Decision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": list, "scope": "recent window of this replica"})
}

func (a *api) getUsage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.src.Recorder.UsageOf(chi.URLParam(r, "tenantID"))
	if !ok {
		problem.WriteStatus(w, http.StatusNotFound, "no traffic recorded for that tenant on this replica")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": u, "scope": "this replica since start"})
}

func (a *api) getRateLimit(w http.ResponseWriter, r *http.Request) {
	var l *admission.Limiter
	switch chi.URLParam(r, "scope") {
	case "client":
		l = a.src.IPLimiter
	case "tenant":
		l = a.src.TenantLimiter
	default:
		problem.WriteStatus(w, http.StatusBadRequest, "scope must be client or tenant")
		return
	}
	if !l.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	remaining, burst, rate := l.State(chi.URLParam(r, "key"))
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "remaining_tokens": remaining, "burst": burst, "tokens_per_second": rate,
		"scope": "this replica",
	})
}

func (a *api) getVersions(w http.ResponseWriter, _ *http.Request) {
	versions := a.src.Policy.SupportedVersions
	if versions == nil {
		versions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supported_versions":       versions,
		"version_admission":        len(versions) > 0,
		"max_body_bytes":           a.src.Policy.MaxBodyBytes,
		"compressed_bodies":        len(a.src.Policy.AllowedEncodings) > 0,
		"max_compressed_body_size": a.src.Policy.MaxCompressedBytes,
	})
}
