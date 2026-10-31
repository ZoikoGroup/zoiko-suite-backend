package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var decAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func asg(id, code, typ, evt string) Assignment {
	return Assignment{JurisdictionID: id, JurisdictionCode: code, ApplicabilityID: "app-" + evt, ApplicabilityType: typ,
		EffectiveFrom: decAt.AddDate(-1, 0, 0), SourceEventID: evt, SourceObjectVersion: 1}
}

func TestDetermineJurisdiction_SingleAuthorisedJurisdiction(t *testing.T) {
	d := DetermineJurisdiction([]Assignment{asg("j1", "GB", "ENTITY_PRIMARY", "e1")}, PurposeTax, "", decAt)
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Equal(t, BasisSingleAuthorised, d.Basis)
	require.NotNil(t, d.Jurisdiction)
	assert.Equal(t, "GB", d.Jurisdiction.JurisdictionCode)
	assert.Equal(t, ClaimNotProvided, d.Claim.Status)
	assert.Equal(t, "e1", d.Jurisdiction.Evidence[0].SourceEventID, "the decision names the assignment it rests on")

	// A claim that agrees is confirmed (case-insensitively); it is still the assignment that decides.
	d = DetermineJurisdiction([]Assignment{asg("j1", "GB", "PRIMARY", "e1")}, PurposeTax, " gb ", decAt)
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Equal(t, ClaimConfirmed, d.Claim.Status)
}

func TestDetermineJurisdiction_AClaimNeverWidensTheAuthorisedSet(t *testing.T) {
	// JUR-NEG-05: the entity is authorised for GB only; a claim of FR is refused, not accepted and not silently corrected.
	d := DetermineJurisdiction([]Assignment{asg("j1", "GB", "PRIMARY", "e1")}, PurposeTax, "FR", decAt)
	assert.Equal(t, OutcomeClaimNotAuthorised, d.Outcome)
	assert.Equal(t, ClaimNotAuthorised, d.Claim.Status)
	assert.Nil(t, d.Jurisdiction, "nothing is chosen for the caller")
	require.Len(t, d.Candidates, 1, "the authorised set is shown so the caller can correct the claim")
	assert.Equal(t, "GB", d.Candidates[0].JurisdictionCode)
}

func TestDetermineJurisdiction_NothingAuthorisedNeverFallsBackToTheClaim(t *testing.T) {
	for _, claim := range []string{"", "GB"} {
		d := DetermineJurisdiction(nil, PurposeTax, claim, decAt)
		assert.Equal(t, OutcomeNoApplicable, d.Outcome, "claim %q", claim)
		assert.Nil(t, d.Jurisdiction)
		assert.Empty(t, d.Candidates)
	}
	// An assignment of the wrong kind for the purpose is not an authorisation for it.
	d := DetermineJurisdiction([]Assignment{asg("j1", "GB", "FILING_ONLY", "e1")}, PurposeTax, "GB", decAt)
	assert.Equal(t, OutcomeNoApplicable, d.Outcome, "a FILING_ONLY assignment does not authorise a TAX determination")
}

func TestDetermineJurisdiction_SeveralAuthorisedNeedReviewUnlessAClaimSelectsAmongThem(t *testing.T) {
	two := []Assignment{asg("j1", "GB", "PRIMARY", "e1"), asg("j2", "FR", "SECONDARY", "e2")}
	d := DetermineJurisdiction(two, PurposeTax, "", decAt)
	assert.Equal(t, OutcomeJurisdictionAmbiguous, d.Outcome, "nothing is guessed")
	assert.Nil(t, d.Jurisdiction)
	require.Len(t, d.Candidates, 2)
	assert.Equal(t, "FR", d.Candidates[0].JurisdictionCode, "candidates are ordered by code so the answer is stable")

	d = DetermineJurisdiction(two, PurposeTax, "fr", decAt)
	assert.Equal(t, OutcomeJurisdictionDetermined, d.Outcome)
	assert.Equal(t, BasisClaimWithinAuthorised, d.Basis)
	assert.Equal(t, ClaimSelectedAmongAuthorised, d.Claim.Status)
	assert.Equal(t, "FR", d.Jurisdiction.JurisdictionCode)

	d = DetermineJurisdiction(two, PurposeTax, "DE", decAt)
	assert.Equal(t, OutcomeClaimNotAuthorised, d.Outcome, "a claim outside the set is refused even when several are authorised")
}

func TestDetermineJurisdiction_PurposeFiltersAndTheSameJurisdictionIsOneCandidate(t *testing.T) {
	rows := []Assignment{
		asg("j1", "GB", "ENTITY_PRIMARY", "e1"), asg("j1", "GB", "PRIMARY", "e2"), // the same jurisdiction twice: one candidate
		asg("j2", "FR", "TAX_ONLY", "e3"), asg("j3", "DE", "FILING_ONLY", "e4"),
	}
	tax := DetermineJurisdiction(rows, PurposeTax, "", decAt)
	assert.Equal(t, OutcomeJurisdictionAmbiguous, tax.Outcome)
	require.Len(t, tax.Candidates, 2, "GB and FR; DE is filing-only")
	var gb Candidate
	for _, c := range tax.Candidates {
		if c.JurisdictionCode == "GB" {
			gb = c
		}
	}
	assert.Equal(t, []string{"ENTITY_PRIMARY", "PRIMARY"}, gb.ApplicabilityType)
	assert.Len(t, gb.Evidence, 2, "both assignments are cited")

	filing := DetermineJurisdiction(rows, PurposeFiling, "", decAt)
	codes := []string{}
	for _, c := range filing.Candidates {
		codes = append(codes, c.JurisdictionCode)
	}
	assert.Equal(t, []string{"DE", "GB"}, codes)
	assert.Len(t, DetermineJurisdiction(rows, PurposeAny, "", decAt).Candidates, 3)
}

func TestValidPurpose(t *testing.T) {
	for _, p := range []string{"TAX", "FILING", "ANY"} {
		assert.True(t, ValidPurpose(p))
	}
	for _, p := range []string{"", "tax", "PAYROLL"} {
		assert.False(t, ValidPurpose(p), p)
	}
}
