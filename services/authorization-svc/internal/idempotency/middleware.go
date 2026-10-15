// Package idempotency makes Idempotency-Key mean something on this service.
//
// The envelope has required the header on every admin write since enforcement,
// and nothing read it. CreateRoleAssignment minted a new id per call and
// CreateSoDRule / CreateABACRule were plain inserts, so a retried request — a
// client timeout, a proxy retry — created a second grant or a second rule, and
// the same key reused for a DIFFERENT request was accepted silently.
//
// This is identity-context-svc's and access-control-svc's middleware on this
// service's store (migration 000021): the key is bound to a fingerprint of the
// caller and the body; a reuse for a different request is 409; a genuine retry
// is answered from the first response without the handler running again.
//
// Only material writes are covered, as the caller's isWrite decides
// (handler.MaterialWrite): /v1/authorize and the evaluate routes answer a
// question, and two identical questions must produce two decision records.
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

	"zoiko.io/authorization-svc/internal/store"
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
func Middleware(s Store, isWrite func(*http.Request) bool, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)
			if !isWrite(r) || key == "" || tenantID == "" {
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

			// The concrete path, not the route pattern, so one key is scoped
			// to one resource.
			endpoint := r.Method + " " + r.URL.RequestURI()
			fingerprint := fingerprintOf(r.Header.Get(headerPrincipal), string(body))

			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				writeErr(w, http.StatusConflict, "idempotency_mismatch",
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
					writeErr(w, http.StatusConflict, "idempotency_in_flight",
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
// mismatch instead of the first one's stored response.
func fingerprintOf(principalID, body string) string {
	sum := sha256.Sum256([]byte(principalID + "\x00" + body))
	return hex.EncodeToString(sum[:])
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

// replay answers from the record. A 201 is answered 200: this service's
// contract is "201 created / 200 idempotent replay" (CreateRole), and a replay
// created nothing, whichever of the two mechanisms detected it.
func replay(w http.ResponseWriter, rec *store.IdempotencyRecord) {
	status := rec.ResponseStatus
	if status == http.StatusCreated {
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
	body := map[string]string{"error": code, "message": msg}
	// Governance Control Plane §16 stable class, beside the code.
	if code == "idempotency_mismatch" || code == "idempotency_in_flight" {
		body["error_class"] = "IDEMPOTENCY_MISMATCH"
	}
	_ = json.NewEncoder(w).Encode(body)
}
