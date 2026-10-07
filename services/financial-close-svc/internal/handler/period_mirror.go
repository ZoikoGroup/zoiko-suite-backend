package handler

// REF-05 cutover, phase 1: the workflow-ref provenance endpoint REF-05 calls
// back, the best-effort dual-write mirror hooks, and the admin replay endpoint.
// financial-close-svc stays authoritative; see internal/periodmirror.

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/periodmirror"
)

// WorkflowRefReader is the read side of close_workflow_refs. It is a separate
// interface (not part of Store) so adding it does not widen the contract every
// Store implementation must satisfy.
type WorkflowRefReader interface {
	GetWorkflowRef(ctx context.Context, refID string) (*domain.WorkflowRef, error)
}

// PeriodMirror is the REF-05 dual-write. Implemented by *periodmirror.Mirror.
// Every method is best-effort: it never returns an error and never panics the
// caller's operation; failures are logged and counted inside.
type PeriodMirror interface {
	Enabled() bool
	MirrorLock(ctx context.Context, req periodmirror.Request) []domain.MirrorActionResult
	MirrorSoftClose(ctx context.Context, req periodmirror.Request) []domain.MirrorActionResult
	MirrorReopen(ctx context.Context, req periodmirror.Request) []domain.MirrorActionResult
	Fail(command, reason string) domain.MirrorActionResult
}

// SetWorkflowRefs wires the provenance endpoint: refs is where rows are read
// from, callers are the X-Workload-Id values allowed to read them. With no
// callers every request is refused (fail closed).
func (h *Handler) SetWorkflowRefs(refs WorkflowRefReader, callers []string) *Handler {
	h.workflowRefs = refs
	h.workflowRefCallers = make(map[string]struct{}, len(callers))
	for _, c := range callers {
		if c != "" {
			h.workflowRefCallers[c] = struct{}{}
		}
	}
	return h
}

// SetPeriodMirror wires the REF-05 mirror. nil (the default) means no mirroring.
func (h *Handler) SetPeriodMirror(m PeriodMirror) *Handler {
	h.mirror = m
	return h
}

func (h *Handler) mirrorOn() bool { return h.mirror != nil && h.mirror.Enabled() }

// ── GET /v1/close/workflow-refs/{ref} ───────────────────────────────────────────
//
// The contract accounting-period-svc codes against. Read-only; no side effects.
//
//	tenant:  X-Tenant-Id (401 if absent)
//	caller:  X-Workload-Id must be in WORKFLOW_REF_CALLERS (403 otherwise)
//	200:     {"workflow_ref","period_key","legal_entity_id","command","status","control_snapshot_ref"}
//	404:     unknown ref, malformed ref, or another tenant ref -- indistinguishable
//
// Only a row whose status is APPROVED is ever reported as "APPROVED".
func (h *Handler) GetWorkflowRef(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	caller := r.Header.Get("X-Workload-Id")
	if _, allowed := h.workflowRefCallers[caller]; caller == "" || !allowed {
		writeError(w, http.StatusForbidden, "caller_not_allowed", "this workload may not read workflow refs")
		return
	}
	if h.workflowRefs == nil {
		writeError(w, http.StatusServiceUnavailable, "workflow_refs_unavailable", "")
		return
	}
	wr, err := h.workflowRefs.GetWorkflowRef(r.Context(), chi.URLParam(r, "ref"))
	switch {
	case err == nil && wr.TenantID == tenantID:
		writeJSON(w, http.StatusOK, domain.WorkflowRefResponse{
			WorkflowRef:        wr.RefID,
			PeriodKey:          wr.PeriodKey,
			LegalEntityID:      wr.LegalEntityID,
			Command:            wr.Command,
			Status:             wr.Status,
			ControlSnapshotRef: wr.ControlSnapshotRef,
		})
	case err == nil, errors.Is(err, domain.ErrWorkflowRefNotFound):
		writeError(w, http.StatusNotFound, "workflow_ref_not_found", "")
	default:
		h.writeStoreErr(w, err, "workflow_ref_not_found")
	}
}

// ── mirror hooks ────────────────────────────────────────────────────────────────

