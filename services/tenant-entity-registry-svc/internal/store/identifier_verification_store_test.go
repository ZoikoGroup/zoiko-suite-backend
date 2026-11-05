package store_test

// Identifier verification against a real Postgres: the ZS-SVC-U-001 s14.1 pipeline step by step, precedence and
// freshness instead of last-write-wins, a higher-authority disagreement opening a case instead of overwriting, LEI
// alignment, and what the database holds whatever the service does.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	obsActor     = "p-observer"
	stewardActor = "p-steward-2"
)

func (f *orgFixture) obs(result string, class domain.SourceClass, observed time.Time, status string) domain.VerifyIdentifierRequest {
	return domain.VerifyIdentifierRequest{Result: result, SourceClass: class, SourceRef: "Registry of " + string(class), SourceVersion: "2026-06",
		ExchangeRef: "xic-" + uuid.NewString()[:8], ObservedAt: observed, AuthorityStatus: status, EvidenceRef: "response-" + uuid.NewString()[:8]}
}

// unverifiedIdentifier gives a fresh party an identifier linked by the tenant's own assertion: UNVERIFIED.
func (f *orgFixture) unverifiedIdentifier(t *testing.T, scheme domain.IdentifierScheme, value string) (*domain.Party, string) {
	t.Helper()
	p := f.party(t, "iv-"+uuid.NewString()[:8], "Verify Subject "+uuid.NewString()[:6]+" Limited", "LEI-"+uuid.NewString()[:8])
	out, err := f.s.LinkPartyIdentifier(f.ctx, p.PartyID, domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
		Scheme: scheme, Scope: "GB", Value: value, SourceClass: domain.SourceTenantAsserted, EvidenceRef: "form"}, ExpectedMasterVersion: p.MasterVersion}, "p-steward", false)
	require.NoError(t, err)
	require.NotNil(t, out.Identifier)
	assert.Equal(t, "UNVERIFIED", out.Identifier.VerificationStatus)
	return &out.Party, out.Identifier.IdentifierID
}

func (f *orgFixture) verifyIdent(t *testing.T, id string, version int64, req domain.VerifyIdentifierRequest) *registry.IdentifierVerifyOutcome {
	t.Helper()
	req.ExpectedMasterVersion = version
	out, err := f.s.VerifyPartyIdentifier(f.ctx, id, req, obsActor, false)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) identRow(t *testing.T, id string) (status, source string) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT verification_status, COALESCE(verified_source_class,'') FROM party_identifiers WHERE identifier_id = $1`, id).Scan(&status, &source))
	return
}

func TestIdentifierVerification_PipelineStepByStep(t *testing.T) {
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB 123 4567 89")
	now := time.Now().UTC()

	// Steps 1 to 4 and 7: a tenant's own assertion is recorded as evidence and verifies nothing.
	weak := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceTenantAsserted, now.Add(-time.Hour), ""))
	assert.Equal(t, domain.EffectNoChange, weak.Observation.Effect)
	assert.Equal(t, "UNVERIFIED", weak.Identifier.VerificationStatus)
	assert.Equal(t, p.MasterVersion, weak.Party.MasterVersion, "nothing changed, so the party's version did not move")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'party.identifier.verified' AND tenant_id = $1`, f.tenantID),
		"s14.1 step 7: nothing is published unless the acceptance rules pass")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM identifier_verifications WHERE identifier_id = $1`, id), "step 4: the observation is captured anyway")

	// A provider's confirmation passes the rules: VERIFIED, by that source, published.
	r := f.obs(domain.ObsMatch, domain.SourceExternalProvider, now.Add(-time.Hour), "")
	ok := f.verifyIdent(t, id, p.MasterVersion, r)
	assert.Equal(t, domain.EffectVerified, ok.Observation.Effect)
	assert.Equal(t, "VERIFIED", ok.Identifier.VerificationStatus)
	assert.Equal(t, "UNVERIFIED", ok.Observation.StatusBefore)
	assert.Equal(t, "VERIFIED", ok.Observation.StatusAfter)
	assert.Equal(t, p.MasterVersion+1, ok.Party.MasterVersion)
	assert.Equal(t, r.ExchangeRef, ok.Observation.ExchangeRef)
	assert.Equal(t, r.SourceVersion, ok.Observation.SourceVersion)
	status, source := f.identRow(t, id)
	assert.Equal(t, "VERIFIED", status)
	assert.Equal(t, "EXTERNAL_PROVIDER", source)

	var payload string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT payload::text FROM event_outbox WHERE event_type = 'party.identifier.verified' AND tenant_id = $1`, f.tenantID).Scan(&payload))
	assert.NotContains(t, payload, "123 4567", "an event never carries the identifier's value")
	assert.NotContains(t, strings.ReplaceAll(payload, " ", ""), "1234567")
	assert.Contains(t, payload, id)
}

