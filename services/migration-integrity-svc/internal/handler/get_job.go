package handler

import (
	"net/http"
)

// GetJobByID handles GET /v1/migrations/{id}.
//
// It returns one job with its integrity checks, quarantine entries, control
// totals and open_quarantine_count. Archived jobs stay readable: archive is
// a soft delete and the evidence is retained.
//
// Validation, in order:
//   - X-Principal-Id present                                  401
//   - {id} is a UUID                                          400
//   - job exists in the caller's tenant (RLS)                 404 (also cross-tenant)
//   - MIGRATION_JOB_VIEW at the job's own legal entity        403 / 503
//
// Writes: none. Events: none.
// Success: 200 with the job and ETag = its version.
func (h *Handler) GetJobByID(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireCaller(w, r)
	if !ok {
		return
	}
	jobID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), caller.TenantID, jobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	// The job is fetched first because its own legal entity is what must be
	// authorized — the caller names no entity on this route.
	if !h.authorize(w, r, caller, job.LegalEntityID, ActionMigrationJobView) {
		return
	}

	w.Header().Set("ETag", etagFor(job.Version))
	h.writeJSON(w, http.StatusOK, job)
}
