package store_test

// MDM-05 against a real Postgres: s18.2 merge steps 1-6, split lineage, stewardship decisions, and what the
// database enforces whatever the service does.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	merger   = "p-merger"
	splitter = "p-splitter"
)

func (f *orgFixture) party(t *testing.T, id, name, lei string) *domain.Party {
	t.Helper()
	req := createReq(id, name)
	req.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: lei}}
	res := f.createParty(t, req)
	require.Equal(t, domain.OutcomeCreated, res.Outcome, name)
	return res.Party
}

// conflictCase opens an IDENTIFIER_CONFLICT case over a and b: a create naming both of their identifiers.
func (f *orgFixture) conflictCase(t *testing.T, leiA, leiB string) *domain.ResolutionCase {
	t.Helper()
	req := createReq("conflict-"+uuid.NewString(), "Third Party Limited")
	req.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: leiA}, {Scheme: domain.SchemeLEI, Value: leiB}}
	res := f.createParty(t, req)
	require.Equal(t, domain.OutcomeReviewRequired, res.Outcome)
	return res.Case
}

func mergeReq(survivor *domain.Party, merged ...*domain.Party) domain.MergePartiesRequest {
	r := domain.MergePartiesRequest{SurvivorPartyID: survivor.PartyID, ExpectedMasterVersions: map[string]int64{survivor.PartyID: survivor.MasterVersion},
		Reason: "same company, two records", EvidenceRef: "registry-extract-9", CorrelationID: "corr-merge"}
	for _, m := range merged {
		r.MergedPartyIDs = append(r.MergedPartyIDs, m.PartyID)
		r.ExpectedMasterVersions[m.PartyID] = m.MasterVersion
	}
	return r
}

