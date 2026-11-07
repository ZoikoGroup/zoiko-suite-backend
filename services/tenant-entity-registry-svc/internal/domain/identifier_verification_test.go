package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var verifyNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func ago(days int) time.Time { return verifyNow.AddDate(0, 0, -days) }

func observe(result string, class SourceClass, observed time.Time, authorityStatus string) VerifyIdentifierRequest {
	return VerifyIdentifierRequest{Result: result, SourceClass: class, SourceRef: "src", ObservedAt: observed, EvidenceRef: "ev", AuthorityStatus: authorityStatus}
}

func state(status string, source SourceClass, last *time.Time) VerificationState {
	return VerificationState{Status: status, SourceClass: source, LastObservedAt: last}
}

// The s14.1 pipeline's rules, one case each: precedence, freshness, and "a higher authority that disagrees opens a
// case instead of overwriting".
func TestDecideObservation(t *testing.T) {
	ten := ago(10)
	for name, tc := range map[string]struct {
		cur      VerificationState
		scheme   IdentifierScheme
		req      VerifyIdentifierRequest
		effect   string
		status   string
		source   SourceClass
		conflict bool
		advances bool
		noteHas  string
	}{
		"a registry confirms an unverified identifier": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectVerified, IdentVerified, SourceAuthorityRegistry, false, true, ""},
		"a provider confirms an unverified identifier": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceExternalProvider, ago(1), ""), EffectVerified, IdentVerified, SourceExternalProvider, false, true, ""},
		"a tenant assertion cannot verify": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceTenantAsserted, ago(1), ""), EffectNoChange, IdentUnverified, "", false, false, "cannot verify"},
		"a system import cannot verify": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceSystemImport, ago(1), ""), EffectNoChange, IdentUnverified, "", false, false, "cannot verify"},
		"a stale observation is recorded, not applied": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(400), ""), EffectNoChange, IdentUnverified, "", false, false, "freshness"},
		"an LEI goes stale sooner": {state(IdentUnverified, "", nil), SchemeLEI,
			observe(ObsMatch, SourceAuthorityRegistry, ago(100), "ISSUED"), EffectNoChange, IdentUnverified, "", false, false, "freshness"},
		"an LEI within its window verifies": {state(IdentUnverified, "", nil), SchemeLEI,
			observe(ObsMatch, SourceAuthorityRegistry, ago(80), "ISSUED"), EffectVerified, IdentVerified, SourceAuthorityRegistry, false, true, ""},
		"a lapsed LEI is not verified": {state(IdentUnverified, "", nil), SchemeLEI,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), "LAPSED"), EffectNoChange, IdentUnverified, "", false, false, "LAPSED"},
		"an LEI with no stated status is not verified": {state(IdentUnverified, "", nil), SchemeLEI,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectNoChange, IdentUnverified, "", false, false, "unstated"},
		"authority status means nothing for other schemes": {state(IdentUnverified, "", nil), SchemeVAT,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), "LAPSED"), EffectVerified, IdentVerified, SourceAuthorityRegistry, false, true, ""},

		"a higher authority takes over the verification": {state(IdentVerified, SourceExternalProvider, nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectSourceUpgraded, IdentVerified, SourceAuthorityRegistry, false, true, ""},
		"a lower source does not replace a higher one": {state(IdentVerified, SourceAuthorityRegistry, nil), SchemeTaxID,
			observe(ObsMatch, SourceExternalProvider, ago(1), ""), EffectNoChange, IdentVerified, SourceAuthorityRegistry, false, false, "equal or higher"},
		"an equal source changes nothing": {state(IdentVerified, SourceAuthorityRegistry, nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectNoChange, IdentVerified, SourceAuthorityRegistry, false, false, "equal or higher"},

		"a higher authority that disagrees opens a conflict": {state(IdentVerified, SourceExternalProvider, nil), SchemeTaxID,
			observe(ObsMismatch, SourceAuthorityRegistry, ago(1), ""), EffectConflictOpened, IdentConflicted, SourceExternalProvider, true, true, "not overwritten"},
		"NOT_FOUND from a higher authority is a disagreement too": {state(IdentVerified, SourceExternalProvider, nil), SchemeTaxID,
			observe(ObsNotFound, SourceAuthorityDocument, ago(1), ""), EffectConflictOpened, IdentConflicted, SourceExternalProvider, true, true, ""},
		"a lower source that disagrees changes nothing": {state(IdentVerified, SourceAuthorityRegistry, nil), SchemeTaxID,
			observe(ObsMismatch, SourceExternalProvider, ago(1), ""), EffectNoChange, IdentVerified, SourceAuthorityRegistry, false, false, "does not outrank"},
		"an equal source that disagrees changes nothing": {state(IdentVerified, SourceAuthorityRegistry, nil), SchemeTaxID,
			observe(ObsMismatch, SourceAuthorityRegistry, ago(1), ""), EffectNoChange, IdentVerified, SourceAuthorityRegistry, false, false, "does not outrank"},
		"a provider's denial fails an unverified identifier": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMismatch, SourceExternalProvider, ago(1), ""), EffectFailed, IdentFailed, "", false, true, ""},
		"a tenant's denial fails nothing": {state(IdentUnverified, "", nil), SchemeTaxID,
			observe(ObsMismatch, SourceTenantAsserted, ago(1), ""), EffectNoChange, IdentUnverified, "", false, false, "cannot verify"},
		"a registry can recover a failed identifier": {state(IdentFailed, "", nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectVerified, IdentVerified, SourceAuthorityRegistry, false, true, ""},

		"an open conflict is left to a steward": {state(IdentConflicted, SourceExternalProvider, nil), SchemeTaxID,
			observe(ObsMatch, SourceAuthorityRegistry, ago(1), ""), EffectNoChange, IdentConflicted, SourceExternalProvider, false, false, "steward"},

		"an older observation does not undo a newer one": {state(IdentVerified, SourceExternalProvider, &ten), SchemeTaxID,
			observe(ObsMatch, SourceExternalProvider, ago(20), ""), EffectNoChange, IdentVerified, SourceExternalProvider, false, false, "older"},
		"an older observation does not undo a failure": {state(IdentFailed, "", &ten), SchemeTaxID,
			observe(ObsMatch, SourceExternalProvider, ago(20), ""), EffectNoChange, IdentFailed, "", false, false, "older"},
		"but a higher authority correcting a verified identifier is not held back by age": {state(IdentVerified, SourceExternalProvider, &ten), SchemeTaxID,
			observe(ObsMismatch, SourceAuthorityRegistry, ago(20), ""), EffectConflictOpened, IdentConflicted, SourceExternalProvider, true, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := DecideObservation(tc.cur, tc.scheme, tc.req, verifyNow)
			assert.Equal(t, tc.effect, d.Effect)
			assert.Equal(t, tc.status, d.NewStatus)
			assert.Equal(t, tc.source, d.NewSource)
			assert.Equal(t, tc.conflict, d.Conflict)
			assert.Equal(t, tc.advances, d.AdvanceTime)
			assert.NotEmpty(t, d.Note, "every decision says why")
			if tc.noteHas != "" {
				assert.Contains(t, d.Note, tc.noteHas)
			}
			// The invariants, whatever the case: a conflict only ever comes from a verified identifier, and a
			// disagreement never produces VERIFIED.
			if d.Conflict {
				assert.Equal(t, IdentVerified, tc.cur.Status)
				assert.Equal(t, IdentConflicted, d.NewStatus)
			}
			if tc.req.Result != ObsMatch {
				assert.True(t, d.Effect != EffectVerified && d.Effect != EffectSourceUpgraded, "a disagreement never verifies")
				if tc.cur.Status == IdentVerified {
					assert.Contains(t, []string{IdentVerified, IdentConflicted}, d.NewStatus, "a verified identifier is kept or disputed, never overwritten")
				}
			}
		})
	}
}

