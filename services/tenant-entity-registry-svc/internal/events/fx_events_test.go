package events_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

// An exchange-rate event carries the rate itself, as an exact decimal string, so a consumer need not call back, and
// the reference and hash of the raw payload, never the payload.
func TestFXEvents(t *testing.T) {
	now := time.Now().UTC()
	o := domain.FXObservation{ObservationID: "0190a000-0000-7000-8000-0000000fa002", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		SourceID: "0190a000-0000-7000-8000-0000000fa001", BaseCurrency: "EUR", QuoteCurrency: "USD", RateType: domain.FXClosing, Rate: "1.085",
		MarketDate: "2026-06-12", ObservedAt: now, PayloadRef: "s3://rates/p1", PayloadHash: strings.Repeat("a", 64), Status: domain.FXValidated, IngestedBy: "p"}

	obs, err := events.FXRateObservedRecord(o, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventFXRateObserved, obs.EventType)
	p := string(obs.Payload)
	assert.Contains(t, p, `"rate":"1.085"`)
	assert.Contains(t, p, `"rate_type":"CLOSING"`)
	assert.Contains(t, p, `"payload_ref":"s3://rates/p1"`)

	sup, err := events.FXRateSupersededRecord(o, "0190a000-0000-7000-8000-0000000fa009", "re-issued", "p2", "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventFXRateSuperseded, sup.EventType)
	assert.Contains(t, string(sup.Payload), "re-issued")

	closedAt := now
	set := domain.FXRateSet{SetID: "0190a000-0000-7000-8000-0000000fa003", TenantID: o.TenantID, SetName: "DEFAULT", RateType: domain.FXClosing,
		MarketDate: "2026-06-12", SetVersion: 2, Status: domain.FXSetClosed, SupersedesSetID: "old", EvidenceRef: "pack", ApprovedBy: "p", ApprovedAt: now,
		ClosedBy: "p3", ClosedAt: &closedAt, Members: []domain.FXObservation{o}}
	for name, build := range map[string]func() (string, string, error){
		"approved": func() (string, string, error) {
			r, err := events.FXRateApprovedRecord(set, "c")
			return r.EventType, string(r.Payload), err
		},
		"closed": func() (string, string, error) {
			r, err := events.FXRateSetClosedRecord(set, "c")
			return r.EventType, string(r.Payload), err
		},
		"superseded": func() (string, string, error) {
			r, err := events.FXRateSetSupersededRecord(set, "new", "p", "c")
			return r.EventType, string(r.Payload), err
		},
	} {
		typ, payload, err := build()
		require.NoError(t, err, name)
		assert.Contains(t, []string{events.EventFXRateApproved, events.EventFXRateSetClosed, events.EventFXRateSetSuperseded}, typ, name)
		assert.Contains(t, payload, `"set_version":2`, name)
		assert.Contains(t, payload, `"rate":"1.085"`, name+": the set's members carry their rates")
		assert.Contains(t, payload, o.ObservationID, name)
	}
	assert.Len(t, events.FXEventNames, 5)
}