func (f *orgFixture) view(t *testing.T, id string) *domain.PartyView {
	t.Helper()
	v, err := f.s.GetPartyView(f.ctx, id, time.Now().UTC(), time.Now().UTC(), true)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

func (f *orgFixture) fresh(t *testing.T, id string) *domain.Party { return &f.view(t, id).Party }

func (f *orgFixture) role(t *testing.T, p *domain.Party, rt domain.RoleType) *domain.Party {
	t.Helper()
	out, err := f.s.AddPartyRole(f.ctx, p.PartyID, domain.AddPartyRoleRequest{RoleType: rt, ValidFrom: "2025-01-01", EvidenceRef: "contract",
		ExpectedMasterVersion: p.MasterVersion}, "p-steward", false)
	require.NoError(t, err)
	return &out.Party
}

func (f *orgFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func TestMerge_SurvivorKeepsItsIdAndTheDuplicateStillResolves(t *testing.T) { // s18.2 steps 3, 4, 6
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	a = f.role(t, a, domain.RoleCustomer)
	b = f.role(t, b, domain.RoleCustomer)
	b = f.role(t, b, domain.RoleSupplier)
	c := f.conflictCase(t, "LEI-A", "LEI-B")

	out, err := f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
	require.NoError(t, err)

	assert.Equal(t, a.PartyID, out.Survivor.PartyID, "the survivor keeps its id")
	assert.Equal(t, a.MasterVersion+1, out.Survivor.MasterVersion)
	require.Len(t, out.Aliases, 1)
	assert.Equal(t, domain.PartyMerged, out.Aliases[0].Status)
	require.NotNil(t, out.Aliases[0].MergedInto)
	assert.Equal(t, a.PartyID, *out.Aliases[0].MergedInto)
	assert.Equal(t, domain.CaseResolved, out.Case.Status)
	assert.Equal(t, domain.DecisionMerge, out.Case.Decision)
	require.Len(t, out.Lineage, 1)
	assert.Equal(t, merger, out.Lineage[0].Actor)
	assert.Equal(t, "p-steward", out.Lineage[0].IndependentOf, "independent of whoever opened the case")

	// the merged id still resolves, to the survivor
	bv := f.view(t, b.PartyID)
	assert.Equal(t, domain.PartyMerged, bv.Party.Status)
	assert.Equal(t, a.PartyID, *bv.Party.MergedInto)
	av := f.view(t, a.PartyID)
	assert.Equal(t, []string{b.PartyID}, av.AliasIDs)
	h, err := f.s.ExplainMergeHistory(f.ctx, b.PartyID)
	require.NoError(t, err)
	assert.Equal(t, a.PartyID, h.ResolvesTo)
	require.Len(t, h.Lineage, 1)
	h, err = f.s.ExplainMergeHistory(f.ctx, a.PartyID)
	require.NoError(t, err)
	assert.Equal(t, a.PartyID, h.ResolvesTo)
	assert.Equal(t, []string{b.PartyID}, h.AliasIDs)
	none, err := f.s.ExplainMergeHistory(f.ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Nil(t, none)

	// identifiers moved by ending and copying; the old row is history
	found, err := f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeLEI, "", "LEI-B")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, a.PartyID, found[0].PartyID, "the identifier now names the survivor")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_identifiers WHERE party_id = $1 AND status = 'ENDED' AND ended_reason = 'MERGED'`, b.PartyID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_identifiers WHERE party_id = $1 AND moved_from_identifier_id IS NOT NULL`, a.PartyID))

	// roles: the survivor gains the one it lacked, and holds one CUSTOMER role, not two
	var types []domain.RoleType
	for _, r := range f.view(t, a.PartyID).Roles {
		types = append(types, r.RoleType)
	}
	assert.ElementsMatch(t, []domain.RoleType{domain.RoleCustomer, domain.RoleSupplier}, types)
	assert.Empty(t, f.view(t, b.PartyID).Roles, "the duplicate's roles ended")
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM party_roles WHERE party_id = $1`, b.PartyID), "and stayed as history")

	// the duplicate takes no more writes; its id is not reusable and is no longer a match candidate
	_, err = f.s.AddPartyRole(f.ctx, b.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleBank, ValidFrom: "2025-01-01",
		EvidenceRef: "e", ExpectedMasterVersion: bv.Party.MasterVersion}, "p", false)
	assert.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = f.pool.Exec(context.Background(), `INSERT INTO parties (party_id, tenant_id, party_kind, created_by) VALUES ($1,$2,'LEGAL_ENTITY','x')`, b.PartyID, f.tenantID)
	assert.Error(t, err, "a merged id is never issued again")
	m, err := f.s.MatchParties(f.ctx, domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity, Country: "GB", Names: []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Bravo Limited"}}})
	require.NoError(t, err)
	assert.Equal(t, "NO_MATCH", m.Verdict, "a merged-away party is not a candidate; its survivor is")

	assert.Equal(t, 1, outboxCount(t, f, "party.merged"))
	assert.Equal(t, 1, outboxCount(t, f, "master.conflict.resolved"))
	var payload string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT payload::text FROM event_outbox WHERE event_type = 'party.merged' AND tenant_id = $1`, f.tenantID).Scan(&payload))
	assert.Contains(t, payload, b.PartyID)
	assert.Contains(t, payload, "REFERENCES", "downstream remediation scope is named")
}

func TestMerge_RefusalsChangeNothing(t *testing.T) { // s18.2 step 2: revalidate before the decision commits
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	stranger := f.party(t, "rc", "Charlie Limited", "LEI-C")

	_, err := f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), "p-steward")
	assert.ErrorIs(t, err, registry.ErrSelfApproval, "the opener cannot decide the merge")

	stale := mergeReq(a, b)
	stale.ExpectedMasterVersions[b.PartyID] = 99
	_, err = f.s.MergeParties(f.ctx, c.CaseID, stale, merger)
	assert.ErrorIs(t, err, registry.ErrVersionConflict, "a party that moved since the steward looked")

	_, err = f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, stranger), merger)
	assert.ErrorIs(t, err, registry.ErrInvalidInput, "a party the case does not name")

	_, err = f.s.MergeParties(f.ctx, uuid.NewString(), mergeReq(a, b), merger)
	assert.ErrorIs(t, err, registry.ErrNotFound)

	person := createReq("rp", "Alpha Person")
	person.PartyKind = domain.PartyNaturalPerson
	person.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeNationalID, Value: "NID-1"}}
	pp := f.createParty(t, person).Party
	// a case over a legal entity and a person
	mixed := createReq("mixed", "Mixed")
	mixed.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "LEI-A"}, {Scheme: domain.SchemeNationalID, Value: "NID-1"}}
	mc := f.createParty(t, mixed)
	require.Equal(t, domain.OutcomeReviewRequired, mc.Outcome)
	_, err = f.s.MergeParties(f.ctx, mc.Case.CaseID, mergeReq(a, pp), merger)
	assert.ErrorIs(t, err, registry.ErrMergeProhibited, "a person is not merged into a legal entity")

	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM party_lineage`))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM parties WHERE status = 'MERGED'`))
	assert.Equal(t, int64(1), f.fresh(t, b.PartyID).MasterVersion)
	assert.Equal(t, 0, outboxCount(t, f, "party.merged"))
	assert.Equal(t, domain.CaseOpen, func() string { cc, _ := f.s.GetResolutionCase(f.ctx, c.CaseID); return cc.Status }())
}

