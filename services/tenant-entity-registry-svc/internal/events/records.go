package events

import (
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// Outbox records for the write paths that used to publish straight to Kafka
// after commit (entity create/update/status, workspaces, hierarchies,
// jurisdiction assignments). Each is now written into event_outbox inside the
// same transaction as the fact it attests (ORG §9.2 gate 2), so a crash
// between commit and publish can no longer lose it.
//
// The payloads are exactly the ones the direct publisher emitted, so an
// existing consumer sees the same `payload` object; the envelope gains the
// §7 object identity, version and times.

const (
	EventLegalEntityUpdated         = "entity.updated"
	EventEntityHierarchyChanged     = "entity.hierarchy.changed"
	EventEntityJurisdictionChanged  = "entity.jurisdiction.changed"
	EventWorkspaceCreated           = "workspace.created"
	EventWorkspaceUpdated           = "workspace.updated"
	EventWorkspaceStatusChanged     = "workspace.status.changed"
	HierarchyChangeCreated          = "CREATED"
	HierarchyChangeEndDated         = "END_DATED"
	JurisdictionChangeAssigned      = "ASSIGNED"
	JurisdictionChangeEndDated      = "END_DATED"
	registryConflictObjectVersionV1 = 1
)

// EntityCreatedRecord is entity.created (LegalEntityCreated).
func EntityCreatedRecord(e *domain.LegalEntity, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventLegalEntityCreated,
		TenantID:      e.TenantID,
		LegalEntityID: e.LegalEntityID,
		Jurisdiction:  e.PrimaryJurisdictionID,
		ActorID:       e.CreatedByPrincipalID,
		CorrelationID: correlationID,
		ObjectID:      e.LegalEntityID,
		ObjectVersion: max64(e.RecordVersion, 1),
		EffectiveAt:   e.CreatedAt,
		PartitionKey:  e.LegalEntityID,
		Payload: map[string]any{
			"tenant_id":                e.TenantID,
			"legal_entity_id":          e.LegalEntityID,
			"entity_code":              e.EntityCode,
			"entity_type":              e.EntityType,
			"entity_status":            e.EntityStatus,
			"primary_jurisdiction_id":  e.PrimaryJurisdictionID,
			"data_residency_policy_id": e.DataResidencyPolicyID,
		},
	})
}

// EntityUpdatedRecord is entity.updated, from the row as the UPDATE left it.
func EntityUpdatedRecord(e *domain.LegalEntity, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventLegalEntityUpdated,
		TenantID:      e.TenantID,
		LegalEntityID: e.LegalEntityID,
		Jurisdiction:  e.PrimaryJurisdictionID,
		ActorID:       e.UpdatedByPrincipalID,
		CorrelationID: correlationID,
		ObjectID:      e.LegalEntityID,
		ObjectVersion: e.RecordVersion,
		EffectiveAt:   e.UpdatedAt,
		PartitionKey:  e.LegalEntityID,
		Payload: map[string]any{
			"tenant_id":       e.TenantID,
			"legal_entity_id": e.LegalEntityID,
		},
	})
}

// StatusChange is what a guarded status transition returns from inside its
// transaction: enough to name what moved, from what, to what, at which version.
type StatusChange struct {
	TenantID      string
	ObjectID      string
	LegalEntityID string
	Previous      string
	New           string
	RecordVersion int64
	ActorID       string
	At            time.Time
}

// EntityStatusChangedRecord is entity.status.changed for the generic status
// route. previous_status is now populated: the transition reads it in the
// same statement (it used to be sent empty).
func EntityStatusChangedRecord(c StatusChange, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventLegalEntityStatusChanged,
		TenantID:      c.TenantID,
		LegalEntityID: c.ObjectID,
		ActorID:       c.ActorID,
		CorrelationID: correlationID,
		ObjectID:      c.ObjectID,
		ObjectVersion: c.RecordVersion,
		EffectiveAt:   c.At,
		PartitionKey:  c.ObjectID,
		Payload: map[string]any{
			"tenant_id":       c.TenantID,
			"legal_entity_id": c.ObjectID,
			"previous_status": c.Previous,
			"new_status":      c.New,
		},
	})
}

