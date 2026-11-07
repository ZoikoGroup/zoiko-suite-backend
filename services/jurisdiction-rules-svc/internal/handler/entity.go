package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/approval"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// EntityApplicabilityReader reads the entity-jurisdiction applicability projection.
type EntityApplicabilityReader interface {
	ListEntityJurisdictions(ctx context.Context, tenantID, legalEntityID string, at time.Time) ([]*domain.EntityJurisdictionApplicability, error)
}

// WithEntityApplicability mounts GET /v1/entities/{legal_entity_id}/jurisdictions.
func (h *Handler) WithEntityApplicability(r EntityApplicabilityReader) *Handler {
	h.entities = r
	return h
}

// GetEntityJurisdictions handles GET /v1/entities/{legal_entity_id}/jurisdictions.
//
// The "resolve jurisdiction set" capability of Doc 03 §8.2 from the entity's
// side: which jurisdictions apply to this legal entity at effective_at,
// projected from tenant-entity-registry-svc's entity.created and
// entity.jurisdiction.changed. Each jurisdiction_id can then be resolved to a
// rule pack.
//
// Unlike the jurisdiction reference reads, this is tenant data: X-Tenant-Id is
// required and filters every row. An entity of another tenant is an empty
// list, indistinguishable from an unknown one.
//
//	200 → {legal_entity_id, effective_at, jurisdictions: [...]} (possibly empty)
//	400 → malformed effective_at
//	401 → no X-Tenant-Id
//	503 → store unavailable
func (h *Handler) GetEntityJurisdictions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := chi.URLParam(r, "legal_entity_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenantID == "" {
		writeError(w, r, http.StatusUnauthorized, "missing_tenant",
			"X-Tenant-Id is required: entity jurisdiction mappings are tenant data")
		return
	}

	at, ok := h.parseEffectiveAt(w, r, r.URL.Query())
	if !ok {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	rows, err := h.entities.ListEntityJurisdictions(r.Context(), tenantID, legalEntityID, at)
	if err != nil {
		h.writeStoreError(w, r, err, correlationID)
		return
	}
	h.log.Debug("GetEntityJurisdictions",
		zap.String("legal_entity_id", legalEntityID),
		zap.Int("count", len(rows)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"legal_entity_id": legalEntityID,
		"effective_at":    at,
		"jurisdictions":   rows,
	})
}

// WithApprovalVerifier makes rule activation confirm its X-Approval-Reference
// with workflow-svc (Governance audit J-02). Without it the reference is
// recorded and not checked, which main.go allows only in local/dev.
func (h *Handler) WithApprovalVerifier(v approval.Verifier) *Handler {
	h.approvals = v
	return h
}