func TestMerge_ADecidedCaseIsNotDecidedTwiceAndMergesDoNotChain(t *testing.T) {
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	c1 := f.conflictCase(t, "LEI-A", "LEI-B")
	_, err := f.s.MergeParties(f.ctx, c1.CaseID, mergeReq(a, b), merger)
	require.NoError(t, err)

	a2 := f.fresh(t, a.PartyID)
	_, err = f.s.MergeParties(f.ctx, c1.CaseID, mergeReq(a2, b), merger)
	assert.ErrorIs(t, err, registry.ErrStateConflict, "a case is decided once")

	// a survivor that absorbed another party cannot itself be merged away
	d := f.party(t, "rd", "Delta Limited", "LEI-D")
	c2 := f.conflictCase(t, "LEI-A", "LEI-D")
	_, err = f.s.MergeParties(f.ctx, c2.CaseID, mergeReq(d, a2), merger)
	assert.ErrorIs(t, err, registry.ErrMergeProhibited, "merges do not chain")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM parties WHERE status = 'MERGED'`))
}

func TestMerge_DistinctLegalEntitiesAreNeverCollapsed(t *testing.T) { // s18.1 tier D
	f := newORGFixture(t)
	other := ownershipEntity(t, f)
	mk := func(id, name, lei, le string) *domain.Party {
		req := createReq(id, name)
		req.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: lei}}
		req.LegalEntityID = le
		res := f.createParty(t, req)
		require.Equal(t, domain.OutcomeCreated, res.Outcome)
		return res.Party
	}
	a := mk("ra", "Alpha Limited", "LEI-A", f.entityID)
	b := mk("rb", "Bravo Limited", "LEI-B", other)
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	_, err := f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
	require.ErrorIs(t, err, registry.ErrMergeProhibited)
	assert.Contains(t, err.Error(), "distinct legal entities")
}

func TestMerge_SurvivorAdoptsTheLegalEntityAndASplitReturnsIt(t *testing.T) {
	f := newORGFixture(t)
	req := createReq("rb", "Bravo Limited")
	req.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "LEI-B"}}
	req.LegalEntityID = f.entityID
	b := f.createParty(t, req).Party
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	c := f.conflictCase(t, "LEI-A", "LEI-B")

	out, err := f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
	require.NoError(t, err)
	require.NotNil(t, out.Survivor.LegalEntityID)
	assert.Equal(t, f.entityID, *out.Survivor.LegalEntityID, "a legal entity is one party: the survivor stands for it now")
	assert.Nil(t, out.Aliases[0].LegalEntityID)

	sp, err := f.s.SplitParties(f.ctx, c.CaseID, domain.SplitPartiesRequest{MergedPartyID: b.PartyID,
		ExpectedMasterVersions: map[string]int64{b.PartyID: out.Aliases[0].MasterVersion, a.PartyID: out.Survivor.MasterVersion},
		Reason:                 "evidence shows two companies", EvidenceRef: "registry-extract-10"}, splitter)
	require.NoError(t, err)
	require.NotNil(t, sp.Restored.LegalEntityID)
	assert.Equal(t, f.entityID, *sp.Restored.LegalEntityID)
	assert.Nil(t, sp.Survivor.LegalEntityID)
}

func TestSplit_CorrectsAMergeWithNewLineageAndErasesNothing(t *testing.T) { // s18.2 step 7
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	merged, err := f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
	require.NoError(t, err)

	split := func(actor string, versions map[string]int64) (*registry.SplitOutcome, error) {
		return f.s.SplitParties(f.ctx, c.CaseID, domain.SplitPartiesRequest{MergedPartyID: b.PartyID, ExpectedMasterVersions: versions,
			Reason: "evidence shows two companies", EvidenceRef: "registry-extract-10", CorrelationID: "corr-split"}, actor)
	}
	versions := map[string]int64{b.PartyID: merged.Aliases[0].MasterVersion, a.PartyID: merged.Survivor.MasterVersion}

	_, err = split(merger, versions)
	assert.ErrorIs(t, err, registry.ErrSelfApproval, "whoever merged cannot undo it")
	_, err = split(splitter, map[string]int64{b.PartyID: 1, a.PartyID: 1})
	assert.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.SplitParties(f.ctx, uuid.NewString(), domain.SplitPartiesRequest{MergedPartyID: b.PartyID, ExpectedMasterVersions: versions,
		Reason: "r", EvidenceRef: "e"}, splitter)
	assert.ErrorIs(t, err, registry.ErrNotFound)
	assert.Equal(t, domain.PartyMerged, f.fresh(t, b.PartyID).Status, "a refused split changes nothing")

	out, err := split(splitter, versions)
	require.NoError(t, err)
	assert.Equal(t, domain.PartyActive, out.Restored.Status, "the party is what it was before the merge")
	assert.Nil(t, out.Restored.MergedInto)
	assert.Equal(t, domain.LineageSplit, out.Lineage.EventType)
	assert.Equal(t, merger, out.Lineage.IndependentOf)

	// the identifier went back; the merge and the split are both on the record
	found, _ := f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeLEI, "", "LEI-B")
	require.Len(t, found, 1)
	assert.Equal(t, b.PartyID, found[0].PartyID)
	h, err := f.s.ExplainMergeHistory(f.ctx, a.PartyID)
	require.NoError(t, err)
	require.Len(t, h.Lineage, 2, "the error stays in the lineage")
	assert.Equal(t, []string{domain.LineageMerge, domain.LineageSplit}, []string{h.Lineage[0].EventType, h.Lineage[1].EventType})
	assert.Empty(t, h.AliasIDs)
	assert.Equal(t, 1, outboxCount(t, f, "party.split"))

	_, err = split(splitter, map[string]int64{b.PartyID: out.Restored.MasterVersion, a.PartyID: out.Survivor.MasterVersion})
	assert.ErrorIs(t, err, registry.ErrStateConflict, "already split")

	_, err = f.pool.Exec(context.Background(), `DELETE FROM party_lineage`)
	assert.Error(t, err, "lineage is never deleted")
	_, err = f.pool.Exec(context.Background(), `UPDATE party_lineage SET reason = 'edited'`)
	assert.Error(t, err, "and never edited")
	_, err = f.pool.Exec(context.Background(), `INSERT INTO party_lineage (lineage_id, tenant_id, event_type, case_id, survivor_id, party_id, actor, independent_of, reason, evidence_ref)
		VALUES (gen_random_uuid(), $1, 'MERGE', $2, $3, $4, 'same', 'same', 'r', 'e')`, f.tenantID, c.CaseID, a.PartyID, b.PartyID)
	assert.Error(t, err, "the database refuses a decision that is not independent")
}

func TestMerge_TwoStewardsMergingOneCaseAtOnceOneWins(t *testing.T) {
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_lineage WHERE event_type = 'MERGE'`))
}

