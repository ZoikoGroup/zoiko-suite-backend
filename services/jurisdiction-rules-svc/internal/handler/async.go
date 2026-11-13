package handler

// API-5 asynchronous operations for the two long-running pack commands (tracker
// EST-5: "jurisdiction-rules compile and test runs").
//
// A caller that sends `Prefer: respond-async` on POST …/compile or …/test-runs
// gets 202 with Location: /v1/operations/{id} and the operation; the command
// itself then runs in the background, exactly as the synchronous request would
// (the same handler, the same authorization, events and evidence), in its own
// unit of work. The caller polls GET /v1/operations/{id} until the operation is
// succeeded (resource_ref names what it produced) or failed (problem carries
// the command's RFC 9457 answer). POST /v1/operations/{id}/cancel stops an
// operation that has not started; once it runs it completes, because a compile
// or a test run is one transaction and has no safe midpoint. The same
// Idempotency-Key with the same request answers the same operation; with a
// different request it is 409. Without Prefer the commands stay synchronous.

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

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/apicontract"

	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// asyncRoute names an operation type and the authorization its command needs.
type asyncRoute struct {
	opType, resource, action, resultSuffix string
}

var asyncRoutes = map[string]asyncRoute{
	"/v1/admin/packs/{pack_ref}/versions/{version}/compile":   {"jurisdiction_pack.compile", "jurisdiction_pack_version", "compile", "/artifact"},
	"/v1/admin/packs/{pack_ref}/versions/{version}/test-runs": {"jurisdiction_pack.test_run", "jurisdiction_pack_version", "execute_tests", "/test-runs"},
}

// WithAsyncOperations enables API-5 on the async routes and mounts the operation routes.
func (h *Handler) WithAsyncOperations(ops apicontract.OperationStore, units UnitStore) *Handler {
	h.operations, h.units = ops, units
	return h
}

func registerOperationRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/operations/{operation_id}", h.GetOperation)
	r.Post("/v1/operations/{operation_id}/cancel", h.CancelOperation)
}

func prefersAsync(r *http.Request) bool {
	for _, v := range r.Header.Values("Prefer") {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), "respond-async") {
				return true
			}
		}
	}
	return false
}

// asyncCapable makes the command at pattern run as an operation when asked.
func (h *Handler) asyncCapable(pattern string, fn http.HandlerFunc) http.HandlerFunc {
	spec, ok := asyncRoutes[pattern]
	if !ok {
		return fn
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !prefersAsync(r) || h.operations == nil || h.units == nil {
			fn(w, r)
			return
		}
		actor, ok := h.admin(w, r, spec.resource, spec.action)
		if !ok {
			return
		}
		tenant := requestTenant(r)
		if tenant == "" {
			writeError(w, r, http.StatusUnauthorized, "missing_tenant", "an asynchronous operation belongs to a tenant: X-Tenant-Id is required")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request_body", err.Error())
			return
		}
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		ref, version := chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
		now := time.Now().UTC()
		// Recorded outside the request's transaction: the operation must be
		// committed before its background run looks it up.
		op, created, err := h.operations.Create(store.WithTenant(store.WithoutUnit(r.Context()), tenant), apicontract.Operation{
			ID: apicontract.NewOperationID(), TenantID: tenant, PrincipalID: actor, Type: spec.opType,
			Route: r.URL.Path, State: apicontract.OperationQueued, Cancelable: true,
			ResourceRef:    "/v1/packs/" + ref + "/versions/" + version + spec.resultSuffix,
			IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), RequestHash: hex.EncodeToString(sum[:]),
			CreatedAt: now, UpdatedAt: now,
		})
		switch {
		case errors.Is(err, apicontract.ErrOperationConflict):
			writeError(w, r, http.StatusConflict, "idempotency_key_reused", "this Idempotency-Key started a different operation")
			return
		case err != nil:
			h.log.Error("create operation failed", zap.Error(err))
			writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
			return
		}
		if created {
			job := h.detach(r, body, tenant)
			go h.runOperation(op, job, fn)
		}
		apicontract.WriteOperation(w, http.StatusAccepted, op)
	}
}

// detach copies what the background run needs from the request: a body, the
// route parameters (chi recycles its route context when the request ends) and
// the request's values (envelope, verified identity, tenant), without its
// cancellation or its unit of work.
func (h *Handler) detach(r *http.Request, body []byte, tenant string) *http.Request {
	rctx := chi.NewRouteContext()
	if orig := chi.RouteContext(r.Context()); orig != nil {
		for i, k := range orig.URLParams.Keys {
			rctx.URLParams.Add(k, orig.URLParams.Values[i])
		}
		rctx.RoutePatterns = append(rctx.RoutePatterns, orig.RoutePatterns...)
	}
	ctx := context.WithValue(context.WithoutCancel(r.Context()), chi.RouteCtxKey, rctx)
	ctx = store.WithoutUnit(store.WithTenant(ctx, tenant))
	job := r.Clone(ctx)
	job.Body = io.NopCloser(bytes.NewReader(body))
	job.ContentLength = int64(len(body))
	job.Header.Del("Prefer")
	return job
}

