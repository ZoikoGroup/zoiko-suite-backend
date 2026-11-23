package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validRegistration() CreateRegistrationRequest {
	return CreateRegistrationRequest{RegistrationType: RegLicense, IssuingAuthority: "FCA", Jurisdiction: "GB", Number: "FRN 123456",
		HolderLegalEntityID: "e", EffectiveFrom: "2025-01-01", ExpiryDate: "2027-01-01", SourceClass: SourceAuthorityDocument, EvidenceRef: "cert"}
}

func TestRegistrationTypes(t *testing.T) {
	for _, tp := range []RegistrationType{RegPermit, RegLicense, RegRegulated} {
		assert.True(t, tp.Material(), tp)
	}
	for _, tp := range []RegistrationType{RegCompany, RegTaxReference, RegEmployer, RegOtherGoverned} {
		assert.False(t, tp.Material(), tp)
	}
	assert.True(t, RegTaxReference.Sensitive())
	assert.True(t, RegEmployer.Sensitive())
	assert.False(t, RegCompany.Sensitive())
	assert.False(t, RegistrationType("MYSTERY").Valid())
	assert.True(t, RegDraft.Live() && RegActive.Live() && RegSuspended.Live())
	assert.False(t, RegExpired.Live() || RegRevoked.Live() || RegSuperseded.Live(), "an ended registration frees its number")
}

func TestEvaluateInForceAndLapsed(t *testing.T) {
	asOf := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		status          RegistrationStatus
		from, expiry    string
		inForce, lapsed bool
	}{
		"active and current":                {RegActive, "2025-01-01", "2027-01-01", true, false},
		"active, no expiry":                 {RegActive, "2025-01-01", "", true, false},
		"active on its last day":            {RegActive, "2025-01-01", "2026-06-15", true, false},
		"active but past expiry, unswept":   {RegActive, "2025-01-01", "2026-06-14", false, true},
		"active but not yet effective":      {RegActive, "2026-07-01", "2027-01-01", false, false},
		"a draft is never in force":         {RegDraft, "2025-01-01", "2027-01-01", false, false},
		"suspended is not in force":         {RegSuspended, "2025-01-01", "2027-01-01", false, false},
		"suspended past expiry is lapsed":   {RegSuspended, "2025-01-01", "2026-01-01", false, true},
		"expired is not in force or lapsed": {RegExpired, "2025-01-01", "2026-01-01", false, false},
		"revoked":                           {RegRevoked, "2025-01-01", "2027-01-01", false, false},
	} {
		r := Registration{Status: tc.status, EffectiveFrom: tc.from, ExpiryDate: tc.expiry}
		r.Evaluate(asOf)
		assert.Equal(t, tc.inForce, r.InForce, name)
		assert.Equal(t, tc.lapsed, r.Lapsed, name)
	}
}

func TestMaskNumberAndNormalization(t *testing.T) {
	assert.Equal(t, "********3456", MaskNumber("GB1234563456"))
	assert.Equal(t, "***", MaskNumber("abc"))
	assert.Equal(t, "", MaskNumber(""))
	assert.Equal(t, "GB123456", NormalizeRegistrationNumber("gb 123-456"))
	assert.Equal(t, "MULLER1", NormalizeRegistrationNumber("Müller-1"))
	assert.Equal(t, "", NormalizeRegistrationNumber(" - . "))
	assert.Equal(t, NormalizeAuthority("Companies House"), NormalizeAuthority("COMPANIES  HOUSE."))
	assert.Equal(t, "HMRC", NormalizeAuthority("H.M.R.C."))
}

func TestCreateRegistrationValidate(t *testing.T) {
	require.NoError(t, validRegistration().Validate())
	mut := func(f func(*CreateRegistrationRequest)) CreateRegistrationRequest {
		r := validRegistration()
		f(&r)
		return r
	}
	for name, r := range map[string]CreateRegistrationRequest{
		"bad type":        mut(func(r *CreateRegistrationRequest) { r.RegistrationType = "X" }),
		"no authority":    mut(func(r *CreateRegistrationRequest) { r.IssuingAuthority = " " }),
		"no jurisdiction": mut(func(r *CreateRegistrationRequest) { r.Jurisdiction = "" }),
		"no number":       mut(func(r *CreateRegistrationRequest) { r.Number = " - " }),
		"long number":     mut(func(r *CreateRegistrationRequest) { r.Number = strings.Repeat("1", 101) }),
		"no holder":       mut(func(r *CreateRegistrationRequest) { r.HolderLegalEntityID = "" }),
		"two holders":     mut(func(r *CreateRegistrationRequest) { r.HolderPartyID = "p" }),
		"no source":       mut(func(r *CreateRegistrationRequest) { r.SourceClass = "" }),
		"no evidence":     mut(func(r *CreateRegistrationRequest) { r.EvidenceRef = "" }),
		"bad hash":        mut(func(r *CreateRegistrationRequest) { r.EvidenceHash = "XYZ" }),
		"bad date":        mut(func(r *CreateRegistrationRequest) { r.EffectiveFrom = "x" }),
		"expiry before":   mut(func(r *CreateRegistrationRequest) { r.ExpiryDate = "2024-01-01" }),
		"issue after":     mut(func(r *CreateRegistrationRequest) { r.IssueDate = "2025-06-01" }),
	} {
		assert.Error(t, r.Validate(), name)
	}
	ok := validRegistration()
	ok.EvidenceHash = strings.Repeat("a", 64)
	assert.NoError(t, ok.Validate())
	ok.HolderLegalEntityID, ok.HolderPartyID = "", "p"
	assert.NoError(t, ok.Validate())

	base := validRegistration()
	same := base
	same.CorrelationID, same.CreateRequestID = "c", "r"
	assert.Equal(t, base.Fingerprint(), same.Fingerprint())
	spaced := base
	spaced.Number, spaced.IssuingAuthority = "frn123456", "f.c.a."
	assert.Equal(t, base.Fingerprint(), spaced.Fingerprint(), "the fingerprint is over the normalized identity")
	other := base
	other.Jurisdiction = "US"
	assert.NotEqual(t, base.Fingerprint(), other.Fingerprint())
}

func TestCommandRequestsValidate(t *testing.T) {
	assert.NoError(t, VerifyRegistrationRequest{EvidenceRef: "e"}.Validate())
	assert.Error(t, VerifyRegistrationRequest{}.Validate(), "a registration is verified from evidence")
	assert.Error(t, VerifyRegistrationRequest{EvidenceRef: "e", EvidenceHash: "zz"}.Validate())

	assert.NoError(t, RenewRegistrationRequest{EvidenceRef: "e", NewExpiryDate: "2030-01-01"}.Validate())
	assert.Error(t, RenewRegistrationRequest{NewExpiryDate: "2030-01-01"}.Validate())
	assert.Error(t, RenewRegistrationRequest{EvidenceRef: "e"}.Validate())
	assert.Error(t, RenewRegistrationRequest{EvidenceRef: "e", NewExpiryDate: "2030-01-01", EffectiveFrom: "x"}.Validate())

	assert.NoError(t, RegistrationEndRequest{Reason: "r", EvidenceRef: "e"}.Validate())
	assert.Error(t, RegistrationEndRequest{EvidenceRef: "e"}.Validate())
	assert.Error(t, RegistrationEndRequest{Reason: "r"}.Validate())
	assert.Error(t, RegistrationEndRequest{Reason: "r", EvidenceRef: "e", EffectiveDate: "x"}.Validate())
}
