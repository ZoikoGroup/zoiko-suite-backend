package handler

// J-10: backfill the entity-jurisdiction projection from tenant-entity-registry-svc.
//
// The projection (Doc 03 §8.2 "entity-jurisdiction applicability mappings") is
// fed by entity.created and entity.jurisdiction.changed from Kafka; an entity
// whose events left the topic's retention before this service consumed them has
// no mapping. The backfill reads the registry itself, for the caller's tenant,
// and upserts every entity's primary jurisdiction and every assignment through
// the consumer's own write: de-duplicated on a synthetic event id
// ("backfill:<id>:<version>") and guarded by the registry's record version, so a
// later live event still wins, and running the backfill again changes nothing.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/entityregistry"
)

// ApplicabilityWriter is the projection's write (store.PgStore, also the consumer's).
type ApplicabilityWriter interface {
	ApplyEntityApplicability(ctx context.Context, eventID, eventType string, a domain.EntityJurisdictionApplicability) (bool, error)
}

// EventEntityApplicabilityBackfilled announces a completed backfill.
const EventEntityApplicabilityBackfilled = "jurisdiction-entity-applicability.backfilled"

type entityBackfill struct {
	registry entityregistry.Reader
	writer   ApplicabilityWriter
}

// WithEntityBackfill mounts POST /v1/admin/entity-applicability:backfill.
func (h *Handler) WithEntityBackfill(reader entityregistry.Reader, writer ApplicabilityWriter) *Handler {
	h.backfill = &entityBackfill{registry: reader, writer: writer}
	return h
}

const backfillPath = "/v1/admin/entity-applicability:backfill"

type backfillRequest struct {
	// LegalEntityID limits the backfill to one entity; empty means every
	// entity of the caller's tenant.
	LegalEntityID string `json:"legal_entity_id"`
}

type backfillSkip struct {
	LegalEntityID string `json:"legal_entity_id"`
	ID            string `json:"id"`
	Reason        string `json:"reason"`
}

type backfillResult struct {
	TenantID        string         `json:"tenant_id"`
	EntitiesScanned int            `json:"entities_scanned"`
	MappingsRead    int            `json:"mappings_read"`
	Applied         int            `json:"applied"`
	Unchanged       int            `json:"unchanged"`
	Skipped         []backfillSkip `json:"skipped"`
}