// WorkspaceCreatedRecord is workspace.created.
func WorkspaceCreatedRecord(w *domain.Workspace, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventWorkspaceCreated,
		TenantID:      w.TenantID,
		LegalEntityID: deref(w.LegalEntityID),
		ActorID:       w.CreatedByPrincipalID,
		CorrelationID: correlationID,
		ObjectID:      w.WorkspaceID,
		ObjectVersion: max64(w.RecordVersion, 1),
		EffectiveAt:   w.CreatedAt,
		PartitionKey:  w.WorkspaceID,
		Payload: map[string]any{
			"tenant_id":              w.TenantID,
			"workspace_id":           w.WorkspaceID,
			"legal_entity_id":        w.LegalEntityID,
			"billing_classification": w.BillingClassification,
			"billing_source":         w.BillingSource,
		},
	})
}

// WorkspaceUpdatedRecord is workspace.updated, carrying the commercial fields:
// commercial-account-svc has to be told the new classification rather than
// having to come back and ask for it.
func WorkspaceUpdatedRecord(w *domain.Workspace, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventWorkspaceUpdated,
		TenantID:      w.TenantID,
		LegalEntityID: deref(w.LegalEntityID),
		ActorID:       w.UpdatedByPrincipalID,
		CorrelationID: correlationID,
		ObjectID:      w.WorkspaceID,
		ObjectVersion: w.RecordVersion,
		EffectiveAt:   w.UpdatedAt,
		PartitionKey:  w.WorkspaceID,
		Payload: map[string]any{
			"tenant_id":              w.TenantID,
			"workspace_id":           w.WorkspaceID,
			"legal_entity_id":        w.LegalEntityID,
			"name":                   w.Name,
			"billing_classification": w.BillingClassification,
			"billing_source":         w.BillingSource,
			"commercial_account_id":  w.CommercialAccountID,
		},
	})
}

// WorkspaceStatusChangedRecord is workspace.status.changed.
func WorkspaceStatusChangedRecord(c StatusChange, correlationID string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventWorkspaceStatusChanged,
		TenantID:      c.TenantID,
		LegalEntityID: c.LegalEntityID,
		ActorID:       c.ActorID,
		CorrelationID: correlationID,
		ObjectID:      c.ObjectID,
		ObjectVersion: c.RecordVersion,
		EffectiveAt:   c.At,
		PartitionKey:  c.ObjectID,
		Payload: map[string]any{
			"tenant_id":       c.TenantID,
			"workspace_id":    c.ObjectID,
			"previous_status": c.Previous,
			"new_status":      c.New,
		},
	})
}

// HierarchyChangedRecord is entity.hierarchy.changed, from the full row —
// the END_DATED event used to carry only the id and end date.
func HierarchyChangedRecord(h *domain.EntityHierarchy, changeType, correlationID string) (*outbox.Record, error) {
	actor, at := h.CreatedByPrincipalID, h.EffectiveFrom
	if changeType == HierarchyChangeEndDated {
		actor = h.UpdatedByPrincipalID
		if h.EffectiveTo != nil {
			at = *h.EffectiveTo
		}
	}
	return BuildRecord(RecordSpec{
		EventType:     EventEntityHierarchyChanged,
		TenantID:      h.TenantID,
		LegalEntityID: h.ChildLegalEntityID,
		ActorID:       actor,
		CorrelationID: correlationID,
		ObjectID:      h.HierarchyID,
		ObjectVersion: max64(h.RecordVersion, 1),
		EffectiveAt:   at,
		PartitionKey:  h.ChildLegalEntityID,
		Payload: map[string]any{
			"tenant_id":              h.TenantID,
			"hierarchy_id":           h.HierarchyID,
			"parent_legal_entity_id": h.ParentLegalEntityID,
			"child_legal_entity_id":  h.ChildLegalEntityID,
			"relationship_type":      h.RelationshipType,
			"change_type":            changeType,
			"effective_from":         h.EffectiveFrom,
			"effective_to":           h.EffectiveTo,
		},
	})
}

