// Package idempotency makes Idempotency-Key mean something.
//
// ZS-SVC-A-001 §4's Engineering Interaction Wireframe requires "Idempotency-Key
// + expected_version where stateful" on every COMMAND, and the canonical
// envelope contract makes the header mandatory on every material write. Both
// were enforced. Neither was ever HONOURED: there was no dedupe store, no
// replay path, and domain.ErrCodeIdempotencyMismatch was declared and returned
// by no code in the service.
//
// The consequence was not theoretical. POST /v1/context/support mints a
// break-glass elevation — privileged, time-limited, independently approved. A
// client that retried after a lost response minted a SECOND one, and the only
// trace was two grants where the operator believed there was one.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/store"
)

// Store is the persistence this middleware needs. Satisfied by *store.PgStore.
type Store interface {
	ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*store.IdempotencyRecord, error)
	CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error
}

const (
	headerKey    = "Idempotency-Key"
	headerTenant = "X-Tenant-Id"

	// headerReplayed tells a caller its answer came from the record rather
	// than from doing the work again. Without it a retry is indistinguishable
	// from a first attempt, and "did my command actually run twice?" is the
	// question replay protection exists to answer.
	headerReplayed = "X-Idempotent-Replay"
)

// exemptPaths never participate in replay protection.
//
// Both mint bearer credentials. Recording their responses would put a signed
// identity envelope — a working credential for a principal — into a Postgres
// table with a retention period, to be handed back to anyone who later presents
// the same key. The replay protection would itself become the credential leak.
//
// /v1/context/resolve is additionally a QUERY in §4's own wireframe, however
// the envelope middleware classifies its POST; deduplicating a query is
// meaningless.
var exemptPaths = map[string]bool{
	"/v1/authenticate":    true,
	"/v1/context/resolve": true,
}

// Middleware answers a repeated command from its first response.
//
// Applies only to material writes that carry a key. A write without one is
// already refused upstream by the envelope contract, so this does not need to
// enforce presence — and must not, or it would answer for endpoints the
// envelope deliberately exempts.
func Middleware(s Store, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)

			if !materialWrite(r.Method) || key == "" || tenantID == "" || exemptPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			// The body has to be read to fingerprint it, and the handler still
			// needs it afterwards.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request", "could not read request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The concrete path, not chi's route pattern. Under the pattern,
			// DELETE /v1/context/support/{id} collapses two different support
			// contexts onto one endpoint, so the same key used against each
			// would be reported as a mismatch rather than deduplicated per
			// resource. The concrete path is also available before chi has
			// matched a route, which the pattern is not.
			endpoint := r.Method + " " + r.URL.Path
			fingerprint := fingerprintOf(string(body))

			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				// The stable code §4 named and this service could not produce.
				writeErr(w, http.StatusConflict, "IDEMPOTENCY_MISMATCH",
					"this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				// Fail closed. Executing a command we cannot deduplicate is
				// exactly the behaviour being fixed.
				log.Error("idempotency claim failed", zap.Error(err), zap.String("endpoint", endpoint))
				writeErr(w, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE",
					"could not establish replay protection for this command")
				return
			}

			if rec != nil {
				if rec.ResponseStatus == 0 {
					// Claimed by a concurrent request that has not finished.
					// 409 rather than replaying an empty record: the first
					// attempt may still succeed, and the caller should retry.
					writeErr(w, http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT",
						"an identical command is currently in flight")
					return
				}
				w.Header().Set(headerReplayed, "true")
				replay(w, rec)
				return
			}

			rw := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			// 5xx is not a terminal answer. Release the claim so a retry can
			// genuinely retry rather than being handed the failure forever.
			if rw.status >= 500 {
				if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			if err := s.CompleteIdempotencyKey(r.Context(), tenantID, endpoint, key, rw.status, rw.body.Bytes()); err != nil {
				// The command DID run. Logging is all that is left — failing
				// the response now would tell the caller nothing happened when
				// something did, which is worse than an unrecorded key.
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// fingerprintOf hashes a request body. A hash, not the body: a support-context
// request carries a justification and a ticket reference, and the replay table
// must not become a second copy of them.
func fingerprintOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// materialWrite mirrors the envelope contract's own definition, including PUT
// and DELETE: both are idempotent at the HTTP level and neither is at the
// governance level.
func materialWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// recorder captures the handler's answer so it can be stored and replayed.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func replay(w http.ResponseWriter, rec *store.IdempotencyRecord) {
	// A 204 was stored as JSON null; it has no body to return.
	if rec.ResponseStatus == http.StatusNoContent || bytes.Equal(rec.ResponseBody, []byte("null")) {
		w.WriteHeader(rec.ResponseStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.ResponseStatus)
	_, _ = w.Write(rec.ResponseBody)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code": code,
		"error":      msg,
	})
}
