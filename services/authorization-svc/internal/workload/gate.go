// Package workload decides which calling service may use this service's
// evaluation endpoints (Governance audit register A-02: "only internal PEPs
// call the evaluation endpoint").
//
// The caller's identity is the client certificate the mTLS listener verified
// during the handshake (cmd/server/main.go, internal/mtls.ServerTLSConfig:
// RequireAndVerifyClientCert against mtls-management-svc's CA). Its Subject
// CN is the service_name the certificate was issued for. That identity — not
// the X-Workload-Id header — is what this gate checks against the PEP
// allow-list, and it overwrites X-Workload-Id so everything downstream (the
// envelope's actor, logs) sees the verified name rather than a claimed one.
//
// What the gate does NOT do: it does not authenticate the principal a PEP asks
// about. A registered PEP vouches for the X-Principal-Id it forwards, exactly
// as it vouches for it today; binding that principal to a signed gateway
// assertion is register item A-01 and waits on gateway-auth-svc.
package workload

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Mode says what happens to an evaluation request that arrives without a
// verified client certificate (i.e. on the plaintext port).
type Mode string

const (
	// ModePermit admits it: the caller has not migrated to mTLS yet. Callers
	// that DO present a certificate are still checked against the allow-list.
	ModePermit Mode = "permit"
	// ModeRequire refuses it 401. A certificate is the only way in.
	ModeRequire Mode = "require"
)

// HeaderWorkloadID is the envelope header the gate overwrites with the
// verified certificate identity.
const HeaderWorkloadID = "X-Workload-Id"

// evaluationPaths are the decision endpoints a PEP calls. Every /internal/
// route is east-west by definition and is covered by prefix.
var evaluationPaths = map[string]bool{
	"/v1/authorize":                 true,
	"/v1/sod/evaluate":              true,
	"/v1/sod/validate":              true,
	"/v1/entity-scope/validate":     true,
	"/v1/delegated-access/evaluate": true,
}

// IsEvaluationPath reports whether path is gated.
func IsEvaluationPath(path string) bool {
	return evaluationPaths[path] || strings.HasPrefix(path, "/internal/")
}

// Gate is the workload check. Build it with New.
type Gate struct {
	mode  Mode
	allow map[string]bool
	log   *zap.Logger
}

// New builds a Gate. allowList is the set of registered PEP service names;
// empty means "any certificate issued by the platform CA" and is refused by
// config validation in require mode.
func New(mode Mode, allowList []string, log *zap.Logger) *Gate {
	allow := make(map[string]bool, len(allowList))
	for _, s := range allowList {
		if s = strings.TrimSpace(s); s != "" {
			allow[s] = true
		}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Gate{mode: mode, allow: allow, log: log}
}

// Mode returns the configured mode.
func (g *Gate) Mode() Mode { return g.mode }

// Registered returns the allow-list, sorted, for the startup log.
func (g *Gate) Registered() []string {
	out := make([]string, 0, len(g.allow))
	for s := range g.allow {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Middleware applies the gate to evaluation paths and passes every other
// route through untouched.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsEvaluationPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		id := VerifiedIdentity(r)
		if id == "" {
			if g.mode == ModeRequire {
				g.log.Warn("evaluation call refused: no verified client certificate",
					zap.String("path", r.URL.Path), zap.String("remote_addr", r.RemoteAddr),
					zap.String("claimed_workload", r.Header.Get(HeaderWorkloadID)))
				refuse(w, http.StatusUnauthorized, "client_certificate_required", "CONTEXT_UNRESOLVED",
					"evaluation endpoints require a client certificate issued by mtls-management-svc; call the mTLS listener")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if len(g.allow) > 0 && !g.allow[id] {
			g.log.Warn("evaluation call refused: workload is not a registered PEP",
				zap.String("workload", id), zap.String("path", r.URL.Path))
			refuse(w, http.StatusForbidden, "workload_not_registered", "AUTHORIZATION_DENIED",
				"the calling service is not a registered policy enforcement point")
			return
		}

		if claimed := strings.TrimSpace(r.Header.Get(HeaderWorkloadID)); claimed != "" && claimed != id {
			g.log.Warn("evaluation call refused: X-Workload-Id does not match the client certificate",
				zap.String("workload", id), zap.String("claimed", claimed), zap.String("path", r.URL.Path))
			refuse(w, http.StatusForbidden, "workload_identity_mismatch", "AUTHORIZATION_DENIED",
				"X-Workload-Id does not name the service the client certificate was issued to")
			return
		}

		r.Header.Set(HeaderWorkloadID, id)
		next.ServeHTTP(w, r)
	})
}

// VerifiedIdentity returns the Subject CN of a client certificate the TLS
// handshake verified against the configured CA, or "" when there is none.
//
// VerifiedChains, not PeerCertificates: PeerCertificates is whatever the
// client sent, VerifiedChains is only populated when it chained to ClientCAs.
func VerifiedIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	return strings.TrimSpace(r.TLS.VerifiedChains[0][0].Subject.CommonName)
}

func refuse(w http.ResponseWriter, status int, code, class, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":       code,
		"error_class": class,
		"message":     msg,
	})
}