func (h *Handler) runOperation(op apicontract.Operation, job *http.Request, fn http.HandlerFunc) {
	octx, cancel := context.WithTimeout(job.Context(), 10*time.Minute)
	defer cancel()
	if _, err := h.operations.Advance(octx, op.TenantID, op.ID, apicontract.OperationRunning, nil, nil, op.ResourceRef); err != nil {
		// A cancel arrived before the run started: settle it as cancelled.
		cur, gerr := h.operations.Get(octx, op.TenantID, op.ID)
		if gerr == nil && cur.State == apicontract.OperationCancelRequested {
			_, _ = h.operations.Advance(octx, op.TenantID, op.ID, apicontract.OperationCancelled, nil, nil, "")
			return
		}
		h.log.Error("operation could not start", zap.String("operation_id", op.ID), zap.Error(err))
		return
	}

	uctx, unit := h.units.BeginUnit(octx, op.TenantID)
	held := &heldResponse{header: http.Header{}, status: http.StatusOK}
	func() {
		defer func() {
			if p := recover(); p != nil {
				unit.Rollback(uctx)
				held.status = http.StatusInternalServerError
				held.body.Reset()
				h.log.Error("operation panicked", zap.String("operation_id", op.ID), zap.Any("panic", p))
			}
		}()
		fn(held, job.WithContext(uctx))
	}()
	if held.status < 500 || held.status >= 600 {
		if err := unit.Finish(uctx); err != nil {
			held.status = http.StatusServiceUnavailable
			held.body.Reset()
			_ = json.NewEncoder(&held.body).Encode(apicontract.ProblemFor(http.StatusServiceUnavailable, "store_unavailable", "the operation's changes could not be committed; nothing was written"))
		}
	} else {
		unit.Rollback(uctx)
	}

	fctx := store.WithTenant(context.WithoutCancel(octx), op.TenantID)
	if held.status >= 200 && held.status < 300 {
		if _, err := h.operations.Advance(fctx, op.TenantID, op.ID, apicontract.OperationSucceeded, nil, nil, op.ResourceRef); err != nil {
			h.log.Error("operation succeeded but could not be recorded", zap.String("operation_id", op.ID), zap.Error(err))
		}
		return
	}
	var p apicontract.Problem
	if err := json.Unmarshal(held.body.Bytes(), &p); err != nil || p.Status == 0 {
		p = apicontract.ProblemFor(held.status, apicontract.CodeForStatus(held.status), strings.TrimSpace(held.body.String()))
	}
	if _, err := h.operations.Advance(fctx, op.TenantID, op.ID, apicontract.OperationFailed, nil, &p, op.ResourceRef); err != nil {
		h.log.Error("operation failed and could not be recorded", zap.String("operation_id", op.ID), zap.Error(err))
	}
}

// GetOperation handles GET /v1/operations/{operation_id} (JURISDICTION_OPERATION_VIEW).
// Another tenant's operation is not found.
func (h *Handler) GetOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_operation", "view"); !ok {
		return
	}
	op, err := h.operations.Get(r.Context(), requestTenant(r), chi.URLParam(r, "operation_id"))
	if errors.Is(err, apicontract.ErrOperationNotFound) {
		writeError(w, r, http.StatusNotFound, "operation_not_found", "no such operation for this tenant")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if !op.State.Terminal() {
		w.Header().Set("Retry-After", "1")
	}
	apicontract.WriteOperation(w, http.StatusOK, op)
}

// CancelOperation handles POST /v1/operations/{operation_id}/cancel
// (JURISDICTION_OPERATION_CANCEL): an operation that has not started is
// cancelled; one that is running or finished is 409 operation_not_cancelable.
func (h *Handler) CancelOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_operation", "cancel"); !ok {
		return
	}
	tenant, id := requestTenant(r), chi.URLParam(r, "operation_id")
	cur, err := h.operations.Get(r.Context(), tenant, id)
	if errors.Is(err, apicontract.ErrOperationNotFound) {
		writeError(w, r, http.StatusNotFound, "operation_not_found", "no such operation for this tenant")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if cur.State != apicontract.OperationQueued {
		writeError(w, r, http.StatusConflict, "operation_not_cancelable",
			"only a queued operation can be cancelled; this one is "+string(cur.State))
		return
	}
	op, err := h.operations.RequestCancel(r.Context(), tenant, id)
	switch {
	case errors.Is(err, apicontract.ErrOperationNotCancelable):
		writeError(w, r, http.StatusConflict, "operation_not_cancelable", err.Error())
		return
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	apicontract.WriteOperation(w, http.StatusOK, op)
}
