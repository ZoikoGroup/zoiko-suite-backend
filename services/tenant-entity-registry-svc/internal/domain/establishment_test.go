package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstablishmentLifecycle(t *testing.T) {
	allowed := map[[2]EstablishmentStatus]bool{
		{EstProposed, EstActive}: true, {EstProposed, EstClosed}: true,
		{EstActive, EstInactive}: true, {EstActive, EstClosed}: true,
		{EstInactive, EstActive}: true, {EstInactive, EstClosed}: true,
	}
	all := []EstablishmentStatus{EstProposed, EstActive, EstInactive, EstClosed}
	for _, from := range all {
		for _, to := range all {
			assert.Equal(t, allowed[[2]EstablishmentStatus{from, to}], from.CanMoveTo(to), "%s -> %s", from, to)
		}
	}
}

func TestEstablishmentMateriality(t *testing.T) {
	assert.True(t, EstBranch.Material())
	assert.True(t, EstPermanent.Material())
	for _, tp := range []EstablishmentType{EstPlaceOfBusiness, EstRepresentativeOffice, EstOperatingSite, EstEmploymentSite} {
		assert.False(t, tp.Material(), tp)
	}
	assert.False(t, EstablishmentType("MYSTERY").Valid())
}

func TestCreateEstablishmentValidate(t *testing.T) {
	ok := CreateEstablishmentRequest{LegalEntityID: "e", EstablishmentType: EstBranch, Name: "Leeds", AddressID: "a",
		SourceClass: SourceAuthorityDocument, EvidenceRef: "cert"}
	require.NoError(t, ok.Validate())
	mut := func(f func(*CreateEstablishmentRequest)) CreateEstablishmentRequest { r := ok; f(&r); return r }
	for name, r := range map[string]CreateEstablishmentRequest{
		"no entity":   mut(func(r *CreateEstablishmentRequest) { r.LegalEntityID = "" }),
		"bad type":    mut(func(r *CreateEstablishmentRequest) { r.EstablishmentType = "X" }),
		"no name":     mut(func(r *CreateEstablishmentRequest) { r.Name = " " }),
		"no address":  mut(func(r *CreateEstablishmentRequest) { r.AddressID = "" }),
		"no source":   mut(func(r *CreateEstablishmentRequest) { r.SourceClass = "" }),
		"no evidence": mut(func(r *CreateEstablishmentRequest) { r.EvidenceRef = "" }),
		"bad date":    mut(func(r *CreateEstablishmentRequest) { r.ValidFrom = "yesterday" }),
	} {
		assert.Error(t, r.Validate(), name)
	}
	same := ok
	same.CorrelationID, same.CreateRequestID = "c", "r"
	assert.Equal(t, ok.Fingerprint(), same.Fingerprint())
	other := ok
	other.Name = "York"
	assert.NotEqual(t, ok.Fingerprint(), other.Fingerprint())
}

func TestChangeEstablishmentStatusValidate(t *testing.T) {
	assert.NoError(t, ChangeEstablishmentStatusRequest{To: EstActive, EvidenceRef: "e"}.Validate())
	assert.Error(t, ChangeEstablishmentStatusRequest{To: EstProposed, EvidenceRef: "e"}.Validate(), "nothing moves back to PROPOSED")
	assert.Error(t, ChangeEstablishmentStatusRequest{To: "NOPE", EvidenceRef: "e"}.Validate())
	assert.Error(t, ChangeEstablishmentStatusRequest{To: EstActive}.Validate(), "evidence is required")
	assert.Error(t, ChangeEstablishmentStatusRequest{To: EstClosed, EvidenceRef: "e"}.Validate(), "closing needs a reason")
	assert.Error(t, ChangeEstablishmentStatusRequest{To: EstInactive, EvidenceRef: "e", Reason: "r", EffectiveDate: "x"}.Validate())
}

func TestStructuredOffice(t *testing.T) {
	f, ok := StructuredOffice(`{"lines":["1 High St"],"locality":"Leeds","country":"gb","postal_code":"LS1 1AA"}`)
	require.True(t, ok)
	assert.Equal(t, []string{"1 High St"}, f.Lines)
	assert.Equal(t, "GB", f.Country)

	f, ok = StructuredOffice(`{"Address_Line1":"1 High St","address_line2":"Floor 2","City":"Leeds","State":"West Yorkshire","Postcode":"LS1 1AA","Country_Code":"GB"}`)
	require.True(t, ok, "key names are matched leniently")
	assert.Equal(t, []string{"1 High St", "Floor 2"}, f.Lines)
	assert.Equal(t, "West Yorkshire", f.Subdivision)
	assert.Equal(t, "LS1 1AA", f.PostalCode)

	for name, raw := range map[string]string{
		"not json":           `Somewhere in Leeds`,
		"free text object":   `{"free_text":"Somewhere"}`,
		"no locality":        `{"lines":["1 High St"],"country":"GB"}`,
		"country not a code": `{"lines":["1 High St"],"locality":"Leeds","country":"United Kingdom"}`,
		"too many lines":     `{"lines":["a","b","c","d","e"],"locality":"Leeds","country":"GB"}`,
		"no lines":           `{"locality":"Leeds","country":"GB"}`,
		"array":              `["x"]`,
	} {
		_, ok := StructuredOffice(raw)
		assert.False(t, ok, name)
	}
}