// mirrorLock runs after a successful local lock. It cannot fail the lock.
func (h *Handler) mirrorLock(ctx context.Context, tenantID, correlationID, principalID string, fp *domain.FiscalPeriod, evidenceDocID string, blockingIssues []string) {
	if !h.mirrorOn() {
		return
	}
	defer h.recoverMirror()
	h.mirror.MirrorLock(ctx, periodmirror.Request{
		TenantID: tenantID, CorrelationID: correlationID, Principal: principalID, Period: *fp,
		EvidenceDocumentID: evidenceDocID,
		Readiness:          &periodmirror.ReadinessSnapshot{IsReady: len(blockingIssues) == 0, BlockingIssues: blockingIssues},
	})
}

// mirrorReopen runs after a successful local reopen. It cannot fail the reopen.
func (h *Handler) mirrorReopen(ctx context.Context, tenantID, correlationID, principalID string, fp domain.FiscalPeriod, evidenceDocID, reason string) {
	if !h.mirrorOn() {
		return
	}
	defer h.recoverMirror()
	h.mirror.MirrorReopen(ctx, periodmirror.Request{
		TenantID: tenantID, CorrelationID: correlationID, Principal: principalID, Period: fp,
		EvidenceDocumentID: evidenceDocID, Reason: reason,
	})
}

// recoverMirror is the last line of the "never fails the local operation" rule.
func (h *Handler) recoverMirror() {
	if rec := recover(); rec != nil {
		h.log.Error("REF-05 period mirror panicked; the local operation is unaffected", zap.Any("panic", rec))
	}
}

// ── POST /v1/close/periods/{id}:mirror-to-period-service ───────────────────────
//
// Admin replay: re-drives the REF-05 mirror for the CURRENT legacy state of one
// fiscal period (OPEN => no-op, CLOSED => soft-close step, LOCKED => soft + hard).
// Used by the backfill tool. Idempotent: REF-05 own state decides each step, so
// a step already applied is reported "skipped". The caller is the human principal
// of the hard-close step. Authorised like LockPeriod (PERIOD_CLOSE_INITIATE).
//
// 200 always carries per-action outcomes (applied|skipped|failed); a "failed"
// action does NOT change the HTTP status -- the caller must inspect the actions.
func (h *Handler) MirrorPeriodToPeriodService(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Header.Get("Idempotency-Key") == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required")
		return
	}
	if !h.mirrorOn() {
		writeError(w, http.StatusConflict, "period_mirror_disabled", "PERIOD_SERVICE_MIRROR is off")
		return
	}
	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	resp := domain.MirrorReplayResponse{FiscalPeriodID: fp.FiscalPeriodID, LegacyState: fp.CloseStatus, Actions: []domain.MirrorActionResult{}}
	switch fp.CloseStatus {
	case "OPEN":
		writeJSON(w, http.StatusOK, resp)
		return
	case "CLOSED", "LOCKED":
	default:
		writeError(w, http.StatusUnprocessableEntity, "unknown_legacy_state", fp.CloseStatus)
		return
	}

	// The readiness snapshot of a replay is computed now, as of the replay.
	issues, err := h.checkReadiness(r.Context(), tenantID, principalID, fp)
	if err != nil {
		h.log.Error("mirror replay: readiness could not be computed", zap.String("period_id", id), zap.Error(err))
		resp.Actions = append(resp.Actions, h.mirror.Fail(domain.WorkflowCmdSoftClose, periodmirror.ReasonReadinessUnavail))
		writeJSON(w, http.StatusOK, resp)
		return
	}
	req := periodmirror.Request{
		TenantID: tenantID, CorrelationID: correlationID, Principal: principalID, Period: *fp,
		EvidenceDocumentID: derefString(fp.EvidenceDocumentID), Replay: true,
		Readiness: &periodmirror.ReadinessSnapshot{IsReady: len(issues) == 0, BlockingIssues: issues},
	}
	if fp.CloseStatus == "LOCKED" {
		resp.Actions = h.mirror.MirrorLock(r.Context(), req)
	} else {
		resp.Actions = h.mirror.MirrorSoftClose(r.Context(), req)
	}
	if resp.Actions == nil {
		resp.Actions = []domain.MirrorActionResult{}
	}
	writeJSON(w, http.StatusOK, resp)
}
