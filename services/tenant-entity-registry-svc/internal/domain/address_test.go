package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goodFields() AddressFields {
	return AddressFields{Lines: []string{"12 High Street"}, Locality: "London", PostalCode: "SW1A 1AA", Country: "gb",
		SourceClass: SourceTenantAsserted, EvidenceRef: "form"}
}

func TestNormalizeAddress(t *testing.T) {
	a := NormalizeAddress([]string{"12 High Street"}, "London", "", "sw1a 1aa", "gb")
	b := NormalizeAddress([]string{"12, High St."}, "LONDON", "", "SW1A-1AA", "GB")
	assert.Equal(t, a, b)
	assert.Equal(t, "12 HIGH ST|LONDON||SW1A1AA|GB", a)
	assert.Equal(t, "MULLER ST|KOLN||50667|DE", NormalizeAddress([]string{"Müller Street"}, "Köln", "", "50667", "DE"))
	assert.NotEqual(t, a, NormalizeAddress([]string{"13 High Street"}, "London", "", "SW1A 1AA", "GB"))
}

func TestAddressTypeValidity(t *testing.T) {
	for _, ok := range []AddressType{AddressRegistered, AddressBilling, AddressEmploymentEstab, "X_ARCHIVE", "X_A1"} {
		assert.True(t, ok.Valid(), ok)
	}
	for _, bad := range []AddressType{"", "registered", "HOME", "X_", "X_lower", "X_" + AddressType(make([]byte, 40))} {
		assert.False(t, bad.Valid(), string(bad))
	}
	assert.True(t, AddressRegistered.NeedsAuthority())
	assert.False(t, AddressBilling.NeedsAuthority())
}

func TestAddressFieldsValidate(t *testing.T) {
	require.NoError(t, goodFields().Validate())
	mut := func(f func(*AddressFields)) AddressFields { g := goodFields(); f(&g); return g }
	for name, f := range map[string]AddressFields{
		"no lines":        mut(func(a *AddressFields) { a.Lines = nil }),
		"five lines":      mut(func(a *AddressFields) { a.Lines = []string{"a", "b", "c", "d", "e"} }),
		"blank line":      mut(func(a *AddressFields) { a.Lines = []string{"  "} }),
		"no locality":     mut(func(a *AddressFields) { a.Locality = " " }),
		"country shape":   mut(func(a *AddressFields) { a.Country = "GBR" }),
		"no source":       mut(func(a *AddressFields) { a.SourceClass = "" }),
		"no evidence":     mut(func(a *AddressFields) { a.EvidenceRef = " " }),
		"bad date":        mut(func(a *AddressFields) { a.ValidFrom = "01/02/2025" }),
		"postal too long": mut(func(a *AddressFields) { a.PostalCode = "123456789012345678901" }),
	} {
		assert.Error(t, f.Validate(), name)
	}
	// An unknown but well-shaped country is a verification finding, not a refusal to record.
	assert.NoError(t, mut(func(a *AddressFields) { a.Country = "ZZ" }).Validate())
}

func TestCreateAddressRegisteredRule(t *testing.T) {
	r := CreateAddressRequest{AddressType: AddressRegistered, AddressFields: goodFields()}
	require.ErrorIs(t, r.Validate(), ErrRegisteredNeedsAuthority)
	r.SourceClass = SourceAuthorityDocument
	require.NoError(t, r.Validate())
	r.AddressType = AddressBilling
	r.SourceClass = SourceTenantAsserted
	require.NoError(t, r.Validate(), "only a registered office needs authority evidence")
}

func TestCreateAddressFingerprint(t *testing.T) {
	base := CreateAddressRequest{AddressType: AddressBilling, AddressFields: goodFields()}
	same := base
	same.CorrelationID = "other"
	same.CreateRequestID = "id"
	assert.Equal(t, base.Fingerprint(), same.Fingerprint(), "ids and correlation are not part of the content")
	other := base
	other.Locality = "York"
	assert.NotEqual(t, base.Fingerprint(), other.Fingerprint())
	typ := base
	typ.AddressType = AddressOperating
	assert.NotEqual(t, base.Fingerprint(), typ.Fingerprint())
}

func TestLinkAndAmendValidate(t *testing.T) {
	assert.Error(t, AmendAddressRequest{AddressFields: goodFields()}.Validate(), "reason is required")
	assert.NoError(t, AmendAddressRequest{Reason: "moved", AddressFields: goodFields()}.Validate())
	assert.Error(t, LinkAddressRequest{LinkType: AddressBilling, EvidenceRef: "e"}.Validate())
	assert.Error(t, LinkAddressRequest{AddressID: "a", LinkType: "nope", EvidenceRef: "e"}.Validate())
	assert.Error(t, LinkAddressRequest{AddressID: "a", LinkType: AddressBilling}.Validate())
	assert.NoError(t, LinkAddressRequest{AddressID: "a", LinkType: AddressBilling, EvidenceRef: "e", ValidFrom: "2025-01-01"}.Validate())
}
