package events

// REF-01 JurisdictionUpdated (ZS-SVC-I-001 s4.11, "Events produced").
//
// jurisdiction.updated says something about an existing jurisdiction changed
// without it being created or retired: today, a code that identifies it was
// added or ended. The change is named in `change_type`, so a consumer that only
// cares about one kind can filter, and the next kinds (an attribute change from
// a source import) reuse the same event.

import (
	"context"
	"encoding/json"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const EventJurisdictionUpdated = "jurisdiction.updated"

// Change types carried by jurisdiction.updated.
const (
	ChangeAliasAdded        = "ALIAS_ADDED"
	ChangeAliasEnded        = "ALIAS_ENDED"
	ChangeAttributesChanged = "ATTRIBUTES_CHANGED"
)

// ImportSource says which external source update a change came from, so a
// consumer can tie it to the batch, the standard and the version.
type ImportSource struct {
	BatchID        string
	SourceStandard string
	SourceVersion  string
	ImportHash     string
}

func (s *ImportSource) payload() map[string]any {
	return map[string]any{
		"batch_id": s.BatchID, "source_standard": s.SourceStandard,
		"source_version": s.SourceVersion, "import_hash": s.ImportHash,
	}
}

// JurisdictionChange says what changed about a jurisdiction.
type JurisdictionChange struct {
	Type  string
	Alias *domain.JurisdictionAlias
	// Before is the jurisdiction as it was, for an attribute change: the event
	// then carries the previous and the current name and authority type.
	Before *domain.Jurisdiction
	// Import is the source update the change came from, when it came from one.
	Import *ImportSource
}

// JurisdictionUpdatePublisher is the optional capability of a Publisher to emit
// jurisdiction.updated. It is separate from Publisher so a publisher that
// predates it keeps satisfying that interface.
type JurisdictionUpdatePublisher interface {
	PublishJurisdictionUpdated(ctx context.Context, j domain.Jurisdiction, c JurisdictionChange, correlationID string) error
}

func (p *OutboxPublisher) PublishJurisdictionUpdated(ctx context.Context, j domain.Jurisdiction, c JurisdictionChange, correlationID string) error {
	return p.publish(ctx, EventJurisdictionUpdated, j.JurisdictionID, j.JurisdictionID,
		effectiveActor(j.CreatedByPrincipalID, j.UpdatedByPrincipalID), correlationID, j.DataClassification, j.RowVersion,
		marshalJurisdictionUpdated(j, c))
}

func (p *NoopPublisher) PublishJurisdictionUpdated(context.Context, domain.Jurisdiction, JurisdictionChange, string) error {
	return p.publish(EventJurisdictionUpdated)
}

func marshalJurisdictionUpdated(j domain.Jurisdiction, c JurisdictionChange) []byte {
	payload := map[string]any{
		"jurisdiction_id":   j.JurisdictionID,
		"jurisdiction_code": j.JurisdictionCode,
		"change_type":       c.Type,
		"active_flag":       j.ActiveFlag,
	}
	if a := c.Alias; a != nil {
		payload["alias"] = map[string]any{
			"alias_id":          a.AliasID,
			"alias_system":      a.AliasSystem,
			"alias_code":        a.AliasCode,
			"source_standard":   a.SourceStandard,
			"source_version":    a.SourceVersion,
			"mapping_rationale": a.MappingRationale,
			"valid_from":        a.ValidFrom,
			"valid_to":          a.ValidTo,
		}
	}
	if b := c.Before; b != nil {
		payload["previous"] = map[string]any{"jurisdiction_name": b.JurisdictionName, "authority_type": b.AuthorityType}
		payload["current"] = map[string]any{"jurisdiction_name": j.JurisdictionName, "authority_type": j.AuthorityType}
	}
	if c.Import != nil {
		payload["import"] = c.Import.payload()
	}
	data, _ := json.Marshal(payload)
	return data
}
