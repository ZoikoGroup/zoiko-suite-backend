// Package idempotency implements Idempotency-Key replay for write endpoints
// (spec §16): a repeat of a request with the same key and the same body returns
// the stored result — HTTP status and body — with header Idempotent-Replay:
// true; the same key with a DIFFERENT request is refused 422
// IDEMPOTENCY_KEY_REUSED; the same key while the first attempt is still running
// is refused 409 IDEMPOTENCY_IN_PROGRESS.
//
// Only successful (2xx) outcomes are stored. A 4xx/5xx releases the key so the
// caller can correct the request (or retry after an outage) under the same key
// — replaying a stored authorization denial or a transient 503 forever would
// be the opposite of safe.
//
// Commands behind this middleware are additionally guarded by their own state
// machine / version checks, so a crash between executing the command and
// storing its result (the one window this design cannot close by itself) leaves
// a retry that hits the object's new state and is refused rather than applied
// twice. An in-flight record older than StaleAfter is taken over.
//
// Free of service-specific imports so it can be copied verbatim into sibling
// services.
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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// StaleAfter is how long an in-flight record blocks a retry of the same key.
const StaleAfter = 2 * time.Minute

// maxBody caps the request and stored response size handled here.
const maxBody = 1 << 20

// Outcome is the result of Begin.
type Outcome int

const (
	// Proceed: this caller owns the key and must run the request.
	Proceed Outcome = iota
	// Replay: a completed identical request exists; serve it.
	Replay
	// Mismatch: the key was used with a different request.
	Mismatch
	// InFlight: another attempt with this key is still running.
	InFlight
)

// Record is a stored result.
type Record struct {
	StatusCode int
	Body       []byte
}

// Store persists idempotency records.
type Store interface {
	Begin(ctx context.Context, tenantID, key, requestHash, method, path string) (Outcome, *Record, error)
	Complete(ctx context.Context, tenantID, key string, status int, body []byte) error
	Abandon(ctx context.Context, tenantID, key string) error
}

// PgStore is the Postgres Store (table idempotency_keys).
type PgStore struct{ pool *pgxpool.Pool }

// NewPgStore constructs a PgStore.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Begin claims (tenant, key) or reports what already exists there.
func (s *PgStore) Begin(ctx context.Context, tenantID, key, requestHash, method, path string) (Outcome, *Record, error) {
	out := Proceed
	var rec *Record
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, idempotency_key, request_hash, method, path)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`, tenantID, key, requestHash, method, path)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var hash string
		var status *int
		var body []byte
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			SELECT request_hash, status_code, response_body, created_at
			FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2 FOR UPDATE`, tenantID, key).
			Scan(&hash, &status, &body, &createdAt); err != nil {
			return err
		}
		switch {
		case hash != requestHash:
			out = Mismatch
		case status != nil:
			out = Replay
			rec = &Record{StatusCode: *status, Body: body}
		case time.Since(createdAt) > StaleAfter:
			// Abandoned in-flight record (process died mid-request): take over.
			if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key); err != nil {
				return err
			}
			out = Proceed
		default:
			out = InFlight
		}
		return nil
	})
	return out, rec, err
}

// Complete stores the successful result of the request that owns the key.
func (s *PgStore) Complete(ctx context.Context, tenantID, key string, status int, body []byte) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET status_code = $3, response_body = $4, completed_at = now()
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key, status, body)
		return err
	})
}

// Abandon releases a key whose request did not succeed.
func (s *PgStore) Abandon(ctx context.Context, tenantID, key string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			WHERE tenant_id = $1 AND idempotency_key = $2 AND status_code IS NULL`, tenantID, key)
		return err
	})
}

// Options configures Middleware.
type Options struct {
	// TenantFromContext resolves the caller's verified tenant scope.
	TenantFromContext func(context.Context) string
	// KeyRequired reports whether a request MUST carry an Idempotency-Key
	// (spec §16 mandatory list). A missing key then yields 400
	// IDEMPOTENCY_KEY_REQUIRED. Optional; nil means "honour when supplied".
	KeyRequired func(*http.Request) bool
	Log         *zap.Logger
}

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.buf.Len() < maxBody {
		r.buf.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

func writeErr(w http.ResponseWriter, status int, errName, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errName, "code": code, "detail": detail})
}

func isWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// Middleware applies Idempotency-Key replay to write requests.
func Middleware(st Store, opt Options) func(http.Handler) http.Handler {
	log := opt.Log
	if log == nil {
		log = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWrite(r) {
				next.ServeHTTP(w, r)
				return
			}
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if key == "" {
				if opt.KeyRequired != nil && opt.KeyRequired(r) {
					writeErr(w, http.StatusBadRequest, "idempotency_key_required", "IDEMPOTENCY_KEY_REQUIRED",
						"this command requires an Idempotency-Key header")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			tenantID := opt.TenantFromContext(r.Context())
			if tenantID == "" {
				// The handler refuses an unscoped request; nothing to key on yet.
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "VALIDATION_FAILED", "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := RequestHash(r.Method, r.URL.Path, body)

			outcome, rec, err := st.Begin(r.Context(), tenantID, key, hash, r.Method, r.URL.Path)
			if err != nil {
				// Cannot establish replay protection: fail closed rather than run
				// an unprotected write.
				log.Error("idempotency: store unavailable", zap.Error(err))
				writeErr(w, http.StatusServiceUnavailable, "store_unavailable", "STORE_UNAVAILABLE", "")
				return
			}
			switch outcome {
			case Replay:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(rec.StatusCode)
				_, _ = w.Write(rec.Body)
				return
			case Mismatch:
				writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "IDEMPOTENCY_KEY_REUSED",
					"this Idempotency-Key was already used with a different request")
				return
			case InFlight:
				w.Header().Set("Retry-After", "1")
				writeErr(w, http.StatusConflict, "idempotency_in_progress", "IDEMPOTENCY_IN_PROGRESS",
					"a request with this Idempotency-Key is still being processed")
				return
			}

			rw := &recorder{ResponseWriter: w}
			defer func() {
				// Detached context: the client may have gone away, but the key
				// must still be settled one way or the other.
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
				defer cancel()
				if p := recover(); p != nil {
					_ = st.Abandon(ctx, tenantID, key)
					panic(p)
				}
				if rw.status >= 200 && rw.status < 300 && rw.buf.Len() < maxBody {
					if err := st.Complete(ctx, tenantID, key, rw.status, rw.buf.Bytes()); err != nil {
						log.Error("idempotency: failed to store result", zap.Error(err))
					}
					return
				}
				if err := st.Abandon(ctx, tenantID, key); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("idempotency: failed to release key", zap.Error(err))
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// RequestHash is the fingerprint compared when a key is reused.
func RequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(canonicalJSON(body))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON re-marshals a JSON body so key order and whitespace do not make
// an otherwise identical request look different. Non-JSON bodies hash as-is.
func canonicalJSON(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}