func TestSourceRankOrdersByAuthority(t *testing.T) {
	order := []SourceClass{SourceTenantAsserted, SourceSystemImport, SourceExternalProvider, SourceAuthorityDocument, SourceAuthorityRegistry}
	for i := 1; i < len(order); i++ {
		assert.Less(t, SourceRank(order[i-1]), SourceRank(order[i]), "%s < %s", order[i-1], order[i])
	}
	assert.Equal(t, 0, SourceRank("UNKNOWN"))
	assert.Equal(t, 90*24*time.Hour, FreshnessWindow(SchemeLEI))
	assert.Equal(t, 365*24*time.Hour, FreshnessWindow(SchemeVAT))
}

func TestVerifyIdentifierRequestValidate(t *testing.T) {
	ok := observe(ObsMatch, SourceAuthorityRegistry, ago(1), "")
	require.NoError(t, ok.Validate(verifyNow))
	mut := func(f func(*VerifyIdentifierRequest)) VerifyIdentifierRequest { r := ok; f(&r); return r }
	for name, r := range map[string]VerifyIdentifierRequest{
		"bad result":  mut(func(r *VerifyIdentifierRequest) { r.Result = "MAYBE" }),
		"no source":   mut(func(r *VerifyIdentifierRequest) { r.SourceClass = "" }),
		"no ref":      mut(func(r *VerifyIdentifierRequest) { r.SourceRef = " " }),
		"no evidence": mut(func(r *VerifyIdentifierRequest) { r.EvidenceRef = "" }),
		"no time":     mut(func(r *VerifyIdentifierRequest) { r.ObservedAt = time.Time{} }),
		"future":      mut(func(r *VerifyIdentifierRequest) { r.ObservedAt = verifyNow.Add(time.Hour) }),
		"bad hash":    mut(func(r *VerifyIdentifierRequest) { r.EvidenceHash = "nope" }),
		"long ref":    mut(func(r *VerifyIdentifierRequest) { r.SourceRef = strings.Repeat("x", 201) }),
		"long status": mut(func(r *VerifyIdentifierRequest) { r.AuthorityStatus = strings.Repeat("x", 25) }),
	} {
		assert.Error(t, r.Validate(verifyNow), name)
	}
	skew := mut(func(r *VerifyIdentifierRequest) { r.ObservedAt = verifyNow.Add(2 * time.Minute) })
	assert.NoError(t, skew.Validate(verifyNow), "a little clock skew is tolerated")
}

func TestLEICheckDigitsRoundTripWithTheExistingValidator(t *testing.T) {
	for _, base := range []string{"529900T8BM49AURSDO", "5493001KJTIIGC8Y1R", "HWUPKR0MPOU8FGXBT3", "ZOIKOTEST0000000A1"} {
		lei := base + LEICheckDigits(base)
		assert.Len(t, lei, 20)
		assert.True(t, ValidLEI(lei), lei)
		bad := base + "00"
		if LEICheckDigits(base) != "00" {
			assert.False(t, ValidLEI(bad), bad)
		}
	}
	assert.Equal(t, -1, leiMod97("ab"))
	assert.True(t, ValidLEI("5493001KJTIIGC8Y1R12"), "a published LEI")
	assert.Equal(t, "12", LEICheckDigits("5493001KJTIIGC8Y1R"))
}

func TestRulesAreStatedByTheCaseDecisions(t *testing.T) {
	assert.ElementsMatch(t, []string{DecisionKeepVerified, DecisionAcceptObservation}, DecisionsFor(CaseVerificationConflict))
}
