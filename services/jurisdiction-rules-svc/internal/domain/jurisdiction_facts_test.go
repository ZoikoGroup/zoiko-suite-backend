package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var twoJur = []Assignment{asg("j1", "GB", "PRIMARY", "e1"), asg("j2", "FR", "SECONDARY", "e2")}

func estF(id, ctry, status, eff string, v int64) FactRow {
	return FactRow{EventID: "ev-" + id + eff, FactKind: FactEstablishment, FactID: id, Subtype: "BRANCH", AddressID: "a-" + id, AddressCtry: ctry, Status: status, EffectiveDate: eff, ObjectVersion: v}
}

func regF(id, jur, status, eff, exp string, v int64) FactRow {
	return FactRow{EventID: "ev-" + id + eff, FactKind: FactRegistration, FactID: id, Subtype: "VAT", Jurisdiction: jur, Status: status, EffectiveDate: eff, ExpiryDate: exp, ObjectVersion: v}
}

func jfacts(rows ...FactRow) []FactView { return CurrentFacts(rows, "2026-08-01") }

func TestCurrentFacts_ReconstructsTheStateOnTheDate(t *testing.T) {
	rows := []FactRow{estF("e1", "gb", "ACTIVE", "2026-01-01", 2), estF("e1", "gb", "INACTIVE", "2026-09-01", 3),
		regF("r1", "fr", "ACTIVE", "2026-02-01", "2026-07-31", 2), regF("r2", "fr", "SUSPENDED", "2026-03-01", "", 3)}
	v := CurrentFacts(rows, "2026-08-01")
	require.Len(t, v, 3)
	byID := map[string]FactView{}
	for _, f := range v {
		byID[f.ID] = f
	}
	assert.True(t, byID["e1"].InForce, "a deactivation after the date is not read back into it")
	assert.Equal(t, "GB", byID["e1"].Code)
	assert.False(t, byID["r1"].InForce)
	assert.Equal(t, "expired on 2026-07-31", byID["r1"].Reason)
	assert.False(t, byID["r2"].InForce)
	assert.Equal(t, "status is SUSPENDED", byID["r2"].Reason)

	// The expiry day itself is still in force, as the registry reads it.
	assert.True(t, CurrentFacts(rows, "2026-07-31")[1].InForce || CurrentFacts(rows, "2026-07-31")[0].InForce)
	// Before anything was recorded there is nothing.
	assert.Empty(t, CurrentFacts(rows, "2025-12-31"))
}

func TestDecide_NoFactsNeededWhenOneJurisdictionIsAuthorised(t *testing.T) {
	d := DecideJurisdiction(twoJur[:1], PurposeTax, "", decAt, nil, FactInputs{})
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Empty(t, d.MissingFacts)
}

func TestDecide_SeveralAuthorisedAndNothingSuppliedNamesTheMissingFacts(t *testing.T) {
	f := jfacts(estF("e1", "FR", "ACTIVE", "2026-01-01", 1), regF("r1", "GB", "ACTIVE", "2026-01-01", "", 1), estF("e2", "US", "ACTIVE", "2026-01-01", 1))
	d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, FactInputs{})
	assert.Equal(t, OutcomeInsufficientFacts, d.Outcome)
	assert.Nil(t, d.Jurisdiction, "never guessed, even though one candidate has a registration")
	var names []string
	for _, m := range d.MissingFacts {
		names = append(names, m.Fact)
	}
	assert.ElementsMatch(t, []string{"establishment_id", "registration_id", "claimed_jurisdiction"}, names)
	require.Len(t, d.Facts, 3, "every consulted fact is in the evidence chain")
	for _, e := range d.Facts {
		assert.False(t, e.Used)
		assert.True(t, len(e.RefDigest) > 20, "a digest stands in for the id")
	}

	// With no in-force fact in a candidate jurisdiction, only the claim could select.
	d = DecideJurisdiction(twoJur, PurposeTax, "", decAt, jfacts(estF("e2", "US", "ACTIVE", "2026-01-01", 1)), FactInputs{})
	require.Len(t, d.MissingFacts, 1)
	assert.Equal(t, "claimed_jurisdiction", d.MissingFacts[0].Fact)
}

