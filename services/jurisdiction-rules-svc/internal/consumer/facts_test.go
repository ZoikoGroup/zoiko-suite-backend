package consumer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

type fakeFacts struct {
	facts []domain.RegistryFactEvent
	addrs []domain.AddressCountryEvent
	err   error
}

func (f *fakeFacts) ApplyRegistryFact(_ context.Context, _, _ string, e domain.RegistryFactEvent) (bool, error) {
	f.facts = append(f.facts, e)
	return f.err == nil, f.err
}

func (f *fakeFacts) ApplyAddressCountry(_ context.Context, _, _ string, e domain.AddressCountryEvent) (bool, error) {
	f.addrs = append(f.addrs, e)
	return f.err == nil, f.err
}

func factConsumer() (*EntityConsumer, *fakeFacts) {
	f := &fakeFacts{}
	return NewEntityConsumer(zap.NewNop(), &fakeStore{}).WithFacts(f), f
}

func TestFactConsumer_ProjectsEstablishmentsRegistrationsAndAddressCountries(t *testing.T) {
	c, f := factConsumer()
	ctx := context.Background()
	require.NoError(t, c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"tenant_id": tenant, "establishment_id": "est-1", "legal_entity_id": entity, "establishment_type": "BRANCH",
		"address_id": "addr-1", "status": "ACTIVE", "effective_date": "2026-02-01"})))
	require.NoError(t, c.Handle(ctx, envelope(t, EventRegistrationRenewed, tenant, 3, map[string]any{
		"tenant_id": tenant, "registration_id": "reg-1", "registration_type": "VAT", "jurisdiction": "gb", "status": "ACTIVE",
		"expiry_date": "2027-03-31", "effective_date": "2026-03-01T10:00:00Z", "holder_legal_entity_id": entity})))
	require.NoError(t, c.Handle(ctx, envelope(t, EventAddressCreated, tenant, 1, map[string]any{"address_id": "addr-1", "country": "fr"})))
	require.NoError(t, c.Handle(ctx, envelope(t, EventAddressSuperseded, tenant, 2, map[string]any{"address_id": "addr-1"})))

	require.Len(t, f.facts, 2)
	assert.Equal(t, domain.FactEstablishment, f.facts[0].Row.FactKind)
	assert.Equal(t, entity, f.facts[0].LegalEntityID)
	assert.Equal(t, "2026-02-01", f.facts[0].Row.EffectiveDate)
	assert.Equal(t, "GB", f.facts[1].Row.Jurisdiction)
	assert.Equal(t, "2026-03-01", f.facts[1].Row.EffectiveDate, "an instant is reduced to its date")
	assert.Equal(t, "2027-03-31", f.facts[1].Row.ExpiryDate)
	require.Len(t, f.addrs, 2)
	assert.Equal(t, "FR", f.addrs[0].Country)
	assert.Equal(t, "", f.addrs[1].Country, "a superseded version does not say its country")
	assert.EqualValues(t, 2, f.addrs[1].Version)
}

func TestFactConsumer_SkipsWhatCannotBeAppliedAndRetriesOutages(t *testing.T) {
	c, f := factConsumer()
	ctx := context.Background()
	// A party-held registration is not a fact about an entity.
	require.NoError(t, c.Handle(ctx, envelope(t, EventRegistrationVerified, tenant, 2, map[string]any{
		"registration_id": "reg-2", "jurisdiction": "GB", "status": "ACTIVE", "effective_date": "2026-03-01", "holder_party_id": "p-1"})))
	// A malformed date, a missing status and a payload for another tenant are skipped, never retried.
	require.NoError(t, c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"establishment_id": "est-1", "status": "ACTIVE", "effective_date": "soon"})))
	require.NoError(t, c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"establishment_id": "est-1", "effective_date": "2026-02-01"})))
	require.NoError(t, c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"tenant_id": "other", "establishment_id": "est-1", "status": "ACTIVE", "effective_date": "2026-02-01"})))
	assert.Empty(t, f.facts)

	f.err = domain.ErrStoreUnavailable
	err := c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"establishment_id": "est-1", "status": "ACTIVE", "effective_date": "2026-02-01"}))
	assert.True(t, errors.Is(err, domain.ErrStoreUnavailable), "an outage is retried")
	f.err = domain.ErrInvalidEvent
	assert.NoError(t, c.Handle(ctx, envelope(t, EventEstablishmentActivated, tenant, 2, map[string]any{
		"establishment_id": "est-1", "status": "ACTIVE", "effective_date": "2026-02-01"})), "a store-rejected event is skipped")

	// Without the projection wired the events are ignored.
	plain := NewEntityConsumer(zap.NewNop(), &fakeStore{})
	assert.NoError(t, plain.Handle(ctx, envelope(t, EventAddressCreated, tenant, 1, map[string]any{"address_id": "a", "country": "FR"})))
}
