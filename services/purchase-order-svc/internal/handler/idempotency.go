package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

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

// idempotent wraps a command with Idempotency-Key replay (spec §16). The key is
// scoped to (tenant, command, target order) and bound to the exact request
// (method, path, principal, body):
//
//   - same key, same request, already finished -> the stored status and body are
//     replayed with Idempotent-Replay: true and the command does NOT run again;
//   - same key, different request -> 422 IDEMPOTENCY_KEY_REUSED;
//   - same key while the first call is still running -> 409 IDEMPOTENCY_IN_PROGRESS.
//
// The envelope middleware already makes the header mandatory on material writes;
// without a key this wrapper simply runs the command. A 5xx, 401 or 403 result is
// not stored (the key is released), so a retry runs the command again: a server
// failure must be retried, and permissions can change.
func (h *Handler) idempotent(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > 200 {
			writeError(w, http.StatusBadRequest, "invalid_field", "Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentRequestBody+1))
		if err != nil || len(body) > maxIdempotentRequestBody {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body unreadable or too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		sum := sha256.New()
		sum.Write([]byte(r.Method + "\n" + r.URL.Path + "\n" + r.Header.Get("X-Principal-Id") + "\n"))
		sum.Write(body)
		hash := hex.EncodeToString(sum.Sum(nil))
		scope := name + ":" + chi.URLParam(r, "purchase_order_id")

		rec, created, err := h.store.BeginIdempotent(r.Context(), scope, key, hash)
		if err != nil {
			h.log.Error("idempotency store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			return
		}
		if !created {
			switch {
			case rec.RequestHash != hash:
				writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was already used for a different request")
			case !rec.Completed:
				writeError(w, http.StatusConflict, "idempotency_in_progress", "a request with this Idempotency-Key is still being processed")
			default:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
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