func TestIdentifierVerification_AHigherAuthorityThatDisagreesOpensACaseInsteadOfOverwriting(t *testing.T) { // s14.1 step 6
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB987654321")
	now := time.Now().UTC()
	v := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceExternalProvider, now.Add(-2*time.Hour), ""))

	c := f.verifyIdent(t, id, v.Party.MasterVersion, f.obs(domain.ObsMismatch, domain.SourceAuthorityRegistry, now.Add(-time.Hour), ""))
	assert.Equal(t, domain.EffectConflictOpened, c.Observation.Effect)
	require.NotNil(t, c.Case)
	assert.Equal(t, domain.CaseVerificationConflict, c.Case.CaseType)
	assert.Equal(t, domain.CaseOpen, c.Case.Status)
	assert.Equal(t, p.PartyID, *c.Case.SubjectPartyID)
	assert.Equal(t, c.Case.CaseID, c.Observation.CaseID)
	assert.Equal(t, "CONFLICTED", c.Identifier.VerificationStatus)
	status, source := f.identRow(t, id)
	assert.Equal(t, "CONFLICTED", status)
	assert.Equal(t, "EXTERNAL_PROVIDER", source, "the verified state is held, not overwritten: its source is still the one that verified it")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master.conflict.opened' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'party.identifier.verified' AND tenant_id = $1`, f.tenantID),
		"the dispute publishes a conflict, not a verification")

	// While it is disputed, nothing else moves it: a steward decides.
	again := f.verifyIdent(t, id, c.Party.MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now, ""))
	assert.Equal(t, domain.EffectNoChange, again.Observation.Effect)
	assert.Contains(t, again.Observation.EffectNote, "steward")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM resolution_cases WHERE tenant_id = $1 AND case_type = 'VERIFICATION_CONFLICT'`, f.tenantID))

	// A disputed identifier is not one a create may link to automatically.
	dup := createReq("iv-dup-"+uuid.NewString()[:6], "Another Verify Subject Limited")
	dup.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeVAT, Scope: "GB", Value: "GB987654321"}}
	res := f.createParty(t, dup)
	assert.NotEqual(t, domain.OutcomeMatchedExisting, res.Outcome, "a CONFLICTED identifier is not a safe link")

	// The opener cannot decide it; another steward can, either way.
	kase := c.Case
	_, err := f.s.DecideResolutionCase(f.ctx, kase.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionKeepVerified, Note: "n", ExpectedCaseVersion: kase.RecordVersion}, obsActor, false)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	_, err = f.s.DecideResolutionCase(f.ctx, kase.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionMoveToRequester, Note: "n", ExpectedCaseVersion: kase.RecordVersion}, stewardActor, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a verification conflict is decided by keeping or accepting")

	dec, err := f.s.DecideResolutionCase(f.ctx, kase.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionKeepVerified, Note: "registry was stale", ExpectedCaseVersion: kase.RecordVersion}, stewardActor, false)
	require.NoError(t, err)
	assert.Equal(t, domain.CaseResolved, dec.Case.Status)
	status, source = f.identRow(t, id)
	assert.Equal(t, "VERIFIED", status)
	assert.Equal(t, "EXTERNAL_PROVIDER", source)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM identifier_verifications WHERE identifier_id = $1 AND effect = 'CONFLICT_OPENED'`, id), "the observations stay as the record")
}

