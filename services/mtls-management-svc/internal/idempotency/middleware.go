// Package idempotency answers a repeated material write from its first
// response, so a retried POST/PUT carrying the same Idempotency-Key does not
// run the command twice.
//
// Why this exists: the platform envelope requires an Idempotency-Key on every
// material write, but this service never read it — the 2026-10-27 live e2e
// run showed a retried create making a second record. Same design as
// delegated-authority-svc, identity-context-svc and notification-svc:
//
//  1. Claim (tenant, METHOD + path, key) with a fingerprint of principal+body.
//  2. No previous claim      -> run the handler, store its answer.
//     Same key, same request -> replay the stored answer (X-Idempotent-Replay: true).
//     Same key, other request-> 409 idempotency_mismatch.
//     Previous still running -> 409 idempotency_in_flight.
//  3. Answers that describe a changing state (5xx, 409, 428, 429) are not
//     stored, so a retry after the cause is fixed genuinely retries.
//
// Fail closed: if replay protection cannot be established (store error) the
// command is refused with 503 rather than run without it.
//
// mtls-management-svc addition: an answer that carries secret material (the
// leaf private key returned once by provision/rotate) is never stored. The
// claim is completed with secret_withheld = true and no body, and a replay is
// refused with 409 idempotency_secret_not_replayable: the command still does
// not run twice, and no private key ever reaches the database.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"
)

// ErrFingerprintMismatch: the key was already used for a different request.
var ErrFingerprintMismatch = errors.New("idempotency key reused for a different request")

// Record is a stored claim; ResponseStatus 0 means the first request is still running.
// SecretWithheld means the first answer carried secret material and was
// deliberately not stored, so it cannot be replayed.
type Record struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	SecretWithheld     bool
}

// Store is the persistence the middleware needs. PgStore implements it.
type Store interface {
	// Claim returns (nil, nil) when the caller now holds the claim and must run
	// the command; a Record when an identical request already owns the key;
	// ErrFingerprintMismatch when the key was used for a different request.
	Claim(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*Record, error)
	// Complete records the terminal answer. With secretWithheld the body is
	// discarded (stored as NULL) and only the fact of completion is kept.
	Complete(ctx context.Context, tenantID, endpoint, key string, status int, body []byte, secretWithheld bool) error
	Release(ctx context.Context, tenantID, endpoint, key string) error
}

const (
	headerKey       = "Idempotency-Key"
	headerTenant    = "X-Tenant-Id"
	headerPrincipal = "X-Principal-Id"
	headerReplayed  = "X-Idempotent-Replay"
	maxBodyBytes    = 1 << 20
)

// Option configures Middleware.
type Option func(*options)

type options struct {
	carriesSecret func(body []byte) bool
}

// WithSecretDetector marks answers that must never be stored: detect is given
// the response body and returns true when it carries secret material.
func WithSecretDetector(detect func(body []byte) bool) Option {
	return func(o *options) { o.carriesSecret = detect }
}

// Middleware deduplicates material writes that carry an Idempotency-Key. A
// write without a key is refused earlier by the envelope middleware, so the
// key's presence is not enforced again here.
func Middleware(store Store, log *zap.Logger, opts ...Option) func(http.Handler) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)
			if !isMaterialWrite(r.Method) || key == "" || tenantID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Read the body once so it can be fingerprinted, then hand the
			// handler an identical copy.
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "could not read request body")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))

			// The concrete path and query: the same key sent to two different
			// resources is two different commands.
			endpoint := r.Method + " " + r.URL.RequestURI()
			fingerprint := fingerprintOf(r.Header.Get(headerPrincipal), body)

			previous, err := store.Claim(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, ErrFingerprintMismatch):
				writeError(w, r, http.StatusConflict, "idempotency_mismatch",
					"this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				log.Error("idempotency claim failed — refusing the write", zap.Error(err), zap.String("endpoint", endpoint))
				writeError(w, r, http.StatusServiceUnavailable, "store_unavailable",
					"could not establish replay protection for this command")
				return
			}

			if previous != nil {
				if previous.ResponseStatus == 0 {
					writeError(w, r, http.StatusConflict, "idempotency_in_flight",
						"an identical command is currently in flight")
					return
				}
				if previous.SecretWithheld {
					writeError(w, r, http.StatusConflict, "idempotency_secret_not_replayable",
						"the first request with this Idempotency-Key succeeded; its answer carried a private key, "+
							"which is returned once and never retained. Read the certificate, or rotate it for new key material")
					return
				}
				replayStoredAnswer(w, previous)
				return
			}

			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				if p := recover(); p != nil {
					_ = store.Release(r.Context(), tenantID, endpoint, key)
					panic(p)
				}
			}()
			next.ServeHTTP(recorder, r)

			if isTransientOutcome(recorder.status) {
				if err := store.Release(r.Context(), tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			answer := recorder.body.Bytes()
			withheld := o.carriesSecret != nil && o.carriesSecret(answer)
			if withheld {
				answer = nil
			}
			if err := store.Complete(r.Context(), tenantID, endpoint, key, recorder.status, answer, withheld); err != nil {
				// The command DID run; failing the response now would claim it did not.
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// replayStoredAnswer writes the first request's status and body again.
func replayStoredAnswer(w http.ResponseWriter, previous *Record) {
	w.Header().Set(headerReplayed, "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(previous.ResponseStatus)
	if len(previous.ResponseBody) > 0 {
		_, _ = w.Write(previous.ResponseBody)
	}
}

// fingerprintOf binds the key to the acting principal and the exact body, so a
// different principal presenting someone else's key gets a mismatch instead of
// their stored answer.
func fingerprintOf(principalID string, body []byte) string {
	hash := sha256.New()
	hash.Write([]byte(principalID))
	hash.Write([]byte{0})
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func isMaterialWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// isTransientOutcome: answers that describe a state which can change, so they
// must not be replayed to a later retry.
func isTransientOutcome(status int) bool {
	return status >= 500 ||
		status == http.StatusConflict ||
		status == http.StatusPreconditionRequired ||
		status == http.StatusTooManyRequests
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// writeError renders refusals as RFC 9457 problem+json (apicontract), like
// every other error this service writes.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    status,
		Code:      code,
		Detail:    message,
		Retryable: status >= 500 || code == "idempotency_in_flight",
	})
}
