// Package idempotency makes Idempotency-Key mean something.
//
// The canonical envelope contract has required the header on every material
// write in notification-svc since the contract rollout, and nothing read it
// (Group 1 cross-service finding 2). Here that meant a retried
// POST /v1/notifications/{id}/resend recorded a second reasoned resend, and a
// retried template approval answered 409 on the second call because the first
// had succeeded — a caller could not tell "done" from "refused".
//
// Ported from search-indexer-svc, itself ported from identity-context-svc
// (the pattern the audit names): the fingerprint is bound to the principal,
// and a claim abandoned by a crash is taken over by an identical retry.
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
	"strings"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/store"
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
	headerWorkload  = "X-Workload-Id"
	// headerReplayed tells a caller its answer came from the record rather
	// than from doing the work again.
	headerReplayed = "X-Idempotent-Replay"
)

// queryPaths are POSTs that are QUERIES. Deduplicating a query is meaningless
// — worse, replaying a stored search response would serve a caller results
// re-authorized for the moment of the first request, which is the stale
// permission NP-56 forbids ("prior cursor does not freeze permission").
var queryPaths = map[string]bool{
	"/v1/render-previews": true,
}

// exemptPrefixes carry no ZoikoSuite tenant identity (provider callbacks,
// mail-client unsubscribes, action links) and are authenticated otherwise.
var exemptPrefixes = []string{
	"/v1/provider-events/", "/v1/notifications/webhooks/", "/v1/notifications/actions/", "/v1/notifications/unsubscribe",
}

func exempt(path string) bool {
	for _, p := range exemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Middleware answers a repeated command from its first response.
//
// Only material writes that carry a key participate. A write without one is
// already refused upstream by the envelope contract, so this does not enforce
// presence.
func Middleware(s Store, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerKey)
			tenantID := r.Header.Get(headerTenant)

			if !materialWrite(r.Method) || key == "" || tenantID == "" || queryPaths[r.URL.Path] || exempt(r.URL.Path) {
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

			// The concrete path, not the route pattern: the same key used
			// against two different communications' /dispatch is two commands.
			endpoint := r.Method + " " + r.URL.Path
			principal := r.Header.Get(headerPrincipal)
			if principal == "" {
				principal = r.Header.Get(headerWorkload)
			}
			fingerprint := fingerprintOf(principal, string(body))

			rec, err := s.ClaimIdempotencyKey(r.Context(), tenantID, endpoint, key, fingerprint)
			switch {
			case errors.Is(err, store.ErrIdempotencyFingerprintMismatch):
				writeErr(w, http.StatusConflict, "IDEMPOTENCY_MISMATCH",
					"this Idempotency-Key was already used for a different request")
				return
			case err != nil:
				// Fail closed: executing a command that cannot be deduplicated
				// is exactly the behaviour being fixed.
				log.Error("idempotency claim failed", zap.Error(err), zap.String("endpoint", endpoint))
				writeErr(w, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE",
					"could not establish replay protection for this command")
				return
			}

			if rec != nil {
				if rec.ResponseStatus == 0 {
					writeErr(w, http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT",
						"an identical command is currently in flight")
					return
				}
				w.Header().Set(headerReplayed, "true")
				replay(w, rec)
				return
			}

			rw := &recorder{ResponseWriter: w, status: http.StatusOK}
			// A panicking handler never returns here; release the claim so
			// the retry is not told IDEMPOTENCY_IN_FLIGHT for the whole lease,
			// then re-panic so the Recoverer still answers 500.
			defer func() {
				if p := recover(); p != nil {
					if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
						log.Error("idempotency release after panic failed", zap.Error(err), zap.String("endpoint", endpoint))
					}
					panic(p)
				}
			}()
			next.ServeHTTP(rw, r)

			// 5xx is not a terminal answer, and neither is a 409 or 429: they
			// report a state that changes — a communication not yet prepared, an
			// approval not yet decided, an UNKNOWN attempt not yet reconciled. A
			// retry after the cause is fixed must genuinely retry, not be
			// handed the stale refusal.
			if rw.status >= 500 || rw.status == http.StatusConflict || rw.status == http.StatusTooManyRequests {
				if err := s.ReleaseIdempotencyKey(r.Context(), tenantID, endpoint, key); err != nil {
					log.Error("idempotency release failed", zap.Error(err), zap.String("endpoint", endpoint))
				}
				return
			}
			if err := s.CompleteIdempotencyKey(r.Context(), tenantID, endpoint, key, rw.status, rw.body.Bytes()); err != nil {
				// The command DID run; failing the response now would tell the
				// caller nothing happened when something did.
				log.Error("idempotency completion failed", zap.Error(err), zap.String("endpoint", endpoint))
			}
		})
	}
}

// fingerprintOf hashes the acting principal and the request body. With the
// principal folded in, a second principal presenting the first one's key and
// body is a DIFFERENT request and gets IDEMPOTENCY_MISMATCH — rather than the
// first principal's stored response, handed over before any authorization check
// ran. The NUL separator keeps ("ab","c") and ("a","bc") distinct.
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
	if rec.ResponseStatus == http.StatusNoContent || bytes.Equal(rec.ResponseBody, []byte("null")) {
		w.WriteHeader(rec.ResponseStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.ResponseStatus)
	_, _ = w.Write(rec.ResponseBody)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "detail": msg})
}
