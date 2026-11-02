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

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/middleware"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// IdempotencyStore is the persistence the Idempotency-Key middleware needs.
type IdempotencyStore interface {
	ReserveIdempotencyKey(ctx context.Context, tenantID, key, fingerprint string) (store.IdempotencyOutcome, *store.StoredResponse, error)
	CompleteIdempotencyKey(ctx context.Context, tenantID, key string, status int, body []byte) error
	ReleaseIdempotencyKey(ctx context.Context, tenantID, key string) error
}

// maxIdempotencyKeyLength matches idempotency_records.idempotency_key.
const maxIdempotencyKeyLength = 255

// idempotencyMiddleware implements Idempotency-Key replay protection (INV-08)
// for every write (POST/PUT/PATCH/DELETE):
//
//  1. fingerprint the request: sha256(method, path, body);
//  2. reserve (tenant, key) in idempotency_records;
//     - same key + same request already succeeded -> replay the stored
//     response with header Idempotent-Replayed: true (the command is NOT
//     executed again);
//     - same key + different request -> 422 IDEMPOTENCY_KEY_REUSED;
//     - same key still executing -> 409 IDEMPOTENCY_IN_FLIGHT;
//  3. run the handler, capturing its response;
//  4. 2xx -> store the response for replay; otherwise release the key so
//     the client can fix the request and retry with the same key.
//
// The envelope middleware has already refused a write without a key; when
// enforcement is in observe mode the request passes through undeduplicated.
func (h *Handler) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWriteMethod(r.Method) || h.idempotency == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		tenantID := middleware.GetTenantID(r.Context())
		if key == "" || tenantID == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > maxIdempotencyKeyLength {
			validationProblem(w, r, "Idempotency-Key must be at most 255 characters")
			return
		}

		// Step 1: read the body once (bounded) and restore it for the handler.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, CodeMalformedJSON, "could not read request body")
			return
		}
		if len(body) > maxRequestBodyBytes {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds 1 MiB")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fingerprint := requestFingerprint(r.Method, r.URL.Path, body)

		// Step 2: reserve the key.
		outcome, stored, err := h.idempotency.ReserveIdempotencyKey(r.Context(), tenantID, key, fingerprint)
		if err != nil {
			if errors.Is(err, domain.ErrIdempotencyMismatch) || errors.Is(err, domain.ErrIdempotencyInFlight) {
				h.writeDomainError(w, r, err)
				return
			}
			h.logger.Error("idempotency reservation failed", zap.Error(err))
			writeProblem(w, r, http.StatusInternalServerError, CodeInternalError, "an unexpected error occurred")
			return
		}
		if outcome == store.IdempotencyReplay {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(stored.Status)
			_, _ = w.Write(stored.Body)
			return
		}

		// Step 3: run the handler and capture its response.
		recorder := &capturingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Step 4: keep successes for replay, release everything else. A
		// detached context is used so a client disconnect cannot leave the
		// key stuck IN_PROGRESS.
		finishCtx := context.WithoutCancel(r.Context())
		if recorder.status >= 200 && recorder.status < 300 {
			if err := h.idempotency.CompleteIdempotencyKey(finishCtx, tenantID, key, recorder.status, recorder.body.Bytes()); err != nil {
				h.logger.Error("failed to record idempotent response", zap.Error(err))
			}
			return
		}
		if err := h.idempotency.ReleaseIdempotencyKey(finishCtx, tenantID, key); err != nil {
			h.logger.Error("failed to release idempotency key", zap.Error(err))
		}
	})
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// requestFingerprint identifies "the same request" for replay purposes.
func requestFingerprint(method, path string, body []byte) string {
	hash := sha256.New()
	hash.Write([]byte(method))
	hash.Write([]byte{0})
	hash.Write([]byte(path))
	hash.Write([]byte{0})
	hash.Write(bytes.TrimSpace(body))
	return hex.EncodeToString(hash.Sum(nil))
}

// capturingResponseWriter passes the response through while keeping a copy.
type capturingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (c *capturingResponseWriter) WriteHeader(status int) {
	if !c.wroteHeader {
		c.status = status
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingResponseWriter) Write(data []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	c.body.Write(data)
	return c.ResponseWriter.Write(data)
}
