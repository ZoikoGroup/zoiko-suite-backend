package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func validCreate() domain.CreatePartyRequest {
	return domain.CreatePartyRequest{
		CreateRequestID: "req-1", PartyKind: domain.PartyLegalEntity,
		Names:       []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Nova Holdings Limited"}},
		Country:     "GB",
		SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "registry-extract-1", ValidFrom: "2025-01-01",
	}
}

func TestCreatePartyRequest_Validate(t *testing.T) {
	require.NoError(t, func() error { r := validCreate(); return r.Validate() }())

	for name, mutate := range map[string]func(*domain.CreatePartyRequest){
		"no request id":             func(r *domain.CreatePartyRequest) { r.CreateRequestID = " " },
		"bad kind":                  func(r *domain.CreatePartyRequest) { r.PartyKind = "COMPANY" },
		"no source class":           func(r *domain.CreatePartyRequest) { r.SourceClass = "" },
		"no evidence":               func(r *domain.CreatePartyRequest) { r.EvidenceRef = "" },
		"bad date":                  func(r *domain.CreatePartyRequest) { r.ValidFrom = "01/01/2025" },
		"no names":                  func(r *domain.CreatePartyRequest) { r.Names = nil },
		"blank name":                func(r *domain.CreatePartyRequest) { r.Names[0].Value = " " },
		"legal entity trading only": func(r *domain.CreatePartyRequest) { r.Names[0].Type = domain.FactTradingName },
		"two legal names": func(r *domain.CreatePartyRequest) {
			r.Names = append(r.Names, domain.PartyNameInput{Type: domain.FactLegalName, Value: "Other Ltd"})
		},
		"local script without language": func(r *domain.CreatePartyRequest) {
			r.Names = append(r.Names, domain.PartyNameInput{Type: domain.FactLocalScriptName, Value: "ノヴァ"})
		},
		"bad country":                   func(r *domain.CreatePartyRequest) { r.Country = "gb" },
		"authority without country":     func(r *domain.CreatePartyRequest) { r.PartyKind, r.Country = domain.PartyPublicAuthority, "" },
		"person linked to legal entity": func(r *domain.CreatePartyRequest) { r.PartyKind, r.LegalEntityID = domain.PartyNaturalPerson, "x" },
		"source system alone":           func(r *domain.CreatePartyRequest) { r.SourceSystem = "crm" },
		"unknown scheme": func(r *domain.CreatePartyRequest) {
			r.Identifiers = []domain.PartyIdentifierInput{{Scheme: "PASSPORT", Value: "1"}}
		},
		"source system identifier without system": func(r *domain.CreatePartyRequest) {
			r.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeSourceSystem, Value: "1"}}
		},
	} {
		r := validCreate()
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestFingerprint_IgnoresOrderAndCorrelationButNotContent(t *testing.T) {
	a := validCreate()
	a.Identifiers = []domain.PartyIdentifierInput{
		{Scheme: domain.SchemeVAT, Scope: "GB", Value: "GB 123 456"}, {Scheme: domain.SchemeLEI, Value: "529900"},
	}
	b := validCreate()
	b.CreateRequestID, b.CorrelationID = "req-2", "corr"
	b.Identifiers = []domain.PartyIdentifierInput{
		{Scheme: domain.SchemeLEI, Value: "529-900"}, {Scheme: domain.SchemeVAT, Scope: "GB", Value: "gb123456"},
	}
	assert.Equal(t, a.Fingerprint(), b.Fingerprint(), "the same request, written differently, is the same request")

	b.Names[0].Value = "Nova Holdings Plc"
	assert.NotEqual(t, a.Fingerprint(), b.Fingerprint())
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Nova Holdings Limited": "nova holdings",
		"NOVA  HOLDINGS, Ltd.":  "nova holdings",
		"Müller & Söhne GmbH":   "muller sohne",
		"Limited":               "limited",
		"  Café du Monde S.A. ": "cafe du monde",
	} {
		assert.Equal(t, want, domain.NormalizeName(in), in)
	}
	assert.Equal(t, "GB123456789", domain.NormalizeIdentifier("gb 123-456.789"))
}

func TestMaskIdentifier(t *testing.T) {
	assert.Equal(t, "******6789", domain.MaskIdentifier(domain.SchemeTaxID, "1234566789"))
	assert.Equal(t, "*****", domain.MaskIdentifier(domain.SchemeNationalID, "12345"), "a short value is hidden entirely")
	assert.Equal(t, "GB123", domain.MaskIdentifier(domain.SchemeVAT, "GB123"), "a public scheme is shown")
}

func known(id string, kind domain.PartyKind, country string, names []string, ids ...domain.KnownIdentifier) domain.KnownParty {
	return domain.KnownParty{PartyID: id, Kind: kind, Country: country, Names: names, Identifiers: ids}
}

