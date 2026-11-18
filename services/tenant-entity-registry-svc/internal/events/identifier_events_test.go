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

// party.identifier.verified carries the scheme, the source and the state move, never the identifier's value.
func TestPartyIdentifierVerifiedCarriesNoValue(t *testing.T) {
	p := domain.Party{PartyID: "0190a000-0000-7000-8000-00000000aaaa", TenantID: "0190a000-0000-7000-8000-0000000000aa", MasterVersion: 4,
		PartyKind: domain.PartyLegalEntity, Status: "ACTIVE", CreatedBy: "p", CreatedAt: time.Now()}
	i := domain.PartyIdentifier{IdentifierID: "0190a000-0000-7000-8000-00000000ffff", Scheme: domain.SchemeTaxID, Scope: "GB",
		Value: "SECRET-TAX-123", VerificationStatus: "VERIFIED"}
	o := domain.IdentifierObservation{ObservationID: "0190a000-0000-7000-8000-00000000cafe", Effect: domain.EffectSourceUpgraded,
		SourceClass: domain.SourceAuthorityRegistry, SourceRef: "HMRC", SourceVersion: "2026-06", EvidenceRef: "resp-1", ObservedAt: time.Now().UTC()}

	rec, err := events.PartyIdentifierVerifiedRecord(p, i, o, "p2", "corr")
	require.NoError(t, err)
	assert.Equal(t, events.EventPartyIdentifierVerified, rec.EventType)
	payload := string(rec.Payload)
	assert.False(t, strings.Contains(payload, "SECRET"), "the value must not be published: %s", payload)
	assert.Contains(t, payload, i.IdentifierID)
	assert.Contains(t, payload, `"scheme":"TAX_ID"`)
	assert.Contains(t, payload, `"effect":"SOURCE_UPGRADED"`)
	assert.Contains(t, payload, `"source_class":"AUTHORITY_REGISTRY"`)
	assert.Contains(t, payload, o.ObservationID)
}