func TestDecide_DuplicateReview(t *testing.T) {
	f := newORGFixture(t)
	first := f.createParty(t, createReq("r1", "Nova Holdings Limited")).Party
	held := createReq("held-1", "Nova Holdings Ltd")
	res := f.createParty(t, held)
	require.Equal(t, domain.OutcomeReviewRequired, res.Outcome)
	caseID := res.Case.CaseID

	decide := func(actor string, req domain.CaseDecisionRequest) (*registry.CaseDecisionOutcome, error) {
		if req.ExpectedCaseVersion == 0 {
			req.ExpectedCaseVersion = 1
		}
		if req.Note == "" {
			req.Note = "reviewed"
		}
		return f.s.DecideResolutionCase(f.ctx, caseID, req, actor, false)
	}
	_, err := decide("p-steward", domain.CaseDecisionRequest{Decision: domain.DecisionCreateNew})
	assert.ErrorIs(t, err, registry.ErrSelfApproval, "the opener cannot decide")
	_, err = decide("p-reviewer", domain.CaseDecisionRequest{Decision: domain.DecisionAcceptProposed})
	assert.ErrorIs(t, err, registry.ErrInvalidInput, "not a decision for this kind of case")
	_, err = decide("p-reviewer", domain.CaseDecisionRequest{Decision: domain.DecisionLinkExisting, PartyID: uuid.NewString()})
	assert.ErrorIs(t, err, registry.ErrInvalidInput, "not one of the candidates")
	_, err = decide("p-reviewer", domain.CaseDecisionRequest{Decision: domain.DecisionDismiss, ExpectedCaseVersion: 9})
	assert.ErrorIs(t, err, registry.ErrVersionConflict)
	assert.Equal(t, 1, partyCount(t, f))

	out, err := decide("p-reviewer", domain.CaseDecisionRequest{Decision: domain.DecisionCreateNew})
	require.NoError(t, err)
	require.NotNil(t, out.Party)
	assert.NotEqual(t, first.PartyID, out.Party.PartyID)
	assert.Equal(t, "p-steward", out.Party.CreatedBy, "the party is the requester's, decided by the reviewer")
	assert.Equal(t, domain.CaseResolved, out.Case.Status)
	assert.Equal(t, 2, partyCount(t, f))

	replay := f.createParty(t, held)
	assert.True(t, replay.Replayed)
	require.NotNil(t, replay.Party, "a replay of the held request answers with the party the steward settled on")
	assert.Equal(t, out.Party.PartyID, replay.Party.PartyID)

	_, err = decide("p-other", domain.CaseDecisionRequest{Decision: domain.DecisionDismiss, ExpectedCaseVersion: 2})
	assert.ErrorIs(t, err, registry.ErrStateConflict, "a case is decided once")
	assert.Equal(t, 1, outboxCount(t, f, "master.conflict.resolved"))
}

