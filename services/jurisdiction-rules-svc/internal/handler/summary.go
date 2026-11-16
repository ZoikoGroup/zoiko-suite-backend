package handler

import (
	"context"
	"net/http"

	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// SummaryStore reads the console summary (store.PgStore).
type SummaryStore interface {
	JurisdictionSummary(ctx context.Context) (*store.JurisdictionSummary, error)
}

// WithSummary mounts GET /v1/jurisdiction-summary.
func (h *Handler) WithSummary(s SummaryStore) *Handler {
	h.summary = s
	return h
}

// JurisdictionSummary handles GET /v1/jurisdiction-summary (tracker JR-FE-1):
// the live counts behind the console's KPI row, pack pipeline and attention
// callout. Authorized as JURISDICTION_SUMMARY_VIEW; counts only, no records.
//
//	200 → JurisdictionSummary
//	401 / 403 → no caller identity / JURISDICTION_SUMMARY_VIEW denied
//	503 → store unavailable
func (h *Handler) JurisdictionSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_summary", "view"); !ok {
		return
	}
	x, err := h.summary.JurisdictionSummary(r.Context())
	if err != nil {
		h.writeStoreError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, x)
}
