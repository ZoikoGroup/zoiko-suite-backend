package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/jurisdiction-rules-svc/internal/envelope"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// UnitStore opens a request's unit of work (store.PgStore).
type UnitStore interface {
	BeginUnit(ctx context.Context, tenantID string) (context.Context, *store.Unit)
}

// UnitOfWork runs every write request (any method but GET, HEAD and OPTIONS)
// in one database transaction (Governance audit J-11): the request's changes
// and the outbox rows announcing them commit together or not at all. The
// transaction carries the request's tenant for row-level security; a read
// carries it in its context.
//
// The handler's response is held until the transaction is decided, so a
// client is never told 2xx about a change that then failed to commit:
//
//   - committed → the held response is sent as written;
//   - the unit was aborted (an event could not be written) → 503
//     event_not_recorded, nothing persisted;
//   - the commit failed → 503 store_unavailable, nothing persisted;
//   - the handler panicked → rolled back, and the panic continues to the
//     Recoverer.
//
// Mount it inside IdempotencyMiddleware, so a stored replay is the response
// that was actually committed (a 5xx is never stored, and the client retries).
func UnitOfWork(st UnitStore, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				// Reads need no transaction, only the tenant their scoped
				// reads of tenant tables run as (row-level security, 000032).
				next.ServeHTTP(w, r.WithContext(store.WithTenant(r.Context(), requestTenant(r))))
				return
			}
			ctx, unit := st.BeginUnit(r.Context(), requestTenant(r))
			held := &heldResponse{header: http.Header{}, status: http.StatusOK}
			defer func() {
				if p := recover(); p != nil {
					unit.Rollback(ctx)
					panic(p)
				}
			}()
			next.ServeHTTP(held, r.WithContext(ctx))

			if err := unit.Finish(ctx); err != nil {
				code := "store_unavailable"
				detail := "the change could not be committed; nothing was written — retry with the same Idempotency-Key"
				if errors.Is(err, store.ErrUnitAborted) {
					code = "event_not_recorded"
					detail = "the change was rolled back because its event could not be written to the outbox; retry with the same Idempotency-Key"
				}
				log.Error("unit of work not committed", zap.String("method", r.Method), zap.String("path", r.URL.Path),
					zap.Int("handler_status", held.status), zap.Error(err))
				w.Header().Set("Retry-After", "1")
				writeError(w, r, http.StatusServiceUnavailable, code, detail)
				return
			}
			held.flush(w)
		})
	}
}

// requestTenant is the tenant the request acts for: the resolved input
// envelope's, else the gateway-set header.
func requestTenant(r *http.Request) string {
	if env, ok := svcenvelope.FromContext(r.Context()); ok && env.TenantID != "" {
		return env.TenantID
	}
	return strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
}

// heldResponse records a handler's response for release after the commit.
type heldResponse struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (h *heldResponse) Header() http.Header { return h.header }

func (h *heldResponse) WriteHeader(status int) {
	if h.wroteHeader {
		return
	}
	h.wroteHeader = true
	h.status = status
}

func (h *heldResponse) Write(b []byte) (int, error) {
	if !h.wroteHeader {
		h.WriteHeader(http.StatusOK)
	}
	return h.body.Write(b)
}

func (h *heldResponse) flush(w http.ResponseWriter) {
	dst := w.Header()
	for k, v := range h.header {
		dst[k] = v
	}
	w.WriteHeader(h.status)
	_, _ = w.Write(h.body.Bytes())
}