// JurisdictionChangedRecord is entity.jurisdiction.changed, from the full row.
func JurisdictionChangedRecord(a *domain.EntityJurisdictionAssignment, changeType, correlationID string) (*outbox.Record, error) {
	actor, at := a.CreatedByPrincipalID, a.EffectiveFrom
	if changeType == JurisdictionChangeEndDated {
		actor = a.UpdatedByPrincipalID
		if a.EffectiveTo != nil {
			at = *a.EffectiveTo
		}
	}
	return BuildRecord(RecordSpec{
		EventType:     EventEntityJurisdictionChanged,
		TenantID:      a.TenantID,
		LegalEntityID: a.LegalEntityID,
		Jurisdiction:  a.JurisdictionID,
		ActorID:       actor,
		CorrelationID: correlationID,
		ObjectID:      a.AssignmentID,
		ObjectVersion: max64(a.RecordVersion, 1),
		EffectiveAt:   at,
		EvidenceRef:   a.SourceBasis,
		PartitionKey:  a.LegalEntityID,
		Payload: map[string]any{
			"legal_entity_id": a.LegalEntityID,
			"assignment_id":   a.AssignmentID,
			"jurisdiction_id": a.JurisdictionID,
			"assignment_type": a.AssignmentType,
			"change_type":     changeType,
			"effective_from":  a.EffectiveFrom,
			"effective_to":    a.EffectiveTo,
		},
	})
}

// RegistryConflictQuarantinedRecord is §8 NP5's notification. A conflict
// row is written once, so its version is 1.
func RegistryConflictQuarantinedRecord(c *domain.EntityRegistryConflict) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventRegistryConflictQuarantined,
		TenantID:      c.TenantID,
		Jurisdiction:  c.JurisdictionID,
		ActorID:       c.DetectedByPrincipalID,
		CorrelationID: deref(c.CorrelationID),
		ObjectID:      c.ConflictID,
		ObjectVersion: registryConflictObjectVersionV1,
		EffectiveAt:   c.DetectedAt,
		PartitionKey:  c.TenantID,
		Payload: map[string]any{
			"conflict_id":              c.ConflictID,
			"registration_number":      c.RegistrationNumber,
			"jurisdiction_id":          c.JurisdictionID,
			"existing_legal_entity_id": c.ExistingLegalEntityID,
		},
	})
}

// ConflictResolution is what a registry-conflict resolution returns from
// inside its transaction.
type ConflictResolution struct {
	TenantID      string
	ConflictID    string
	Status        string
	Note          string
	ResolvedBy    string
	ApprovedBy    string
	CorrelationID string
	At            time.Time
}

// RegistryConflictResolvedRecord is the conclusion of a quarantine. A
// conflict resolves exactly once, so the resolved row is version 2.
func RegistryConflictResolvedRecord(r ConflictResolution) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType:     EventRegistryConflictResolved,
		TenantID:      r.TenantID,
		ActorID:       r.ResolvedBy,
		CorrelationID: r.CorrelationID,
		ObjectID:      r.ConflictID,
		ObjectVersion: registryConflictObjectVersionV1 + 1,
		EffectiveAt:   r.At,
		PartitionKey:  r.TenantID,
		Payload: map[string]any{
			"conflict_id":     r.ConflictID,
			"status":          r.Status,
			"resolution_note": r.Note,
			"resolved_by":     r.ResolvedBy,
			"approved_by":     r.ApprovedBy,
		},
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}
