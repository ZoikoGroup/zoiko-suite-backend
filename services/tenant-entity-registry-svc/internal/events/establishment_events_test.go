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

// An establishment event carries identifiers and the status move, never the name, and never a tax or registration
// conclusion: those belong to the domains that consume the fact.
func TestEstablishmentEvents(t *testing.T) {
	e := domain.Establishment{EstablishmentID: "0190a000-0000-7000-8000-00000000cccc", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		LegalEntityID: "0190a000-0000-7000-8000-00000000dddd", EstablishmentType: domain.EstBranch, Name: "SECRET BRANCH NAME",
		AddressID: "0190a000-0000-7000-8000-00000000bbbb", Status: domain.EstActive, Material: true, ValidFrom: "2025-01-01",
		SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "ev", RecordVersion: 2, CreatedBy: "p", CreatedAt: time.Now()}

	created, err := events.EstablishmentCreatedRecord(e, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventEstablishmentCreated, created.EventType)

	for to, want := range map[domain.EstablishmentStatus]string{
		domain.EstActive: events.EventEstablishmentActivated, domain.EstInactive: events.EventEstablishmentDeactivated, domain.EstClosed: events.EventEstablishmentClosed,
	} {
		rec, err := events.EstablishmentStatusRecord(e, domain.EstablishmentTransition{From: domain.EstProposed, To: to, EffectiveDate: "2025-02-01",
			Command: "X", EvidenceRef: "ev", Actor: "p2"}, "c")
		require.NoError(t, err)
		assert.Equal(t, want, rec.EventType)
		p := string(rec.Payload)
		assert.False(t, strings.Contains(p, "SECRET"), "the name must not be published: %s", p)
		assert.Contains(t, p, e.EstablishmentID)
		assert.Contains(t, p, `"effective_date":"2025-02-01"`)
		assert.Contains(t, p, `"material":true`)
	}
	assert.Empty(t, events.EstablishmentStatusEvent(domain.EstProposed))
	assert.False(t, strings.Contains(string(created.Payload), "SECRET"))
	assert.ElementsMatch(t, []string{"establishment.created", "establishment.activated", "establishment.deactivated", "establishment.closed"}, events.EstablishmentEventNames)
}