func TestIdentifierVerification_AcceptingTheObservationFailsTheIdentifier(t *testing.T) {
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB555000111")
	now := time.Now().UTC()
	v := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceExternalProvider, now.Add(-2*time.Hour), ""))
	c := f.verifyIdent(t, id, v.Party.MasterVersion, f.obs(domain.ObsNotFound, domain.SourceAuthorityDocument, now.Add(-time.Hour), ""))
	require.NotNil(t, c.Case)

	_, err := f.s.DecideResolutionCase(f.ctx, c.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionAcceptObservation, Note: "document shows it was cancelled", ExpectedCaseVersion: c.Case.RecordVersion}, stewardActor, false)
	require.NoError(t, err)
	status, source := f.identRow(t, id)
	assert.Equal(t, "FAILED", status)
	assert.Empty(t, source)

	// A FAILED identifier can be recovered by a later authority confirmation.
	back := f.verifyIdent(t, id, f.fresh(t, p.PartyID).MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now, ""))
	assert.Equal(t, domain.EffectVerified, back.Observation.Effect)
}

func TestIdentifierVerification_PrecedenceAndFreshnessNotLastWriteWins(t *testing.T) { // s14.1 step 5
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeTaxID, "GB777888999")
	now := time.Now().UTC()

	v := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceExternalProvider, now.Add(-5*24*time.Hour), ""))
	assert.Equal(t, domain.EffectVerified, v.Observation.Effect)

	// An older observation, even from the same source, does not undo the newer one.
	old := f.verifyIdent(t, id, v.Party.MasterVersion, f.obs(domain.ObsMismatch, domain.SourceExternalProvider, now.Add(-20*24*time.Hour), ""))
	assert.Equal(t, domain.EffectNoChange, old.Observation.Effect)

	// A lower source that disagrees later does not outrank it.
	reg := f.verifyIdent(t, id, v.Party.MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-24*time.Hour), ""))
	assert.Equal(t, domain.EffectSourceUpgraded, reg.Observation.Effect, "a higher authority takes over")
	_, source := f.identRow(t, id)
	assert.Equal(t, "AUTHORITY_REGISTRY", source)
	low := f.verifyIdent(t, id, reg.Party.MasterVersion, f.obs(domain.ObsMismatch, domain.SourceExternalProvider, now, ""))
	assert.Equal(t, domain.EffectNoChange, low.Observation.Effect)
	assert.Contains(t, low.Observation.EffectNote, "does not outrank")
	status, source := f.identRow(t, id)
	assert.Equal(t, "VERIFIED", status)
	assert.Equal(t, "AUTHORITY_REGISTRY", source, "a lower source never replaces a higher one's verification")

	// A stale observation is recorded and not applied.
	_, id2 := f.unverifiedIdentifier(t, domain.SchemeTaxID, "GB000111222")
	stale := f.verifyIdent(t, id2, f.fresh(t, f.partyOf(t, id2)).MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-400*24*time.Hour), ""))
	assert.Equal(t, domain.EffectNoChange, stale.Observation.Effect)
	assert.Contains(t, stale.Observation.EffectNote, "freshness")

	list, err := f.s.ListIdentifierVerifications(f.ctx, id)
	require.NoError(t, err)
	require.Len(t, list, 4)
	assert.Equal(t, domain.ObsMismatch, list[0].Result, "newest first: every observation is kept, applied or not")
	effects := map[string]int{}
	for _, o := range list {
		effects[o.Effect]++
	}
	assert.Equal(t, map[string]int{"VERIFIED": 1, "NO_CHANGE": 2, "SOURCE_UPGRADED": 1}, effects)
}

func (f *orgFixture) partyOf(t *testing.T, identifierID string) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT party_id::text FROM party_identifiers WHERE identifier_id = $1`, identifierID).Scan(&id))
	return id
}

func TestIdentifierVerification_ChecksVersionAndWhatItNames(t *testing.T) {
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB12121212")
	r := f.obs(domain.ObsMatch, domain.SourceExternalProvider, time.Now().UTC().Add(-time.Hour), "")

	r.ExpectedMasterVersion = 99
	_, err := f.s.VerifyPartyIdentifier(f.ctx, id, r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	r.ExpectedMasterVersion = 0
	_, err = f.s.VerifyPartyIdentifier(f.ctx, id, r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	r.ExpectedMasterVersion = p.MasterVersion
	_, err = f.s.VerifyPartyIdentifier(f.ctx, uuid.NewString(), r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.VerifyPartyIdentifier(f.ctx, "not-a-uuid", r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM identifier_verifications WHERE identifier_id = $1`, id), "a refused observation records nothing")

	// An ended identifier is not verified: its replacement is.
	_, err = f.pool.Exec(context.Background(), `UPDATE party_identifiers SET status = 'ENDED', ended_at = now(), ended_reason = 'TEST' WHERE identifier_id = $1`, id)
	require.NoError(t, err)
	_, err = f.s.VerifyPartyIdentifier(f.ctx, id, r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrStateConflict)
}

