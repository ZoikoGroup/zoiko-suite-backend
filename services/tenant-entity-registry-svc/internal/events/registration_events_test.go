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

// A registration event carries identifiers, the type, jurisdiction, status move and dates, never the number: a tax or
// payroll registration number is sensitive and an event reaches every subscriber.
func TestRegistrationEvents(t *testing.T) {
	r := domain.Registration{RegistrationID: "0190a000-0000-7000-8000-00000000eeee", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		RegistrationType: domain.RegTaxReference, IssuingAuthority: "SECRET AUTHORITY", Jurisdiction: "GB", Number: "SECRET-NUMBER-123",
		HolderLegalEntityID: "0190a000-0000-7000-8000-00000000dddd", Status: domain.RegActive, Material: false, EffectiveFrom: "2025-01-01",
		ExpiryDate: "2027-01-01", SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "ev", RecordVersion: 3, CreatedBy: "p", CreatedAt: time.Now()}

	created, err := events.RegistrationCreatedRecord(r, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventRegistrationCreated, created.EventType)

	for _, tc := range []struct {
		command string
		to      domain.RegistrationStatus
		want    string
	}{
		{"VerifyRegistration", domain.RegActive, events.EventRegistrationVerified},
		{"RenewRegistration", domain.RegActive, events.EventRegistrationRenewed},
		{"RenewRegistration", domain.RegExpired, events.EventRegistrationRenewed},
		{"SuspendRegistration", domain.RegSuspended, events.EventRegistrationSuspended},
		{"ExpireRegistration", domain.RegExpired, events.EventRegistrationExpired},
		{"RevokeRegistration", domain.RegRevoked, events.EventRegistrationRevoked},
		{"SupersedeRegistration", domain.RegSuperseded, events.EventRegistrationSuperseded},
	} {
		rec, err := events.RegistrationStatusRecord(r, domain.RegistrationTransition{Command: tc.command, From: domain.RegDraft, To: tc.to,
			EffectiveDate: "2025-02-01", EvidenceRef: "ev", Actor: "p2"}, "c")
		require.NoError(t, err, tc.command)
		assert.Equal(t, tc.want, rec.EventType, tc.command)
		p := string(rec.Payload)
		assert.False(t, strings.Contains(p, "SECRET"), "the number and authority must not be published: %s", p)
		assert.Contains(t, p, r.RegistrationID)
		assert.Contains(t, p, `"registration_type":"TAX_REFERENCE"`)
		assert.Contains(t, p, `"effective_date":"2025-02-01"`)
	}
	assert.Empty(t, events.RegistrationStatusEvent("X", domain.RegDraft))

	exp, err := events.RegistrationExpiringRecord(r, 12, "system:expiry-sweep")
	require.NoError(t, err)
	assert.Equal(t, events.EventRegistrationExpiring, exp.EventType)
	assert.Contains(t, string(exp.Payload), `"days_left":12`)
	assert.Contains(t, string(exp.Payload), `"expiry_date":"2027-01-01"`)
	assert.False(t, strings.Contains(string(exp.Payload), "SECRET"))
	assert.False(t, strings.Contains(string(created.Payload), "SECRET"))
	assert.Len(t, events.RegistrationEventNames, 8)
}