func TestDecide_LinkExistingAndDismiss(t *testing.T) {
	f := newORGFixture(t)
	first := f.createParty(t, createReq("r1", "Nova Holdings Limited")).Party
	r1 := f.createParty(t, createReq("held-1", "Nova Holdings Ltd"))
	out, err := f.s.DecideResolutionCase(f.ctx, r1.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionLinkExisting,
		PartyID: first.PartyID, Note: "same company", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	assert.Equal(t, first.PartyID, out.Party.PartyID)
	assert.Equal(t, 1, partyCount(t, f), "linking creates nothing")

	r2 := f.createParty(t, createReq("held-2", "Nova Holding Co"))
	out, err = f.s.DecideResolutionCase(f.ctx, r2.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionDismiss,
		Note: "request withdrawn", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CaseDismissed, out.Case.Status)
	assert.Nil(t, out.Party)
	assert.Equal(t, 1, partyCount(t, f))
}

func TestDecide_FactConflict(t *testing.T) { // s19: a verified fact is replaced only by a decision
	f := newORGFixture(t)
	p := f.createParty(t, createReq("r1", "Nova Holdings Limited")).Party
	propose := func(version int64, name string) *registry.PartyFactOutcome {
		out, err := f.s.ProposePartyFact(f.ctx, p.PartyID, domain.ProposeFactRequest{FactType: domain.FactLegalName, Value: name,
			ValidFrom: "2025-06-01", SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "gazette", ExpectedMasterVersion: version}, "p-steward", false)
		require.NoError(t, err)
		require.NotNil(t, out.Case)
		return out
	}
	before := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)

	first := propose(1, "Nova Group Limited")
	_, err := f.s.DecideResolutionCase(f.ctx, first.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionAcceptProposed,
		Note: "x", ExpectedCaseVersion: 1}, "p-steward", false)
	assert.ErrorIs(t, err, registry.ErrSelfApproval)

	out, err := f.s.DecideResolutionCase(f.ctx, first.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionAcceptProposed,
		Note: "gazette confirms the change", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	assert.Greater(t, out.Party.MasterVersion, int64(2))

	nameAt := func(asOf string, knownAt time.Time) string {
		d, _ := domain.ParseDate(asOf)
		v, err := f.s.GetPartyView(f.ctx, p.PartyID, d, knownAt, false)
		require.NoError(t, err)
		if len(v.Names) != 1 {
			return ""
		}
		return v.Names[0].Value
	}
	now := time.Now().UTC()
	assert.Equal(t, "Nova Group Limited", nameAt("2025-09-01", now), "the accepted fact is the answer from its start date")
	assert.Equal(t, "Nova Holdings Limited", nameAt("2025-03-01", now), "and the old name still answers before it")
	assert.Equal(t, "Nova Holdings Limited", nameAt("2025-09-01", before), "as it was known before the decision")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_facts WHERE party_id = $1 AND status = 'PROMOTED'`, p.PartyID), "the proposal stays on record")
	assert.Equal(t, 1, outboxCount(t, f, "party.fact.published"))

	// keeping the verified fact rejects the proposal
	cur := f.fresh(t, p.PartyID)
	second, err := f.s.ProposePartyFact(f.ctx, p.PartyID, domain.ProposeFactRequest{FactType: domain.FactLegalName, Value: "Nova Wrong Name",
		ValidFrom: "2025-12-01", SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "gazette", ExpectedMasterVersion: cur.MasterVersion}, "p-steward", false)
	require.NoError(t, err)
	require.NotNil(t, second.Case)
	_, err = f.s.DecideResolutionCase(f.ctx, second.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionKeepVerified,
		Note: "the gazette entry was withdrawn", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	assert.Equal(t, "Nova Group Limited", nameAt("2026-01-01", time.Now().UTC()))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_facts WHERE party_id = $1 AND status = 'REJECTED'`, p.PartyID))
}