func TestIdentifierVerification_LEIAlignmentAndRules(t *testing.T) { // s13.3
	f := newORGFixture(t)
	now := time.Now().UTC()
	lei := "529900T8BM49AURSDO" + domain.LEICheckDigits("529900T8BM49AURSDO")

	// A malformed LEI is refused; an LEI is not mandatory and not a key.
	bp, bid := f.unverifiedIdentifier(t, domain.SchemeLEI, "NOT-AN-LEI")
	r := f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-time.Hour), "ISSUED")
	r.ExpectedMasterVersion = bp.MasterVersion
	_, err := f.s.VerifyPartyIdentifier(f.ctx, bid, r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	// Not linked to a legal entity.
	p, id := f.unverifiedIdentifier(t, domain.SchemeLEI, lei)
	lapsed := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-time.Hour), "LAPSED"))
	assert.Equal(t, domain.EffectNoChange, lapsed.Observation.Effect, "a lapsed LEI is recorded, not verified")
	assert.Equal(t, domain.LEINotApplicable, lapsed.LEIAlignment)
	assert.Equal(t, "LAPSED", lapsed.Observation.AuthorityStatus)
	ok := f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-time.Hour), "ISSUED"))
	assert.Equal(t, domain.EffectVerified, ok.Observation.Effect)
	assert.Equal(t, domain.LEINotLinked, ok.LEIAlignment, "the party stands for no legal entity")

	// A party that stands for the tenant's legal entity: compared with the ORG-03 profile.
	lei = "ZOIKOTEST0000000A1" + domain.LEICheckDigits("ZOIKOTEST0000000A1")
	req := createReq("iv-le-"+uuid.NewString()[:6], "Original Name Ltd")
	req.LegalEntityID = f.entityID
	req.Names = []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Original Name Ltd Party"}}
	res := f.createParty(t, req)
	require.Equal(t, domain.OutcomeCreated, res.Outcome)
	lp := res.Party
	link, err := f.s.LinkPartyIdentifier(f.ctx, lp.PartyID, domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
		Scheme: domain.SchemeLEI, Scope: "GB", Value: strings.ToLower(lei), SourceClass: domain.SourceTenantAsserted, EvidenceRef: "form"}, ExpectedMasterVersion: lp.MasterVersion}, "p-steward", false)
	require.NoError(t, err)
	lid := link.Identifier.IdentifierID
	ver := link.Party.MasterVersion

	a := f.verifyIdent(t, lid, ver, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-time.Hour), "ISSUED"))
	assert.Equal(t, domain.LEIProfileAbsent, a.LEIAlignment, "the legal entity profile holds no LEI")
	ver = a.Party.MasterVersion

	setProfileLEI := func(value string, entityVersion int64) {
		src, st := "GLEIF", "ISSUED"
		_, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
			ProfileVersionID: uuid.NewString(), TenantID: f.tenantID, LegalEntityID: f.entityID, LegalName: "Original Name Ltd",
			LEI: &value, LEISource: &src, LEIStatus: &st, EffectiveFrom: time.Now().UTC().Add(-time.Minute), ChangeReason: domain.ProfileChangeAmendment, CreatedByPrincipalID: "p-actor",
		}, entityVersion, nil)
		require.NoError(t, err)
	}
	setProfileLEI(lei, 1)
	b := f.verifyIdent(t, lid, ver, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-30*time.Minute), "ISSUED"))
	assert.Equal(t, domain.LEIAligned, b.LEIAlignment)
	assert.Equal(t, domain.EffectNoChange, b.Observation.Effect, "already verified by an equal source")

	other := "5493001KJTIIGC8Y1R12"
	setProfileLEI(other, 2)
	c := f.verifyIdent(t, lid, b.Party.MasterVersion, f.obs(domain.ObsMatch, domain.SourceAuthorityRegistry, now.Add(-10*time.Minute), "ISSUED"))
	assert.Equal(t, domain.LEIDiffers, c.LEIAlignment, "a difference is reported for a steward; ORG-03 owns the profile and it is not written over")
	var profileLEI string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT lei FROM legal_entity_profile_versions WHERE legal_entity_id = $1 ORDER BY version_number DESC LIMIT 1`, f.entityID).Scan(&profileLEI))
	assert.Equal(t, other, profileLEI)
}

func TestIdentifierVerification_TenantsNeverSeeEachOthersObservations(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	p, id := a.unverifiedIdentifier(t, domain.SchemeVAT, "GB31313131")
	a.verifyIdent(t, id, p.MasterVersion, a.obs(domain.ObsMatch, domain.SourceExternalProvider, time.Now().UTC().Add(-time.Hour), ""))

	_, err := b.s.ListIdentifierVerifications(b.ctx, id)
	require.ErrorIs(t, err, registry.ErrNotFound)
	r := b.obs(domain.ObsMatch, domain.SourceExternalProvider, time.Now().UTC().Add(-time.Hour), "")
	r.ExpectedMasterVersion = p.MasterVersion + 1
	_, err = b.s.VerifyPartyIdentifier(b.ctx, id, r, obsActor, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestIdentifierVerification_TheDatabaseHoldsTheRules(t *testing.T) {
	f := newORGFixture(t)
	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB41414141")
	f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceExternalProvider, time.Now().UTC().Add(-time.Hour), ""))
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, q, args...)
		return err
	}
	require.Error(t, exec(`UPDATE identifier_verifications SET effect = 'FAILED' WHERE identifier_id = $1`, id), "observations are append-only")
	require.Error(t, exec(`DELETE FROM identifier_verifications WHERE identifier_id = $1`, id))
	require.Error(t, exec(`INSERT INTO identifier_verifications (observation_id, tenant_id, identifier_id, party_id, scheme, result, source_class, source_ref, observed_at,
		evidence_ref, effect, effect_note, status_before, status_after, recorded_by)
		VALUES (gen_random_uuid(), $1, $2, $3, 'VAT', 'MISMATCH', 'AUTHORITY_REGISTRY', 's', now(), 'e', 'CONFLICT_OPENED', 'n', 'VERIFIED', 'CONFLICTED', 'x')`,
		f.tenantID, id, p.PartyID), "a conflict observation names its case")
	require.Error(t, exec(`UPDATE party_identifiers SET verification_status = 'BOGUS' WHERE identifier_id = $1`, id))
	require.NoError(t, exec(`UPDATE party_identifiers SET verification_status = 'CONFLICTED' WHERE identifier_id = $1`, id), "CONFLICTED is a known status")

	// An identifier linked as VERIFIED from an authority was verified by that source: the trigger records it.
	var source string
	reg := f.party(t, "iv-auth-"+uuid.NewString()[:6], "Authority Linked Limited", "LEI-"+uuid.NewString()[:8])
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COALESCE(verified_source_class,'') FROM party_identifiers WHERE party_id = $1 AND verification_status = 'VERIFIED'`, reg.PartyID).Scan(&source))
	assert.Equal(t, "AUTHORITY_REGISTRY", source)
}

// Migration 000021 reverses cleanly while no observation or conflict exists, and applies again; with one it refuses.
func TestIdentifierVerification_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'identifier_verifications'`
	const column = `SELECT count(*) FROM information_schema.columns WHERE table_name = 'party_identifiers' AND column_name = 'verified_source_class'`

	p, id := f.unverifiedIdentifier(t, domain.SchemeVAT, "GB51515151")
	f.verifyIdent(t, id, p.MasterVersion, f.obs(domain.ObsMatch, domain.SourceExternalProvider, time.Now().UTC().Add(-time.Hour), ""))
	_, err := f.pool.Exec(ctx, read("000021_identifier_verification.down.sql"))
	require.Error(t, err, "down is refused while observations exist")
	assert.Contains(t, err.Error(), "000021 down refused")
	assert.True(t, exists(table))

	// On an empty schema it reverses and applies again.
	f2 := newORGFixture(t)
	_ = f2
	_, err = f2.pool.Exec(ctx, read("000021_identifier_verification.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table), "down drops the table")
	assert.False(t, exists(column), "and the columns")
	_, err = f2.pool.Exec(ctx, read("000021_identifier_verification.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
	assert.True(t, exists(column))
}