// BackfillEntityApplicability handles POST /v1/admin/entity-applicability:backfill.
//
//	200 → counts: applied (new or newer), unchanged (already projected, or a
//	      newer live event holds), skipped (a registry row that cannot be projected)
//	400 → malformed body or legal_entity_id
//	401 → no tenant: the backfill is per tenant
//	403 → JURISDICTION_ENTITY_BACKFILL denied
//	502 → the registry refused the read (its own authorization or tenant scope)
//	503 → the registry or the store is unavailable. What was applied before
//	      the failure stays (each write is idempotent); running the backfill
//	      again completes the rest
func (h *Handler) BackfillEntityApplicability(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_entity", "backfill")
	if !ok {
		return
	}
	tenantID := requestTenant(r)
	if _, err := uuid.Parse(tenantID); err != nil {
		writeError(w, r, http.StatusUnauthorized, "missing_tenant", "X-Tenant-Id (a UUID) is required: the backfill runs for the caller's tenant")
		return
	}
	var req backfillRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	req.LegalEntityID = strings.TrimSpace(req.LegalEntityID)
	if req.LegalEntityID != "" {
		if _, err := uuid.Parse(req.LegalEntityID); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
			return
		}
	}

	ctx := r.Context()
	entities, err := h.backfill.registry.ListEntities(ctx, tenantID)
	if err != nil {
		h.writeBackfillError(w, r, err)
		return
	}
	res := backfillResult{TenantID: tenantID, Skipped: []backfillSkip{}}
	apply := func(eventID, eventType string, a domain.EntityJurisdictionApplicability) error {
		res.MappingsRead++
		applied, err := h.backfill.writer.ApplyEntityApplicability(ctx, eventID, eventType, a)
		switch {
		case errors.Is(err, domain.ErrInvalidEvent):
			res.Skipped = append(res.Skipped, backfillSkip{LegalEntityID: a.LegalEntityID, ID: a.ApplicabilityID, Reason: err.Error()})
			return nil
		case err != nil:
			return err
		case applied:
			res.Applied++
		default:
			res.Unchanged++
		}
		return nil
	}
	for _, e := range entities {
		if req.LegalEntityID != "" && e.LegalEntityID != req.LegalEntityID {
			continue
		}
		if e.TenantID != tenantID {
			// The registry answers only the tenant asked for; anything else
			// is never projected into this tenant.
			res.Skipped = append(res.Skipped, backfillSkip{LegalEntityID: e.LegalEntityID, ID: e.LegalEntityID, Reason: "entity of another tenant"})
			continue
		}
		res.EntitiesScanned++
		version := max(e.RecordVersion, 1)
		if _, err := uuid.Parse(e.PrimaryJurisdictionID); err == nil {
			if err := apply(fmt.Sprintf("backfill:primary:%s:%d", e.LegalEntityID, version), "entity.created",
				domain.EntityJurisdictionApplicability{
					ApplicabilityID: "primary:" + e.LegalEntityID, TenantID: tenantID, LegalEntityID: e.LegalEntityID,
					JurisdictionID: e.PrimaryJurisdictionID, ApplicabilityType: domain.ApplicabilityEntityPrimary,
					EffectiveFrom: e.CreatedAt, SourceObjectVersion: version,
				}); err != nil {
				h.writeBackfillError(w, r, err)
				return
			}
		}
		assignments, err := h.backfill.registry.ListAssignments(ctx, e.LegalEntityID)
		if err != nil {
			h.writeBackfillError(w, r, err)
			return
		}
		for _, a := range assignments {
			if a.TenantID != "" && a.TenantID != tenantID {
				res.Skipped = append(res.Skipped, backfillSkip{LegalEntityID: e.LegalEntityID, ID: a.AssignmentID, Reason: "assignment of another tenant"})
				continue
			}
			av := max(a.RecordVersion, 1)
			kind := a.AssignmentType
			if kind == "" {
				kind = "UNSPECIFIED"
			}
			if err := apply(fmt.Sprintf("backfill:%s:%d", a.AssignmentID, av), "entity.jurisdiction.changed",
				domain.EntityJurisdictionApplicability{
					ApplicabilityID: a.AssignmentID, TenantID: tenantID, LegalEntityID: e.LegalEntityID,
					JurisdictionID: a.JurisdictionID, ApplicabilityType: kind,
					EffectiveFrom: a.EffectiveFrom, EffectiveTo: a.EffectiveTo, SourceObjectVersion: av,
				}); err != nil {
				h.writeBackfillError(w, r, err)
				return
			}
		}
	}

	h.emitRegistry(r, EventEntityApplicabilityBackfilled, tenantID, actor, map[string]any{
		"tenant_id": tenantID, "legal_entity_id": req.LegalEntityID, "entities_scanned": res.EntitiesScanned,
		"mappings_read": res.MappingsRead, "applied": res.Applied, "unchanged": res.Unchanged, "skipped": len(res.Skipped),
	})
	h.log.Info("entity applicability backfill", zap.String("tenant_id", tenantID), zap.Int("entities", res.EntitiesScanned),
		zap.Int("applied", res.Applied), zap.Int("unchanged", res.Unchanged), zap.Int("skipped", len(res.Skipped)), zap.String("actor", actor))
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) writeBackfillError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, entityregistry.ErrRefused):
		writeError(w, r, http.StatusBadGateway, "registry_refused", err.Error())
	case errors.Is(err, entityregistry.ErrUnavailable):
		w.Header().Set("Retry-After", "5")
		writeError(w, r, http.StatusServiceUnavailable, "registry_unavailable", err.Error())
	default:
		h.writeStoreError(w, r, err, r.Header.Get("X-Correlation-ID"))
	}
}
