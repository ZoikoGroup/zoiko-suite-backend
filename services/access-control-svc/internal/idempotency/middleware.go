// Package idempotency makes Idempotency-Key mean something on this service.
//
// The envelope contract has required the header on every write here since the
// service was built, and nothing ever read it. Writes were idempotent only on
// the body's correlation_id, so:
//
//   - a retry that kept the header but regenerated correlation_id was a second
//     write (a second role, a second assignment request), and
//   - the same key reused for a DIFFERENT request was accepted silently.
//
// This is identity-context-svc's middleware (its internal/idempotency), on this
// service's store (migration 000012): the key is bound to a fingerprint of the
// caller and the body; a reuse for a different request is 409
// IDEMPOTENCY_MISMATCH; a genuine retry is answered from the first response
// without the handler running again.
//
// One difference, deliberate: a replayed 201 is answered 200. This service's
// contract (openapi.yaml, the console client) is "201 is a new resource, 200
// is a replay", and that must hold whichever of the two keys detected it.
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

	"zoiko.io/access-control-svc/internal/store"
)

// Store is the persistence this middleware needs. Satisfied by *store.PgStore.
type Store interface {
	ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*store.IdempotencyRecord, error)
	CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error
}

const (
	headerKey       = "Idempotency-Key"
	headerTenant    = "X-Tenant-Id"
	headerPrincipal = "X-Principal-Id"
	// HeaderReplayed tells a caller the answer came from the record.
	HeaderReplayed = "X-Idempotent-Replay"
)

// Middleware answers a repeated command from its first response.
func Middleware(s Store, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)
			if !materialWrite(r.Method) || key == "" || tenantID == "" {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request", "could not read request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The concrete path (with the query: a DELETE carries its context
			// there), not chi's route pattern, so one key is scoped to one
			// resource.
			endpoint := r.Method + " " + r.URL.RequestURI()
			fingerprint := fingerprintOf(r.Header.Get(headerPrincipal), string(body))

			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				writeErr(w, http.StatusConflict, "IDEMPOTENCY_MISMATCH",
					"this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				// Fail closed: a command we cannot deduplicate is the defect.
				log.Error("idempotency claim failed", zap.Error(err), zap.String("endpoint", endpoint))
				writeErr(w, http.StatusServiceUnavailable, "store_unavailable",
					"could not establish replay protection for this command")
				return
			}
			if rec != nil {
				if rec.ResponseStatus == 0 {
					writeErr(w, http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT",
						"an identical command is currently in flight")
					return
				}
				w.Header().Set(HeaderReplayed, "true")
				replay(w, rec)
				return
			}

			rw := &recorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				if p := recover(); p != nil {
					if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
						log.Error("idempotency release after panic failed", zap.Error(err), zap.String("endpoint", endpoint))
					}
					panic(p)
				}
			}()
			next.ServeHTTP(rw, r)

			// A 5xx is not a terminal answer: release so a retry can retry.
			if rw.status >= 500 {
				if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			if err := s.CompleteIdempotencyKey(r.Context(), tenantID, endpoint, key, rw.status, rw.body.Bytes()); err != nil {
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// fingerprintOf hashes the acting principal and the body. The principal is in
// it so a second principal presenting the first one's key and body gets a
// mismatch instead of the first one's stored response, unauthorized.
func fingerprintOf(principalID, body string) string {
	sum := sha256.Sum256([]byte(principalID + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func materialWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
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

func replay(w http.ResponseWriter, rec *store.IdempotencyRecord) {
	status := rec.ResponseStatus
	// 201/202 mean "this call created it". A replay created nothing.
	if status == http.StatusCreated || status == http.StatusAccepted {
		status = http.StatusOK
	}
	if bytes.Equal(rec.ResponseBody, []byte("null")) {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(rec.ResponseBody)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}
