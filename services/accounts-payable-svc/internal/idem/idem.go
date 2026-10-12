// Package idem implements spec section 16 command idempotency for the AP-05 and
// AP-06 HTTP handlers.
//
// A repeat with the same Idempotency-Key and the same request returns the stored
// status and body with `Idempotent-Replay: true`; the same key with a different
// request is refused (422 IDEMPOTENCY_KEY_REUSED); a repeat while the first call
// is still running is refused (409). Only 2xx outcomes are stored: a failed
// command releases its claim so the caller can retry it.
package idem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// Store is the persistence the middleware needs.
type Store interface {
	IdemClaim(ctx context.Context, tenantID, key, operation, requestHash string, staleAfter time.Duration) (*domain.IdemRecord, bool, error)
	IdemComplete(ctx context.Context, tenantID, key string, status int, response []byte) error
	IdemRelease(ctx context.Context, tenantID, key string) error
}

const (
	// StaleAfter is how long an unfinished claim blocks retries.
	StaleAfter  = 60 * time.Second
	maxBodySize = 1 << 20
)

type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(s int) { c.status = s }
func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}

func writeErr(w http.ResponseWriter, status int, err, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err, "code": code})
}

// Wrap guards a command handler. required makes the header mandatory (400
// IDEMPOTENCY_KEY_REQUIRED); otherwise it is honoured when supplied.
func Wrap(st Store, tenantOf func(context.Context) string, operation string, required bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			if required {
				writeErr(w, http.StatusBadRequest, "idempotency_key_required", domain.CodeIdempotencyRequired)
				return
			}
			next(w, r)
			return
		}
		tenantID := tenantOf(r.Context())
		if tenantID == "" {
			next(w, r) // the handler refuses a request with no tenant scope
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", domain.CodeValidationFailed)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])

		rec, claimed, err := st.IdemClaim(r.Context(), tenantID, key, operation, hash, StaleAfter)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "store_unavailable", domain.CodeDependencyUnavailable)
			return
		}
		if !claimed {
			switch {
			case rec.Operation != operation || rec.RequestHash != hash:
				writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reused", domain.CodeIdempotencyReused)
			case rec.StatusCode != nil:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(*rec.StatusCode)
				_, _ = w.Write(rec.Response)
			default:
				writeErr(w, http.StatusConflict, "idempotency_request_in_progress", domain.CodeIdempotencyReused)
			}
			return
		}

		c := &capture{ResponseWriter: w}
		next(c, r)
		status := c.status
		if status == 0 {
			status = http.StatusOK
		}
		out := c.buf.Bytes()
		if status >= 200 && status < 300 && json.Valid(out) {
			if err := st.IdemComplete(r.Context(), tenantID, key, status, out); err != nil {
				_ = st.IdemRelease(r.Context(), tenantID, key)
			}
		} else {
			_ = st.IdemRelease(r.Context(), tenantID, key)
		}
		for k, v := range c.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
}