func TestDecide_IdentifierConflict(t *testing.T) {
	f := newORGFixture(t)
	a := f.createParty(t, createReq("ra", "Alpha Limited")).Party
	b := f.createParty(t, createReq("rb", "Bravo Limited")).Party
	link := func(p *domain.Party, v int64) *registry.PartyIdentifierOutcome {
		out, err := f.s.LinkPartyIdentifier(f.ctx, p.PartyID, domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
			Scheme: domain.SchemeVAT, Scope: "GB", Value: "GB999", SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "e"},
			ExpectedMasterVersion: v}, "p-steward", false)
		require.NoError(t, err)
		return out
	}
	link(a, 1)
	clash := link(b, 1)
	require.NotNil(t, clash.Case)

	out, err := f.s.DecideResolutionCase(f.ctx, clash.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionMoveToRequester,
		Note: "the registry shows Bravo owns it", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	assert.Equal(t, b.PartyID, out.Party.PartyID)
	found, _ := f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeVAT, "GB", "GB999")
	require.Len(t, found, 1)
	assert.Equal(t, b.PartyID, found[0].PartyID, "the identifier moved to the requester")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM party_identifiers WHERE party_id = $1 AND ended_reason = 'MOVED_BY_DECISION'`, a.PartyID))

	// keep holder: nothing moves
	c := f.party(t, "rc", "Charlie Limited", "LEI-C")
	d := f.party(t, "rd", "Delta Limited", "LEI-D")
	clash2, err := f.s.LinkPartyIdentifier(f.ctx, d.PartyID, domain.LinkIdentifierRequest{PartyIdentifierInput: domain.PartyIdentifierInput{
		Scheme: domain.SchemeLEI, Value: "LEI-C", SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "e"}, ExpectedMasterVersion: d.MasterVersion}, "p-steward", false)
	require.NoError(t, err)
	require.NotNil(t, clash2.Case)
	_, err = f.s.DecideResolutionCase(f.ctx, clash2.Case.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionKeepHolder, Note: "holder is right", ExpectedCaseVersion: 1}, "p-reviewer", false)
	require.NoError(t, err)
	found, _ = f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeLEI, "", "LEI-C")
	assert.Equal(t, c.PartyID, found[0].PartyID)
}

func TestMatch_VerdictsAndExplanation(t *testing.T) {
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	m := func(r domain.MatchPartiesRequest) *domain.MatchPartiesResult {
		out, err := f.s.MatchParties(f.ctx, r)
		require.NoError(t, err)
		return out
	}
	safe := m(domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity, Identifiers: []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "lei a"}}})
	assert.Equal(t, "SAFE_LINK", safe.Verdict)
	require.Len(t, safe.Candidates, 1)
	assert.Equal(t, a.PartyID, safe.Candidates[0].PartyID)
	assert.Equal(t, domain.TierA, safe.Candidates[0].Tier)
	assert.NotEmpty(t, safe.Candidates[0].Basis, "explainable")

	review := m(domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity, Country: "GB", Names: []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Alpha Ltd"}}})
	assert.Equal(t, "REVIEW_REQUIRED", review.Verdict)
	assert.Equal(t, domain.CaseDuplicateReview, review.CaseType)

	none := m(domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity, Names: []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Quantum Freight"}}})
	assert.Equal(t, "NO_MATCH", none.Verdict)
	assert.Empty(t, none.Candidates)
	assert.Equal(t, 1, partyCount(t, f), "a match writes nothing")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM resolution_cases`))

	other := secondTenant(t, f)
	cross, err := f.s.MatchParties(other.ctx, domain.MatchPartiesRequest{PartyKind: domain.PartyLegalEntity, Names: []domain.PartyNameInput{{Type: domain.FactLegalName, Value: "Alpha Limited"}}})
	require.NoError(t, err)
	assert.Equal(t, "NO_MATCH", cross.Verdict, "no cross-tenant leakage")
}

