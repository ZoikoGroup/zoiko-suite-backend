package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/store"
)

// executionTimeout bounds one pipeline run. The pipeline runs on a context that
// is DETACHED from the HTTP request: a client that disconnects must not leave a
// run stranded half-way (PREPARING forever); the outcome is always recorded.
const executionTimeout = 3 * time.Minute

// ExecuteRun — POST /controls/v1/runs/{run_id}/execute (§25).
//
// Idempotency-Key is mandatory: a retry of the same key returns the run as it
// stands and never starts a second execution. If-Match (optional) applies the
// ETag concurrency check to the start. Authorization is evaluated against the
// run's OWN legal entity, not a client-asserted one.
//
// The response is 200 with the run whatever the outcome — a PASS, a FAIL with
// exceptions, or a FAILED/INDETERMINATE technical failure. The outcome lives in
// the run's states, which is where it is authoritative.
func (h *Handler) ExecuteRun(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required to execute a run")
		return
	}
	expected, ok := parseIfMatch(w, r, store.SkipVersionCheck)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		h.writeErr(w, "execute run", err)
		return
	}
	// Authority is re-evaluated at execution time (Invariant 11 / §25).
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionExecute) {
		return
	}

	started, replay, err := h.store.BeginExecution(r.Context(), tenantID, runID, key, principal, corrID(r), expected)
	if err != nil {
		h.writeErr(w, "execute run", err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
		setETag(w, started.Version)
		writeJSON(w, http.StatusOK, started)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), executionTimeout)
	defer cancel()
	final, err := h.executor.Execute(ctx, engine.Input{TenantID: tenantID, RunID: runID, Actor: principal, CorrelationID: corrID(r)})
	if err != nil {
		h.writeErr(w, "execute run", err)
		return
	}
	final.Attention = attentionSignals(*final, time.Now().UTC())
	setETag(w, final.Version)
	writeJSON(w, http.StatusOK, final)
}

// GetPopulation — GET /controls/v1/runs/{run_id}/population. Always returns the
// frozen snapshots (counts, control totals, hash, watermark). With ?side=A|B it
// also pages the record identity set by keyset (?after=<seq>&limit=).
func (h *Handler) GetPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "get population", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionRead) {
		return
	}
	snaps, err := h.store.ListPopulationSnapshots(r.Context(), tenantID, run.RunID)
	if err != nil {
		h.writeErr(w, "get population", err)
		return
	}
	resp := map[string]any{"run_id": run.RunID, "populations": nonNil(snaps)}

	if s := strings.ToUpper(r.URL.Query().Get("side")); s != "" {
		if s != "A" && s != "B" {
			writeError(w, http.StatusBadRequest, "invalid_request", "side must be A or B")
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := h.store.ListPopulationRecords(r.Context(), tenantID, run.RunID, domain.Side(s), after, limit)
		if err != nil {
			h.writeErr(w, "get population records", err)
			return
		}
		recs := make([]domain.PopulationRecord, 0, len(rows))
		for _, row := range rows {
			recs = append(recs, row.Record)
		}
		resp["side"] = s
		resp["records"] = recs
		if n := len(rows); n > 0 && n == effectiveRecordLimit(limit) {
			resp["next_after"] = rows[n-1].Seq
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func effectiveRecordLimit(l int) int {
	if l <= 0 || l > 1000 {
		return 200
	}
	return l
}

// ListExceptions — GET /controls/v1/runs/{run_id}/exceptions.
func (h *Handler) ListExceptions(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "list exceptions", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.store.ListExceptions(r.Context(), tenantID, run.RunID, store.ListExceptionsFilter{
		State: q.Get("state"), Severity: q.Get("severity"), Limit: limit, AfterID: q.Get("after")})
	if err != nil {
		h.writeErr(w, "list exceptions", err)
		return
	}
	now := time.Now().UTC()
	for i := range list {
		list[i].Attention = list[i].DeriveAttention(now)
	}
	resp := map[string]any{"items": nonNil(list)}
	if n := len(list); n > 0 && n == effectiveExceptionLimit(limit) {
		resp["next_after"] = list[n-1].ExceptionID
	}
	writeJSON(w, http.StatusOK, resp)
}

func effectiveExceptionLimit(l int) int {
	if l <= 0 || l > 500 {
		return 100
	}
	return l
}

// AssignException — POST /controls/v1/exceptions/{exception_id}/assign.
// If-Match (the exception's ETag) is REQUIRED: ownership changes use
// expected-version semantics (§25).
func (h *Handler) AssignException(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	expected, ok := parseIfMatch(w, r, -2)
	if !ok {
		return
	}
	if expected == -2 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match with the exception's ETag is required")
		return
	}
	var req domain.AssignExceptionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "assign exception", err)
		return
	}
	id := chi.URLParam(r, "exception_id")
	ex, err := h.store.GetException(r.Context(), tenantID, id)
	if err != nil {
		h.writeErr(w, "assign exception", err)
		return
	}
	if !h.authorize(w, r, principal, ex.LegalEntityID, ActionAssign) {
		return
	}
	out, err := h.store.AssignException(r.Context(), tenantID, id, principal, corrID(r), expected, req)
	if err != nil {
		h.writeErr(w, "assign exception", err)
		return
	}
	out.Attention = out.DeriveAttention(time.Now().UTC())
	setETag(w, out.Version)
	writeJSON(w, http.StatusOK, out)
}

// GetEvidence — GET /controls/v1/runs/{run_id}/evidence. The digest is
// re-verified against the stored content on every read; integrity_verified=false
// means the package no longer matches what was sealed.
func (h *Handler) GetEvidence(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "get evidence", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionRead) {
		return
	}
	pkg, err := h.store.GetLatestEvidence(r.Context(), tenantID, run.RunID)
	if err != nil {
		h.writeErr(w, "get evidence", err)
		return
	}
	if pkg.Verified != nil && !*pkg.Verified {
		h.log.Error("evidence integrity check FAILED", zap.String("run_id", run.RunID), zap.String("package_id", pkg.PackageID))
	}
	writeJSON(w, http.StatusOK, pkg)
}

// parseIfMatch reads an If-Match ETag (`"3"` or `3`). When absent it returns
// def. A malformed value is a 400.
func parseIfMatch(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(strings.Trim(v, `"`))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must be the ETag returned by the API")
		return 0, false
	}
	return n, true
}