func TestClassifyMatch_Ladder(t *testing.T) {
	reg := domain.KnownIdentifier{Scheme: domain.SchemeCompanyRegistration, Scope: "GB", Value: "12345678", Verified: true}
	unverified := domain.KnownIdentifier{Scheme: domain.SchemeVAT, Scope: "GB", Value: "GB1", Verified: false}
	nova := known("P-1", domain.PartyLegalEntity, "GB", []string{"nova holdings"}, reg, unverified)
	other := known("P-2", domain.PartyLegalEntity, "GB", []string{"zenith traders"},
		domain.KnownIdentifier{Scheme: domain.SchemeLEI, Value: "LEI2", Verified: true})

	t.Run("tier A: a verified identifier links to its one party", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Names: []string{"something else"},
			Identifiers: []domain.KnownIdentifier{reg}}, []domain.KnownParty{nova, other})
		require.NotNil(t, d.SafeLink)
		assert.Equal(t, "P-1", d.SafeLink.PartyID)
		assert.Equal(t, domain.TierA, d.SafeLink.Tier)
		assert.False(t, d.Review)
	})

	t.Run("tier D: identifiers that point at different parties never link", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Identifiers: []domain.KnownIdentifier{reg,
			{Scheme: domain.SchemeLEI, Value: "LEI2"}}}, []domain.KnownParty{nova, other})
		assert.Nil(t, d.SafeLink)
		assert.True(t, d.Review)
		assert.Equal(t, domain.CaseIdentifierConflict, d.CaseType)
		assert.Len(t, d.Candidates, 2)
		for _, c := range d.Candidates {
			assert.Equal(t, domain.TierD, c.Tier)
		}
	})

	t.Run("tier D: the identifier belongs to a different kind of party", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyNaturalPerson, Identifiers: []domain.KnownIdentifier{reg}},
			[]domain.KnownParty{nova})
		assert.Nil(t, d.SafeLink)
		assert.True(t, d.Review)
		assert.Equal(t, domain.TierD, d.Candidates[0].Tier)
	})

	t.Run("an identifier the tenant only asserted is evidence, not proof", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Identifiers: []domain.KnownIdentifier{unverified}},
			[]domain.KnownParty{nova})
		assert.Nil(t, d.SafeLink, "no automatic link on an unverified identifier")
		assert.True(t, d.Review)
	})

	t.Run("tier B: same name and country is a candidate, never an automatic link", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Country: "GB", Names: []string{"nova holdings"}},
			[]domain.KnownParty{nova})
		assert.Nil(t, d.SafeLink)
		assert.True(t, d.Review)
		assert.Equal(t, domain.TierB, d.Candidates[0].Tier)
		assert.Equal(t, domain.CaseDuplicateReview, d.CaseType)
	})

	t.Run("tier C: the same name in another country, and near names, need review", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Country: "IE", Names: []string{"nova holdings"}},
			[]domain.KnownParty{nova})
		require.True(t, d.Review)
		assert.Equal(t, domain.TierC, d.Candidates[0].Tier)

		d = domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Country: "GB", Names: []string{"nova holding"}},
			[]domain.KnownParty{nova})
		require.True(t, d.Review)
		assert.Equal(t, domain.TierC, d.Candidates[0].Tier)
	})

	t.Run("a different kind of party is never a name candidate", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyNaturalPerson, Names: []string{"nova holdings"}},
			[]domain.KnownParty{nova})
		assert.False(t, d.Review)
		assert.Nil(t, d.SafeLink)
	})

	t.Run("no resemblance: nothing to review, nothing to link", func(t *testing.T) {
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Country: "GB", Names: []string{"quantum freight"}},
			[]domain.KnownParty{nova, other})
		assert.False(t, d.Review)
		assert.Nil(t, d.SafeLink)
		assert.Empty(t, d.Candidates)
	})

	t.Run("source system identifier is scheme-proven", func(t *testing.T) {
		src := domain.KnownIdentifier{Scheme: domain.SchemeSourceSystem, Scope: "crm", Value: "C42"}
		d := domain.ClassifyMatch(domain.MatchRequest{Kind: domain.PartyLegalEntity, Identifiers: []domain.KnownIdentifier{src}},
			[]domain.KnownParty{known("P-9", domain.PartyLegalEntity, "GB", nil, src)})
		require.NotNil(t, d.SafeLink)
		assert.Equal(t, "P-9", d.SafeLink.PartyID, "a replayed ingest does not create a second party")
	})
}

func TestNameSimilarity(t *testing.T) {
	assert.Equal(t, 1.0, domain.NameSimilarity("nova", "nova"))
	assert.Equal(t, 0.0, domain.NameSimilarity("", "nova"))
	assert.Equal(t, 0.0, domain.NameSimilarity("a", "b"))
	assert.Greater(t, domain.NameSimilarity("nova holdings", "nova holding"), domain.SimilarityThreshold)
	assert.Less(t, domain.NameSimilarity("nova holdings", "zenith traders"), 0.4)
}

func TestProposeFactRequest_Validate(t *testing.T) {
	ok := domain.ProposeFactRequest{FactType: domain.FactTradingName, Value: "Nova", ValidFrom: "2025-01-01",
		SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e", ExpectedMasterVersion: 1}
	require.NoError(t, ok.Validate())
	for name, mutate := range map[string]func(*domain.ProposeFactRequest){
		"bad type":     func(r *domain.ProposeFactRequest) { r.FactType = "NICKNAME" },
		"no value":     func(r *domain.ProposeFactRequest) { r.Value = "" },
		"no date":      func(r *domain.ProposeFactRequest) { r.ValidFrom = "" },
		"to before":    func(r *domain.ProposeFactRequest) { r.ValidTo = "2024-01-01" },
		"no source":    func(r *domain.ProposeFactRequest) { r.SourceClass = "" },
		"no evidence":  func(r *domain.ProposeFactRequest) { r.EvidenceRef = "" },
		"bad country":  func(r *domain.ProposeFactRequest) { r.FactType, r.Value = domain.FactCountry, "Britain" },
		"local script": func(r *domain.ProposeFactRequest) { r.FactType = domain.FactLocalScriptName },
	} {
		r := ok
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}
