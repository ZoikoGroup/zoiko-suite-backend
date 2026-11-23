package events

// REF-01 JurisdictionHierarchyChanged (ZS-SVC-I-001 s4.11, "Events produced").
//
// jurisdiction.hierarchy.changed says a jurisdiction moved: it has a different
// parent than it had. Everything that resolves a jurisdiction through its
// ancestors (tax, governance and organisation consumers) has to re-resolve, so
// the move gets its own event rather than hiding in jurisdiction.updated. It
// names the previous and the new parent and the version of the jurisdiction the
// move produced, which consumers can pin; and the source update it came from.

import (
	"context"
	"encoding/json"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const EventJurisdictionHierarchyChanged = "jurisdiction.hierarchy.changed"

// JurisdictionHierarchyPublisher is the optional capability of a Publisher to
// emit jurisdiction.hierarchy.changed. j is the jurisdiction after the move and
// previousParentID the parent it left (nil for a root).
type JurisdictionHierarchyPublisher interface {
	PublishJurisdictionHierarchyChanged(ctx context.Context, j domain.Jurisdiction, previousParentID *string, src *ImportSource, correlationID string) error
}

func (p *OutboxPublisher) PublishJurisdictionHierarchyChanged(ctx context.Context, j domain.Jurisdiction, previousParentID *string, src *ImportSource, correlationID string) error {
	return p.publish(ctx, EventJurisdictionHierarchyChanged, j.JurisdictionID, j.JurisdictionID,
		effectiveActor(j.CreatedByPrincipalID, j.UpdatedByPrincipalID), correlationID, j.DataClassification, j.RowVersion,
		marshalHierarchyChanged(j, previousParentID, src))
}

func (p *NoopPublisher) PublishJurisdictionHierarchyChanged(context.Context, domain.Jurisdiction, *string, *ImportSource, string) error {
	return p.publish(EventJurisdictionHierarchyChanged)
}

func marshalHierarchyChanged(j domain.Jurisdiction, previousParentID *string, src *ImportSource) []byte {
	payload := map[string]any{
		"jurisdiction_id":                 j.JurisdictionID,
		"jurisdiction_code":               j.JurisdictionCode,
		"jurisdiction_type":               j.JurisdictionType,
		"previous_parent_jurisdiction_id": previousParentID,
		"parent_jurisdiction_id":          j.ParentJurisdictionID,
	}
	if src != nil {
		payload["import"] = src.payload()
	}
	data, _ := json.Marshal(payload)
	return data
}
