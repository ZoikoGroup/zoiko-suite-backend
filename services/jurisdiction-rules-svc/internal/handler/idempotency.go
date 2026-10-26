package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/problem"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// IdempotencyStore holds Idempotency-Key claims and their responses.
type IdempotencyStore interface {
	ClaimIdempotencyKey(ctx context.Context, principalID, key, method, path, requestHash string) (*store.IdempotencyRecord, error)
	CompleteIdempotencyKey(ctx context.Context, principalID, key, method, path string, status int, contentType string, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, principalID, key, method, path string) error
}

// maxReplayBody bounds what is kept for replay. Admin responses are a record
// or two; anything larger is answered but not stored.
const maxReplayBody = 1 << 20

// IdempotencyMiddleware gives Idempotency-Key its meaning on admin writes
// (POST/PUT under /v1/admin/): the first request with a key runs and its
// response is kept for 24 hours; a retry with the same key and the same
// request gets that response back (Idempotent-Replayed: true) without running
// again; the same key with a different request is 409
// idempotency_key_reused, and while the first is still running, 409
// request_in_progress. A 5xx is not kept, so the client can retry it.
//
// The envelope middleware already demanded the header on every write and
// nothing read it (audit X4). A key is scoped to the principal and the exact
// method and path, so one key reused across different endpoints — as the
// Postman collection does — is still distinct requests.
func IdempotencyMiddleware(st IdempotencyStore, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			principal := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
			if principal == "" {
				principal = strings.TrimSpace(r.Header.Get("X-Workload-Id"))
			}
			if (r.Method != http.MethodPost && r.Method != http.MethodPut) ||
				!strings.HasPrefix(r.URL.Path, "/v1/admin/") || key == "" || principal == "" {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					writeError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body exceeds 256 KiB")
					return
				}
				writeError(w, r, http.StatusBadRequest, "invalid_json", "request body could not be read")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			h := sha256.New()
			for _, part := range []string{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Approval-Reference")} {
				h.Write([]byte(part))
				h.Write([]byte{0})
			}
			h.Write(body)
			reqHash := "sha256:" + hex.EncodeToString(h.Sum(nil))

			ctx := r.Context()
			existing, err := st.ClaimIdempotencyKey(ctx, principal, key, r.Method, r.URL.Path, reqHash)
			if err != nil {
				problem.Write(w, problem.New(http.StatusServiceUnavailable, "store_unavailable", ""))
				return
			}
			if existing != nil {
				switch {
				case existing.RequestHash != reqHash:
					writeError(w, r, http.StatusConflict, "idempotency_key_reused",
						"this Idempotency-Key was already used for a different request")
				case existing.Status == 0:
					writeError(w, r, http.StatusConflict, "request_in_progress",
						"a request with this Idempotency-Key is still being processed")
				default:
					if existing.ContentType != "" {
						w.Header().Set("Content-Type", existing.ContentType)
					}
					w.Header().Set("Idempotent-Replayed", "true")
					w.WriteHeader(existing.Status)
					_, _ = w.Write(existing.Body)
				}
				return
			}

			rec := &capturingWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				// Detached: a client that hung up must not leave the key held.
				bg := context.WithoutCancel(ctx)
				if rec.status >= 500 || rec.overflow {
					if err := st.ReleaseIdempotencyKey(bg, principal, key, r.Method, r.URL.Path); err != nil {
						log.Error("idempotency key release failed", zap.Error(err))
					}
					return
				}
				if err := st.CompleteIdempotencyKey(bg, principal, key, r.Method, r.URL.Path, rec.status, rec.Header().Get("Content-Type"), rec.buf.Bytes()); err != nil {
					log.Error("idempotency key completion failed", zap.Error(err))
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// capturingWriter passes the response through and keeps a copy for replay.
type capturingWriter struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	overflow bool
	wrote    bool
}

func (c *capturingWriter) WriteHeader(status int) {
	if !c.wrote {
		c.status, c.wrote = status, true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.wrote = true
	if !c.overflow {
		if c.buf.Len()+len(b) > maxReplayBody {
			c.overflow = true
			c.buf.Reset()
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