func TestCases_ListFilterAndPage(t *testing.T) {
	f := newORGFixture(t)
	f.createParty(t, createReq("r1", "Nova Holdings Limited"))
	var ids []string
	for i, n := range []string{"Nova Holdings Ltd", "Nova Holding Co", "Nova Holdings Inc"} {
		res := f.createParty(t, createReq("held-"+string(rune('a'+i)), n))
		require.Equal(t, domain.OutcomeReviewRequired, res.Outcome)
		ids = append(ids, res.Case.CaseID)
		time.Sleep(5 * time.Millisecond)
	}
	all, next, err := f.s.ListResolutionCases(f.ctx, "", "", 10, "")
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Empty(t, next)
	assert.Equal(t, ids, []string{all[0].CaseID, all[1].CaseID, all[2].CaseID}, "oldest first")

	var seen []string
	after := ""
	for i := 0; i < 5; i++ {
		page, nxt, err := f.s.ListResolutionCases(f.ctx, "OPEN", "DUPLICATE_REVIEW", 2, after)
		require.NoError(t, err)
		for _, c := range page {
			seen = append(seen, c.CaseID)
		}
		if nxt == "" {
			break
		}
		after = nxt
	}
	assert.Equal(t, ids, seen, "paging returns each case once")

	_, _, err = f.s.ListResolutionCases(f.ctx, "", "", 2, "garbage")
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
	none, _, err := f.s.ListResolutionCases(f.ctx, "RESOLVED", "", 10, "")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestResolution_TenantIsolation(t *testing.T) {
	f := newORGFixture(t)
	f.party(t, "ra", "Alpha Limited", "LEI-A")
	f.party(t, "rb", "Bravo Limited", "LEI-B")
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	other := secondTenant(t, f)

	_, err := other.s.MergeParties(other.ctx, c.CaseID, domain.MergePartiesRequest{SurvivorPartyID: uuid.NewString(),
		MergedPartyIDs: []string{uuid.NewString()}, Reason: "r", EvidenceRef: "e",
		ExpectedMasterVersions: map[string]int64{}}, merger)
	assert.Error(t, err)
	_, err = other.s.DecideResolutionCase(other.ctx, c.CaseID, domain.CaseDecisionRequest{Decision: domain.DecisionDismiss, Note: "n", ExpectedCaseVersion: 1}, merger, false)
	assert.ErrorIs(t, err, registry.ErrNotFound, "another tenant's case is not found")
	list, _, err := other.s.ListResolutionCases(other.ctx, "", "", 10, "")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestMerge_APartyThatIsNotLiveIsNeverMerged(t *testing.T) {
	f := newORGFixture(t)
	a := f.party(t, "ra", "Alpha Limited", "LEI-A")
	b := f.party(t, "rb", "Bravo Limited", "LEI-B")
	c := f.conflictCase(t, "LEI-A", "LEI-B")
	_, err := f.pool.Exec(context.Background(), `UPDATE parties SET status = 'INACTIVE' WHERE party_id = $1`, b.PartyID)
	require.NoError(t, err)

	_, err = f.s.MergeParties(f.ctx, c.CaseID, mergeReq(a, b), merger)
	require.ErrorIs(t, err, registry.ErrMergeProhibited)
	assert.Contains(t, err.Error(), "INACTIVE")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM party_lineage`))
}
