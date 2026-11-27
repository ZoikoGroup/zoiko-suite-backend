package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// Pagination bounds for GET /v1/migrations/.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// listJobsResponse is the list wrapper. data and count are the original
// contract (the frontend reads both); limit, offset and next_offset were
// added for pagination. next_offset is null on the last page.
type listJobsResponse struct {
	Data       []domain.MigrationJob `json:"data"`
	Count      int                   `json:"count"`
	Limit      int                   `json:"limit"`
	Offset     int                   `json:"offset"`
	NextOffset *int                  `json:"next_offset"`
}

// ListJobs handles GET /v1/migrations/.
//
// It lists job summaries (no checks, no audit entries, no source values) for
// ONE legal entity, newest first.
//
// Query: legal_entity_id (falls back to X-Legal-Entity-Id), status
// (optional JobStatus), limit (1..200, default 50), offset (>= 0).
//
// Validation, in order:
//   - X-Principal-Id present                                  401
//   - a legal entity is named                                 400
//   - status is a known JobStatus                             400
//   - limit / offset within bounds                            400
//   - MIGRATION_JOB_VIEW at that legal entity                 403 / 503
//
// A tenant-wide listing has no legal entity to authorize against, so it is
// refused rather than silently filtered. Another tenant's jobs are invisible
// (RLS), so a cross-tenant list is simply empty.
// Writes: none. Events: none. Success: 200 with listJobsResponse.
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireCaller(w, r)
	if !ok {
		return
	}
	filter, ok := parseListFilter(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, caller, filter.LegalEntityID, ActionMigrationJobView) {
		return
	}

	jobs, hasMore, err := h.store.ListJobs(r.Context(), caller.TenantID, filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	response := listJobsResponse{Data: jobs, Count: len(jobs), Limit: filter.Limit, Offset: filter.Offset}
	if hasMore {
		next := filter.Offset + len(jobs)
		response.NextOffset = &next
	}
	h.writeJSON(w, http.StatusOK, response)
}

// parseListFilter reads and bounds the list query parameters.
func parseListFilter(w http.ResponseWriter, r *http.Request) (store.ListFilter, bool) {
	query := r.URL.Query()
	filter := store.ListFilter{
		LegalEntityID: strings.TrimSpace(query.Get("legal_entity_id")),
		Status:        strings.TrimSpace(query.Get("status")),
		Limit:         defaultListLimit,
	}
	if filter.LegalEntityID == "" {
		filter.LegalEntityID = strings.TrimSpace(r.Header.Get(headerLegalEntityID))
	}
	if filter.LegalEntityID == "" {
		writeBadRequest(w, r, "legal_entity_id (query) or X-Legal-Entity-Id is required: reads are authorized per legal entity")
		return filter, false
	}
	if filter.Status != "" && !domain.IsValidJobStatus(filter.Status) {
		writeBadRequest(w, r, fmt.Sprintf("status %q is not one of PENDING, VALIDATING, COMPLETED, FAILED, ARCHIVED", filter.Status))
		return filter, false
	}

	var ok bool
	if filter.Limit, ok = parseBoundedInt(w, r, "limit", defaultListLimit, 1, maxListLimit); !ok {
		return filter, false
	}
	if filter.Offset, ok = parseBoundedInt(w, r, "offset", 0, 0, 1_000_000); !ok {
		return filter, false
	}
	return filter, true
}

// parseBoundedInt reads an optional integer query parameter within
// [minimum, maximum]; out of range or non-numeric is a 400.
func parseBoundedInt(w http.ResponseWriter, r *http.Request, name string, fallback, minimum, maximum int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		writeBadRequest(w, r, fmt.Sprintf("%s must be an integer between %d and %d, got %q", name, minimum, maximum, raw))
		return 0, false
	}
	return value, true
}