func TestDecide_AnInForceEstablishmentOrRegistrationSelectsAmongTheAuthorised(t *testing.T) {
	f := jfacts(estF("e1", "fr", "ACTIVE", "2026-01-01", 1), regF("r1", "GB", "ACTIVE", "2026-01-01", "2027-01-01", 1))
	d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, FactInputs{EstablishmentID: "e1"})
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Equal(t, BasisFactsWithinAuthorised, d.Basis)
	assert.Equal(t, "FR", d.Jurisdiction.JurisdictionCode)

	d = DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, FactInputs{RegistrationID: "r1"})
	assert.Equal(t, "GB", d.Jurisdiction.JurisdictionCode)
	used := 0
	for _, e := range d.Facts {
		if e.Used {
			used++
		}
	}
	assert.Equal(t, 1, used)

	// A claim and a fact that agree, together.
	d = DecideJurisdiction(twoJur, PurposeTax, "GB", decAt, f, FactInputs{RegistrationID: "r1"})
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Equal(t, "GB", d.Jurisdiction.JurisdictionCode)
}

func TestDecide_FactsThatDisagreeAreAConflict(t *testing.T) {
	f := jfacts(estF("e1", "FR", "ACTIVE", "2026-01-01", 1), regF("r1", "GB", "ACTIVE", "2026-01-01", "", 1))
	d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, FactInputs{EstablishmentID: "e1", RegistrationID: "r1"})
	assert.Equal(t, OutcomeFactsConflict, d.Outcome)
	assert.Nil(t, d.Jurisdiction)
	// The claim counts as a selecting fact too.
	d = DecideJurisdiction(twoJur, PurposeTax, "GB", decAt, f, FactInputs{EstablishmentID: "e1"})
	assert.Equal(t, OutcomeFactsConflict, d.Outcome)
}

func TestDecide_AFactThatCannotBeUsedIsNamedAndNeverReplaced(t *testing.T) {
	f := jfacts(estF("e1", "FR", "INACTIVE", "2026-01-01", 1), estF("e2", "US", "ACTIVE", "2026-01-01", 1),
		estF("e3", "", "ACTIVE", "2026-01-01", 1), regF("r1", "FR", "ACTIVE", "2025-01-01", "2026-01-01", 1))
	for name, in := range map[string]FactInputs{
		"establishment_id": {EstablishmentID: "e1"}, "registration_id": {RegistrationID: "r1"},
	} {
		d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, in)
		assert.Equal(t, OutcomeFactNotUsable, d.Outcome, name)
		require.Len(t, d.MissingFacts, 1)
		assert.Equal(t, name, d.MissingFacts[0].Fact)
		assert.Contains(t, d.MissingFacts[0].Reason, "not in force")
		assert.Nil(t, d.Jurisdiction)
	}
	// Unknown id, an address country this service does not hold, and a jurisdiction the entity is not authorised for.
	for id, want := range map[string]string{"nope": "no such establishment", "e3": "not known to this service", "e2": "not one the entity is authorised for"} {
		d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, f, FactInputs{EstablishmentID: id})
		assert.Equal(t, OutcomeFactNotUsable, d.Outcome, id)
		assert.Contains(t, d.MissingFacts[0].Reason, want, id)
	}
	// Even when one jurisdiction is authorised, a named fact that cannot be used is not ignored.
	d := DecideJurisdiction(twoJur[:1], PurposeTax, "", decAt, f, FactInputs{EstablishmentID: "e1"})
	assert.Equal(t, OutcomeFactNotUsable, d.Outcome)
}

func TestDecide_TheClaimOutcomesAreUnchanged(t *testing.T) {
	d := DecideJurisdiction(twoJur[:1], PurposeTax, "FR", decAt, nil, FactInputs{})
	assert.Equal(t, OutcomeClaimNotAuthorised, d.Outcome)
	d = DecideJurisdiction(nil, PurposeTax, "", decAt, jfacts(estF("e1", "FR", "ACTIVE", "2026-01-01", 1)), FactInputs{EstablishmentID: "e1"})
	assert.Equal(t, OutcomeNoApplicable, d.Outcome, "a fact never creates an authorisation")
}

func TestDecide_CounterpartyAndTransactionFactsAreRecordedAndNeverDecide(t *testing.T) {
	in := FactInputs{Counterparty: &Counterparty{Country: "de", BusinessStatus: "B2B"}, Transaction: &Transaction{SupplyType: "GOODS"}}
	d := DecideJurisdiction(twoJur, PurposeTax, "", decAt, nil, in)
	assert.Equal(t, OutcomeInsufficientFacts, d.Outcome, "a counterparty in DE does not pick GB or FR")
	require.Len(t, d.Inputs, 3)
	assert.Equal(t, "DE", d.Inputs[0].Value)
	assert.Equal(t, "RECORDED_NOT_DECIDING", d.Inputs[0].Status)
	assert.True(t, ValidSupplyType("SERVICES") && !ValidSupplyType("x"))
	assert.True(t, ValidBusinessStatus("B2C") && !ValidBusinessStatus("x"))
}
