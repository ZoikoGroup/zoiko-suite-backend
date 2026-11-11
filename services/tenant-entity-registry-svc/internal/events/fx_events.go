package events

// REF-03 exchange-rate events (ZS-SVC-I-001 s4.13: FXRateObserved, FXRateApproved, FXRateSetClosed,
// FXRateSuperseded).
//
// A rate is reference data and is published as such: the pair, type, market date and rate travel in the event so a
// consumer need not call back. The raw provider payload never does: only its reference and hash exist here.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventFXRateObserved      = "fx.rate.observed"       // FXRateObserved
	EventFXRateApproved      = "fx.rate.approved"       // FXRateApproved
	EventFXRateSetClosed     = "fx.rate_set.closed"     // FXRateSetClosed
	EventFXRateSuperseded    = "fx.rate.superseded"     // FXRateSuperseded
	EventFXRateSetSuperseded = "fx.rate_set.superseded" // (no spec name)
)

// FXEventNames is every exchange-rate wire name the code can emit.
var FXEventNames = []string{EventFXRateObserved, EventFXRateApproved, EventFXRateSetClosed, EventFXRateSuperseded, EventFXRateSetSuperseded}

func fxObservationPayload(o domain.FXObservation) map[string]any {
	return map[string]any{
		"tenant_id": o.TenantID, "observation_id": o.ObservationID, "source_id": o.SourceID,
		"base_currency": o.BaseCurrency, "quote_currency": o.QuoteCurrency, "rate_type": o.RateType, "rate": o.Rate,
		"market_date": o.MarketDate, "observed_at": o.ObservedAt, "status": o.Status, "manual": o.Manual, "outlier": o.Outlier,
		"payload_ref": o.PayloadRef, "payload_hash": o.PayloadHash,
	}
}

// FXRateObservedRecord is fx.rate.observed.
func FXRateObservedRecord(o domain.FXObservation, corr string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType: EventFXRateObserved, TenantID: o.TenantID, ActorID: o.IngestedBy, CorrelationID: corr,
		ObjectID: o.ObservationID, ObjectVersion: 1, PartitionKey: o.BaseCurrency + o.QuoteCurrency,
		EvidenceRef: o.PayloadRef, Payload: fxObservationPayload(o),
	})
}

// FXRateSupersededRecord is fx.rate.superseded: an observation was replaced, by another observation or by a
// superseding set. supersededBy is empty when the replacement is a set.
func FXRateSupersededRecord(o domain.FXObservation, supersededBy, reason, actor, corr string) (*outbox.Record, error) {
	p := fxObservationPayload(o)
	p["superseded_by_id"] = supersededBy
	p["reason"] = reason
	return BuildRecord(RecordSpec{
		EventType: EventFXRateSuperseded, TenantID: o.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: o.ObservationID, ObjectVersion: 2, PartitionKey: o.BaseCurrency + o.QuoteCurrency, Payload: p,
	})
}

func fxSetPayload(s domain.FXRateSet) map[string]any {
	members := make([]map[string]any, 0, len(s.Members))
	for _, m := range s.Members {
		members = append(members, map[string]any{"observation_id": m.ObservationID, "base_currency": m.BaseCurrency,
			"quote_currency": m.QuoteCurrency, "rate": m.Rate})
	}
	return map[string]any{
		"tenant_id": s.TenantID, "set_id": s.SetID, "set_name": s.SetName, "rate_type": s.RateType, "market_date": s.MarketDate,
		"set_version": s.SetVersion, "status": s.Status, "supersedes_set_id": s.SupersedesSetID, "members": members,
	}
}

// FXRateApprovedRecord is fx.rate.approved: a rate set was approved. A superseding set says which set it replaces, and
// a consumer that read the old one re-reads.
func FXRateApprovedRecord(s domain.FXRateSet, corr string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType: EventFXRateApproved, TenantID: s.TenantID, ActorID: s.ApprovedBy, CorrelationID: corr,
		ObjectID: s.SetID, ObjectVersion: int64(s.SetVersion), PartitionKey: string(s.RateType) + s.MarketDate,
		EvidenceRef: s.EvidenceRef, EffectiveAt: s.ApprovedAt, Payload: fxSetPayload(s),
	})
}

// FXRateSetClosedRecord is fx.rate_set.closed: the set is certified and immutable.
func FXRateSetClosedRecord(s domain.FXRateSet, corr string) (*outbox.Record, error) {
	spec := RecordSpec{
		EventType: EventFXRateSetClosed, TenantID: s.TenantID, ActorID: s.ClosedBy, CorrelationID: corr,
		ObjectID: s.SetID, ObjectVersion: int64(s.SetVersion), PartitionKey: string(s.RateType) + s.MarketDate,
		EvidenceRef: s.EvidenceRef, Payload: fxSetPayload(s),
	}
	if s.ClosedAt != nil {
		spec.EffectiveAt = *s.ClosedAt
	}
	return BuildRecord(spec)
}

// FXRateSetSupersededRecord is fx.rate_set.superseded: a correction replaced a set; the old set stays readable as
// of the time before it.
func FXRateSetSupersededRecord(old domain.FXRateSet, bySetID, actor, corr string) (*outbox.Record, error) {
	p := fxSetPayload(old)
	p["superseded_by_set_id"] = bySetID
	return BuildRecord(RecordSpec{
		EventType: EventFXRateSetSuperseded, TenantID: old.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: old.SetID, ObjectVersion: int64(old.SetVersion), PartitionKey: string(old.RateType) + old.MarketDate, Payload: p,
	})
}
