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

// A contact point event carries identifiers, the channel, the status and the digest of the address. The address
// itself, in any case or encoding the value took, is never in the payload or the record.
func TestContactPointEventsNeverCarryTheValue(t *testing.T) {
	const value = "Secret.Person@Example.com"
	c := domain.ContactPoint{ContactPointID: "0190a000-0000-7000-8000-00000000c0c0", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		PartyID: "0190a000-0000-7000-8000-00000000a0a0", Channel: domain.ContactEmail, Value: value, ValueDigest: domain.ContactDigest(value),
		Purpose: domain.ContactBilling, Status: domain.ContactVerified, SourceClass: domain.SourceTenantAsserted, ValidFrom: time.Now().UTC(),
		EvidenceRef: "ev", CreatedBy: "p", RecordVersion: 2, CreatedAt: time.Now()}

	for want, name := range map[string]string{
		events.EventContactPointProposed: "party.contact_point.proposed",
		events.EventContactPointVerified: "party.contact_point.verified",
		events.EventContactPointRetired:  "party.contact_point.retired",
	} {
		assert.Equal(t, name, want)
		rec, err := events.ContactPointRecord(want, c, "p2", "corr")
		require.NoError(t, err)
		assert.Equal(t, want, rec.EventType)
		all := strings.ToLower(string(rec.Payload) + rec.EventType + rec.PartitionKey)
		for _, leak := range []string{"secret", "person", "example.com", "example"} {
			assert.NotContains(t, all, leak, "the address must not be published: %s", string(rec.Payload))
		}
		for _, field := range []string{c.TenantID, c.PartyID, c.ContactPointID, `"channel":"EMAIL"`, `"purpose":"BILLING"`, `"status":"VERIFIED"`, c.ValueDigest, `"valid_from"`, `"valid_to"`} {
			assert.Contains(t, string(rec.Payload), field)
		}
		assert.NotContains(t, string(rec.Payload), `"value":`)
	}
	assert.ElementsMatch(t, []string{"party.contact_point.proposed", "party.contact_point.verified", "party.contact_point.retired"}, events.ContactPointEventNames)
}
