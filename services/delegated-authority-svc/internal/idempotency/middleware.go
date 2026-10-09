// Package idempotency answers a repeated command from its first response
// (cross-service finding 2: the envelope demanded Idempotency-Key on every
// material write and nothing here read it, so a retried revoke or activation
// ran twice). Same design as identity-context-svc and notification-svc.
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

	"zoiko.io/delegated-authority-svc/internal/store"
)

// Store is the persistence the middleware needs; satisfied by *store.PgStore.
type Store interface {
	ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*store.IdempotencyRecord, error)
	CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error
}

const (
	headerKey       = "Idempotency-Key"
	headerTenant    = "X-Tenant-Id"
	headerPrincipal = "X-Principal-Id"
	headerReplayed  = "X-Idempotent-Replay"
)

// Middleware deduplicates material writes that carry a key. A write without
// one is refused upstream by the envelope contract, so presence is not
// enforced here.
func Middleware(s Store, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)
			if !materialWrite(r.Method) || key == "" || tenantID == "" {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request", "could not read request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The concrete path and query: the same key against two different
			// delegations' /revoke, or two expected_versions, is two commands.
			endpoint := r.Method + " " + r.URL.RequestURI()
			fingerprint := fingerprintOf(r.Header.Get(headerPrincipal), string(body))
			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				writeErr(w, http.StatusConflict, "idempotency_mismatch", "this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				// Fail closed: running a command that cannot be deduplicated is
				// the behaviour being fixed.
				log.Error("idempotency claim failed", zap.Error(err), zap.String("endpoint", endpoint))
				writeErr(w, http.StatusServiceUnavailable, "store_unavailable", "could not establish replay protection for this command")
				return
			}
			if rec != nil {
				if rec.ResponseStatus == 0 {
					writeErr(w, http.StatusConflict, "idempotency_in_flight", "an identical command is currently in flight")
					return
				}
				w.Header().Set(headerReplayed, "true")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rec.ResponseStatus)
				if !bytes.Equal(rec.ResponseBody, []byte("null")) {
					_, _ = w.Write(rec.ResponseBody)
				}
				return
			}

			rw := &recorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				if p := recover(); p != nil {
					_ = s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key)
					panic(p)
				}
			}()
			next.ServeHTTP(rw, r)

			// 5xx, 409 and 428/429 describe states that change (an authority
			// unavailable, a version moved on, an overlap since revoked): a retry
			// after the cause is fixed must genuinely retry.
			if rw.status >= 500 || rw.status == http.StatusConflict || rw.status == http.StatusPreconditionRequired || rw.status == http.StatusTooManyRequests {
				if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			if err := s.CompleteIdempotencyKey(r.Context(), tenantID, endpoint, key, rw.status, rw.body.Bytes()); err != nil {
				// The command DID run; failing the response now would say it did not.
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// fingerprintOf binds the key to the acting principal and the body, so a
// second principal presenting the first one's key gets a mismatch rather than
// the first one's stored answer, handed over before any authorization ran.
func fingerprintOf(principalID, body string) string {
	sum := sha256.Sum256([]byte(principalID + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func materialWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

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

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error_code": code, "error_message": msg})
}
