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

// An address event carries identifiers and states, never the address: an address of a natural person is personal
// data and an event reaches every subscriber.
func TestAddressEvents_CarryNoAddressContent(t *testing.T) {
	a := domain.Address{AddressID: "0190a000-0000-7000-8000-00000000bbbb", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		AddressType: domain.AddressRegistered, CurrentVersion: 2, CreatedBy: "p", CreatedAt: time.Now()}
	v := domain.AddressVersion{VersionNo: 2, Lines: []string{"SECRET LINE 1"}, Locality: "SECRETVILLE", PostalCode: "ZZ9 9ZZ",
		Country: "GB", Formatted: "SECRET FORMATTED", Normalized: "SECRET NORMALIZED", ValidFrom: "2026-01-01",
		SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "ev-1"}
	old := domain.AddressVersion{VersionNo: 1}
	obs := domain.AddressObservation{Verifier: "LOCAL_RULES", Result: domain.VerifyPassed, Confidence: 0.9, NormalizedSeen: "SECRET SEEN"}
	l := domain.AddressLink{LinkID: "l1", PartyID: "p1", LinkType: domain.AddressRegistered, ValidFrom: "2026-01-01", EvidenceRef: "ev-2"}

	created, err := events.AddressCreatedRecord(a, v, "c")
	require.NoError(t, err)
	verified, err := events.AddressVerifiedRecord(a, v, obs, "p", "c")
	require.NoError(t, err)
	superseded, err := events.AddressSupersededRecord(a, old, v, "p", "c")
	require.NoError(t, err)
	linked, err := events.AddressLinkedRecord(a, v, l, "p", "c")
	require.NoError(t, err)

	assert.Equal(t, events.EventAddressCreated, created.EventType)
	assert.Equal(t, events.EventAddressVerified, verified.EventType)
	assert.Equal(t, events.EventAddressSuperseded, superseded.EventType)
	assert.Equal(t, events.EventAddressLinked, linked.EventType)
	for name, payload := range map[string]string{
		"created": string(created.Payload), "verified": string(verified.Payload),
		"superseded": string(superseded.Payload), "linked": string(linked.Payload),
	} {
		for _, secret := range []string{"SECRET", "ZZ9", "SECRETVILLE"} {
			assert.False(t, strings.Contains(payload, secret), "%s event leaked %q: %s", name, secret, payload)
		}
		assert.Contains(t, payload, a.AddressID, name)
	}
	assert.Contains(t, string(superseded.Payload), `"superseded_version_no":1`)
	assert.Contains(t, string(verified.Payload), `"result":"PASSED"`)
	assert.Contains(t, string(linked.Payload), `"link_type":"REGISTERED"`)
}

func TestAddressEventNamesMatchTheConstants(t *testing.T) {
	assert.ElementsMatch(t, []string{"address.created", "address.verified", "address.superseded", "address.linked"}, events.AddressEventNames)
}
