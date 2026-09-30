// Package idempotency makes Idempotency-Key mean something.
//
// The ORG shared contract (§3) requires that "replays return the original
// material result and never duplicate versions", and the canonical envelope
// makes the header mandatory on every material write. The header was enforced
// and never honoured: a client that retried SuspendTenant or a legal-name
// change after a lost response issued the command a second time (verified live
// on 28 Sep 2026 — one key, two lifecycle history rows). expected_version
// refused some of those retries, but only when the caller sent one.
//
// Ported from identity-context-svc/internal/idempotency, which fixed the same
// defect there; the behaviour and its reasons are the same.
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

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// Store is the persistence this middleware needs. Satisfied by *store.PgStore.
type Store interface {
	ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*store.IdempotencyRecord, error)
	CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error
}

const (
	headerKey = "Idempotency-Key"

	// headerReplayed tells a caller its answer came from the record rather
	// than from doing the work again — the question replay protection exists
	// to answer.
	headerReplayed = "X-Idempotent-Replay"
)

// Middleware answers a repeated command from its first response.
//
// Applies only to material writes carrying a key and a verified tenant. A
// write without a key is already refused upstream by the envelope contract.
func Middleware(s Store, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := domain.TenantFromContext(r.Context())

			if !materialWrite(r.Method) || key == "" || tenantID == "" {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeErr(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "invalid request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The concrete path, not the route pattern: the same key against
			// two different entities is two requests, not a mismatch.
			endpoint := r.Method + " " + r.URL.Path
			fingerprint := fingerprintOf(domain.PrincipalFromContext(r.Context()), string(body))

			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				writeErr(w, r, http.StatusConflict, "IDEMPOTENCY_MISMATCH",
					"idempotency key reuse: this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				// Fail closed: executing a command we cannot deduplicate is
				// exactly the behaviour being fixed.
				log.Error("idempotency claim failed", zap.Error(err), zap.String("endpoint", endpoint))
				writeErr(w, r, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE",
					"upstream service unavailable — could not establish replay protection for this command")
				return
			}

			if rec != nil {
				if rec.ResponseStatus == 0 {
					writeErr(w, r, http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT",
						"an identical command is currently in flight — retry shortly")
					return
				}
				w.Header().Set(headerReplayed, "true")
				replay(w, rec)
				return
			}

			rw := &recorder{ResponseWriter: w, status: http.StatusOK}

			// A panicking handler never returns here; release the claim so
			// retries are not told "in flight" until the lease expires, then
			// re-panic so Recoverer still answers 500.
			defer func() {
				if p := recover(); p != nil {
					if err := s.ReleaseIdempotencyKey(context.WithoutCancel(r.Context()), tenantID, endpoint, key); err != nil {
						log.Error("idempotency release after panic failed", zap.Error(err), zap.String("endpoint", endpoint))
					}
					panic(p)
				}
			}()
			next.ServeHTTP(rw, r)

			// Detached: the response has been written, and a client that
			// disconnects must not stop the outcome being recorded.
			ctx := context.WithoutCancel(r.Context())

			// 5xx is not a terminal answer — a retry must genuinely retry.
			if rw.status >= 500 {
				if err := s.ReleaseIdempotencyKey(ctx, tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			if err := s.CompleteIdempotencyKey(ctx, tenantID, endpoint, key, rw.status, rw.body.Bytes()); err != nil {
				// The command DID run; failing the response now would tell the
				// caller nothing happened when something did.
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// fingerprintOf hashes the acting principal and the request body. The
// principal is part of the request: a replay is answered before the handler's
// authorization check, so keyed on the body alone a second principal in the
// same tenant presenting the first one's key and body would be handed the
// first one's stored response without ever being authorized.
func fingerprintOf(principalID, body string) string {
	sum := sha256.Sum256([]byte(principalID + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

// materialWrite mirrors the envelope contract's own definition.
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
	if rec.ResponseStatus == http.StatusNoContent || bytes.Equal(rec.ResponseBody, []byte("null")) {
		w.WriteHeader(rec.ResponseStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.ResponseStatus)
	_, _ = w.Write(rec.ResponseBody)
}

// writeErr uses this service's error body shape (error, error_code,
// correlation_id), not identity-context-svc's.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":          msg,
		"error_code":     code,
		"correlation_id": r.Header.Get("X-Correlation-ID"),
	})
}
