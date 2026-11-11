//go:build integration

// JUR-1 end to end: establishments and registrations projected from registry events select among the authorised
// jurisdictions, a fact that cannot be used is named, and the evidence ledger holds no tenant object ids.
package registryit_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

func applyFact(t *testing.T, tenant, entity string, row domain.FactRow) {
	t.Helper()
	row.EventID = uuid.NewString()
	applied, err := st.ApplyRegistryFact(ctx, row.EventID, "test.fact", domain.RegistryFactEvent{TenantID: tenant, LegalEntityID: entity, Row: row})
	require.NoError(t, err)
	require.True(t, applied)
}

func applyAddress(t *testing.T, tenant, addr, country string, version int64) {
	t.Helper()
	_, err := st.ApplyAddressCountry(ctx, uuid.NewString(), "address.created", domain.AddressCountryEvent{TenantID: tenant, AddressID: addr, Country: country, Version: version})
	require.NoError(t, err)
}

func TestJurisdictionDecision_EstablishmentsAndRegistrationsSelectAndUnusableFactsAreNamed(t *testing.T) {
	e := newEnvDetermination()
	tenant, entity := "tenant-"+uuid.NewString()[:8], "entity-"+uuid.NewString()[:8]
	gb, fr := seedJurisdiction(t, "GB"), seedJurisdiction(t, "FR")
	start := time.Now().UTC().Add(-120 * time.Hour)
	assign(t, tenant, entity, gb, "PRIMARY", start, nil)
	assign(t, tenant, entity, fr, "SECONDARY", start, nil)
	day := func(d time.Duration) string { return time.Now().UTC().Add(d).Format("2006-01-02") }
	estID, regID, oldReg, noAddr := "est-"+uuid.NewString(), "reg-"+uuid.NewString(), "reg-"+uuid.NewString(), "est-"+uuid.NewString()
	addr := "addr-" + uuid.NewString()
	applyAddress(t, tenant, addr, fr.JurisdictionCode, 1)
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactEstablishment, FactID: estID, Subtype: "BRANCH", AddressID: addr, Status: "ACTIVE", EffectiveDate: day(-48 * time.Hour), ObjectVersion: 2})
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactEstablishment, FactID: noAddr, AddressID: "addr-missing", Status: "ACTIVE", EffectiveDate: day(-48 * time.Hour), ObjectVersion: 2})
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactRegistration, FactID: regID, Subtype: "VAT", Jurisdiction: gb.JurisdictionCode, Status: "ACTIVE", EffectiveDate: day(-48 * time.Hour), ExpiryDate: day(240 * time.Hour), ObjectVersion: 2})
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactRegistration, FactID: oldReg, Subtype: "VAT", Jurisdiction: gb.JurisdictionCode, Status: "ACTIVE", EffectiveDate: day(-480 * time.Hour), ExpiryDate: day(-240 * time.Hour), ObjectVersion: 2})
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"legal_entity_id": entity, "purpose": "TAX", "effective_at": time.Now().UTC().Format(time.RFC3339)}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	code := func(r map[string]any) string {
		return determination(r)["jurisdiction"].(map[string]any)["jurisdiction_code"].(string)
	}

	// Nothing named: 422, and the facts that would select are named, with the consulted facts as evidence.
	r := decide(t, e, tenant, body(nil), 422)
	d := determination(r)
	assert.Equal(t, "INSUFFICIENT_FACTS", d["outcome"])
	var names []string
	for _, m := range d["missing_facts"].([]any) {
		names = append(names, m.(map[string]any)["fact"].(string))
	}
	assert.ElementsMatch(t, []string{"establishment_id", "registration_id", "claimed_jurisdiction"}, names)
	assert.Len(t, d["facts"], 4)

	// The establishment (address in FR) and the registration (GB) each select, and used facts are marked.
	r = decide(t, e, tenant, body(map[string]any{"establishment_id": estID}), 200)
	assert.Equal(t, fr.JurisdictionCode, code(r))
	assert.Equal(t, "FACTS_WITHIN_AUTHORISED_SET", determination(r)["basis"])
	r = decide(t, e, tenant, body(map[string]any{"registration_id": regID, "counterparty": map[string]any{"country": "de", "business_status": "B2B"}, "transaction": map[string]any{"supply_type": "GOODS"}}), 200)
	assert.Equal(t, gb.JurisdictionCode, code(r))
	assert.Len(t, determination(r)["inputs"], 3, "counterparty and transaction facts are recorded and never decide")
	used := 0
	for _, f := range determination(r)["facts"].([]any) {
		if f.(map[string]any)["used"] == true {
			used++
		}
	}
	assert.Equal(t, 1, used)

	// Facts that disagree: 409. Unusable facts: 422 naming the fact.
	decide(t, e, tenant, body(map[string]any{"establishment_id": estID, "registration_id": regID}), 409)
	for id, field := range map[string]string{oldReg: "registration_id", noAddr: "establishment_id", "nope": "establishment_id"} {
		r = decide(t, e, tenant, body(map[string]any{field: id}), 422)
		assert.Equal(t, "FACT_NOT_USABLE", determination(r)["outcome"], id)
		assert.Equal(t, field, determination(r)["missing_facts"].([]any)[0].(map[string]any)["fact"])
	}

	// An establishment was not in force before it began, and a later deactivation does not rewrite the past.
	r = decide(t, e, tenant, body(map[string]any{"establishment_id": estID, "effective_at": time.Now().UTC().Add(-73 * time.Hour).Format(time.RFC3339)}), 422)
	assert.Equal(t, "FACT_NOT_USABLE", determination(r)["outcome"])
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactEstablishment, FactID: estID, AddressID: addr, Status: "INACTIVE", EffectiveDate: day(24 * time.Hour), ObjectVersion: 3})
	decide(t, e, tenant, body(map[string]any{"establishment_id": estID}), 200)
	r = decide(t, e, tenant, body(map[string]any{"establishment_id": estID, "effective_at": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}), 422)
	assert.Equal(t, "FACT_NOT_USABLE", determination(r)["outcome"])

	// A superseded address version carries no country: the establishment is unusable until a later version says otherwise.
	applyAddress(t, tenant, addr, "", 2)
	applyFact(t, tenant, entity, domain.FactRow{FactKind: domain.FactEstablishment, FactID: estID, AddressID: addr, Status: "ACTIVE", EffectiveDate: day(-time.Hour), ObjectVersion: 4})
	assert.Equal(t, "FACT_NOT_USABLE", determination(decide(t, e, tenant, body(map[string]any{"establishment_id": estID}), 422))["outcome"])
	applyAddress(t, tenant, addr, gb.JurisdictionCode, 3)
	assert.Equal(t, gb.JurisdictionCode, code(decide(t, e, tenant, body(map[string]any{"establishment_id": estID}), 200)))

	// Tenant isolation: another tenant cannot use this establishment.
	other := "tenant-" + uuid.NewString()[:8]
	assign(t, other, entity, gb, "PRIMARY", start, nil)
	assign(t, other, entity, fr, "SECONDARY", start, nil)
	assert.Equal(t, "FACT_NOT_USABLE", determination(decide(t, e, other, body(map[string]any{"establishment_id": estID}), 422))["outcome"])

	// Validation, and a redelivered fact is not applied twice.
	status, _ := e.doWithHeaders("POST", decisionPath, "tax-ops", body(map[string]any{"transaction": map[string]any{"supply_type": "LAND"}}), map[string]string{"X-Tenant-Id": tenant})
	assert.Equal(t, 400, status)
	id := uuid.NewString()
	row := domain.FactRow{EventID: id, FactKind: domain.FactRegistration, FactID: regID, Jurisdiction: "GB", Status: "ACTIVE", EffectiveDate: day(-time.Hour), ObjectVersion: 9}
	ok, err := st.ApplyRegistryFact(ctx, id, "registration.renewed", domain.RegistryFactEvent{TenantID: tenant, LegalEntityID: entity, Row: row})
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = st.ApplyRegistryFact(ctx, id, "registration.renewed", domain.RegistryFactEvent{TenantID: tenant, LegalEntityID: entity, Row: row})
	require.NoError(t, err)
	assert.False(t, ok)
}
