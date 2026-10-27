package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Stable machine-readable error codes (spec §16: "clients must never infer
// control meaning from free-form text"). The human text stays in "error".
const (
	CodeValidationFailed       = "VALIDATION_FAILED"
	CodeUnauthenticated        = "UNAUTHENTICATED"
	CodeForbidden              = "FORBIDDEN"
	CodeSoDConflict            = "SOD_CONFLICT"
	CodeNotFound               = "NOT_FOUND"
	CodeConflict               = "CONFLICT"
	CodeStaleVersion           = "STALE_VERSION"
	CodePayeeVersionMismatch   = "PAYEE_VERSION_MISMATCH"
	CodeAuthorizationInvalid   = "AUTHORIZATION_INVALIDATED"
	CodeAuthorizationExpired   = "AUTHORIZATION_EXPIRED"
	CodeAlreadySigned          = "ALREADY_SIGNED"
	CodeDependencyUnavailable  = "DEPENDENCY_UNAVAILABLE"
	CodeIdempotencyRequired    = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyReused      = "IDEMPOTENCY_KEY_REUSED"
	CodeIdempotencyInProgress  = "IDEMPOTENCY_IN_PROGRESS"
	maxIdempotentBody          = 1 << 20
	idempotentReplayHeaderName = "Idempotent-Replay"
)

func defaultCode(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return CodeValidationFailed
	case status == http.StatusUnauthorized:
		return CodeUnauthenticated
	case status == http.StatusForbidden:
		return CodeForbidden
	case status == http.StatusNotFound:
		return CodeNotFound
	case status == http.StatusConflict:
		return CodeConflict
	case status >= 500:
		return CodeDependencyUnavailable
	default:
		return CodeValidationFailed
	}
}

func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// recorder passes the response through while keeping a copy to store.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
	r.ResponseWriter.WriteHeader(s)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// idempotent wraps a command with Idempotency-Key handling (spec §16). The
// key is scoped to (tenant, command, target authorization) and bound to the
// exact request (method, path, principal, body):
//
//   - same key, same request, already finished -> the stored status and body
//     are replayed with Idempotent-Replay: true and the command does NOT run
//     again;
//   - same key, different request -> 422 IDEMPOTENCY_KEY_REUSED;
//   - same key while the first call is still running -> 409
//     IDEMPOTENCY_IN_PROGRESS.
//
// A 5xx, 401 or 403 result is not stored (the key is released) so the caller's
// retry runs the command again. required=true makes a missing key a 400.
func (h *Handler) idempotent(name string, required bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			if required {
				writeErrorCode(w, http.StatusBadRequest, CodeIdempotencyRequired, "Idempotency-Key header is required")
				return
			}
			next(w, r)
			return
		}
		if len(key) > 200 {
			writeErrorCode(w, http.StatusBadRequest, CodeValidationFailed, "Idempotency-Key is too long")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBody+1))
		if err != nil || len(body) > maxIdempotentBody {
			writeErrorCode(w, http.StatusBadRequest, CodeValidationFailed, "request body unreadable or too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		sum := sha256.New()
		sum.Write([]byte(r.Method + "\n" + r.URL.Path + "\n" + r.Header.Get("X-Principal-Id") + "\n"))
		sum.Write(body)
		hash := hex.EncodeToString(sum.Sum(nil))
		scope := name + ":" + chi.URLParam(r, "authorizationID")

		rec, created, err := h.store.BeginIdempotent(r.Context(), scope, key, hash)
		if err != nil {
			h.log.Error("idempotency store unavailable", zap.Error(err))
			writeErrorCode(w, http.StatusServiceUnavailable, CodeDependencyUnavailable, "store unavailable")
			return
		}
		if !created {
			switch {
			case rec.RequestHash != hash:
				writeErrorCode(w, http.StatusUnprocessableEntity, CodeIdempotencyReused, "this Idempotency-Key was already used for a different request")
			case !rec.Completed:
				writeErrorCode(w, http.StatusConflict, CodeIdempotencyInProgress, "a request with this Idempotency-Key is still being processed")
			default:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(idempotentReplayHeaderName, "true")
				w.WriteHeader(rec.StatusCode)
				_, _ = w.Write(rec.Body)
			}
			return
		}

		rw := &recorder{ResponseWriter: w}
		next(rw, r)
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		// Not cached: a server failure (the retry must run again) and an
		// authentication/authorization refusal (permissions can change, and
		// a stored 403 would otherwise be replayed forever).
		if rw.status >= 500 || rw.status == http.StatusUnauthorized || rw.status == http.StatusForbidden {
			if err := h.store.ReleaseIdempotent(r.Context(), scope, key); err != nil {
				h.log.Warn("failed to release idempotency key after a failed command", zap.Error(err))
			}
			return
		}
		if err := h.store.CompleteIdempotent(r.Context(), scope, key, rw.status, rw.buf.Bytes()); err != nil {
			h.log.Warn("failed to store idempotent response", zap.Error(err))
		}
	}
}

// Options tunes behaviour that is configuration, not code.
type Options struct {
	// AuthorizationTTL is how long an unused authorization stays valid.
	AuthorizationTTL time.Duration
	// HighValueSignatures is how many distinct signers a high-value payment
	// (policy result APPROVAL_REQUIRED) needs.
	HighValueSignatures int
	// Now is the clock; time.Now unless a test overrides it.
	Now func() time.Time
}

// WithOptions overrides the defaults (24h, 2 signatures, time.Now).
func (h *Handler) WithOptions(o Options) *Handler {
	if o.AuthorizationTTL > 0 {
		h.ttl = o.AuthorizationTTL
	}
	if o.HighValueSignatures >= 1 {
		h.highValueSignatures = o.HighValueSignatures
	}
	if o.Now != nil {
		h.now = o.Now
	}
	return h
}
