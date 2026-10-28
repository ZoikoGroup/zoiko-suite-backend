package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// Authorization actions of migration and cutover (standard §28, §30). Staging, approving and posting are separate actions held by
// different people: the person who staged a batch cannot approve it, and a migration has its own authority.
const (
	actionMigrationStage     = "GL_MIGRATION_STAGE"
	actionMigrationRead      = "GL_MIGRATION_READ"
	actionMigrationApprove   = "GL_MIGRATION_APPROVE"
	actionMigrationPost      = "GL_MIGRATION_POST"
	actionMigrationReconcile = "GL_MIGRATION_RECONCILE"
)

func (h *KernelHandler) migration(w http.ResponseWriter, tenant string) (*kernel.MigrationService, bool) {
	mr, ok := h.repo.(kernel.MigrationRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "migration_not_configured", "")
		return nil, false
	}
	return &kernel.MigrationService{Repo: mr, Books: h.repo, Dir: kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: h.fxSource(tenant)}, Gate: h.gate}, true
}

// StageMigrationBatch stages a batch of opening lines from a source extract. POST /v1/migration-batches
func (h *KernelHandler) StageMigrationBatch(w http.ResponseWriter, r *http.Request) {
	var req kernel.StageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id is required")
		return
	}
	tenant, a, ok := h.guardFor(w, r, actionMigrationStage, req.LegalEntityID)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	b, err := svc.Stage(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	if b.Existing {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, http.StatusOK, b)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (h *KernelHandler) ListMigrationBatches(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionMigrationRead)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := svc.Repo.ListMigrationBatches(r.Context(), tenant, q.Get("legal_entity_id"), q.Get("status"), queryLimit(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": list, "total": len(list)})
}

func (h *KernelHandler) GetMigrationBatch(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionMigrationRead)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	b, err := svc.Repo.GetMigrationBatch(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ListMigrationLines pages through the staged lines. GET /v1/migration-batches/{id}/lines?offset=&limit=
func (h *KernelHandler) ListMigrationLines(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionMigrationRead)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	lines, err := svc.Repo.MigrationLines(r.Context(), tenant, chi.URLParam(r, "id"), offset, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "offset": offset, "limit": limit})
}

// PreviewMigrationBatch is the migration reconciliation a human reads before anything posts. It writes nothing.
func (h *KernelHandler) PreviewMigrationBatch(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionMigrationRead)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	p, err := svc.Preview(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ValidateMigrationBatch checks the batch and records the report; a clean one becomes VALIDATED.
func (h *KernelHandler) ValidateMigrationBatch(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionMigrationStage)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	b, an, err := svc.Validate(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": b, "analysis": an, "valid": len(an.Issues) == 0})
}

type migrationApproveBody struct {
	AuthorityRef string    `json:"authority_ref"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
}

// ApproveMigrationBatch records the migration authority and the cutover window. Someone other than the person who staged it.
func (h *KernelHandler) ApproveMigrationBatch(w http.ResponseWriter, r *http.Request) {
	var b migrationApproveBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionMigrationApprove)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	out, err := svc.Approve(r.Context(), tenant, a, chi.URLParam(r, "id"), b.AuthorityRef, b.WindowStart, b.WindowEnd)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *KernelHandler) CancelMigrationBatch(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionMigrationStage)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	out, err := svc.Cancel(r.Context(), tenant, a, chi.URLParam(r, "id"), b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PostMigrationBatch posts an approved batch as one OPENING journal inside its cutover window.
func (h *KernelHandler) PostMigrationBatch(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionMigrationPost)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	out, err := svc.Post(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if out.Existing {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ReconcileMigrationBatch compares the posted opening journal with the source and the subledgers; a match freezes the evidence.
func (h *KernelHandler) ReconcileMigrationBatch(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionMigrationReconcile)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	out, err := svc.Reconcile(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetMigrationEvidence returns the batch with its history and whether its hashes still verify against what is stored.
func (h *KernelHandler) GetMigrationEvidence(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionMigrationRead)
	if !ok {
		return
	}
	svc, ok := h.migration(w, tenant)
	if !ok {
		return
	}
	ev, err := svc.Evidence(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}
