package store_test

// ORG-07 parties against a real Postgres: the MDM-01 s10.1 create flow (steps 1-7), the table's CHECKs and
// guards, tenant isolation and the races the per-tenant and per-party locks exist for.

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

func createReq(id, name string) domain.CreatePartyRequest {
	return domain.CreatePartyRequest{
		CreateRequestID: id, PartyKind: domain.PartyLegalEntity,
		Names:       []domain.PartyNameInput{{Type: domain.FactLegalName, Value: name}},
		Country:     "GB",
		SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "registry-extract", ValidFrom: "2025-01-01",
		CorrelationID: "corr-" + id,
	}
}

func (f *orgFixture) createParty(t *testing.T, req domain.CreatePartyRequest) *domain.CreatePartyResult {
	t.Helper()
	res, err := f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: req, Actor: "p-steward", Fingerprint: req.Fingerprint()})
	require.NoError(t, err)
	return res
}

func partyCount(t *testing.T, f *orgFixture) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM parties WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func TestParty_CreateFlow_NewPartyIsReservedWithSourcedFactsAndAnEvent(t *testing.T) { // s10.1 steps 6 and 7
	f := newORGFixture(t)
	req := createReq("req-1", "Nova Holdings Limited")
	req.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeCompanyRegistration, Scope: "GB", Value: "12 345 678"}}
	req.SourceSystem, req.SourceRef = "crm", "C-42"
	res := f.createParty(t, req)

	require.Equal(t, domain.OutcomeCreated, res.Outcome)
	require.NotNil(t, res.Party)
	assert.Equal(t, domain.PartyActive, res.Party.Status, "an authority source verifies on arrival")
	assert.Equal(t, int64(1), res.Party.MasterVersion)
	assert.Equal(t, "p-steward", res.Party.CreatedBy)

	v, err := f.s.GetPartyView(f.ctx, res.Party.PartyID, time.Now().UTC(), time.Now().UTC(), false)
	require.NoError(t, err)
	require.NotNil(t, v)
	require.Len(t, v.Names, 1)
	assert.Equal(t, "Nova Holdings Limited", v.Names[0].Value, "the source value is kept, not the comparison form")
	assert.Equal(t, domain.FactVerified, v.Names[0].Status)
	assert.Equal(t, "registry-extract", v.Names[0].EvidenceRef)
	require.Len(t, v.Facts, 1)
	assert.Equal(t, domain.FactCountry, v.Facts[0].FactType)
	assert.Len(t, v.Identifiers, 2, "the registration number and the source-system reference")
	assert.NotContains(t, v.QualityFlags, "NO_VERIFIED_LEGAL_NAME")

	assert.Equal(t, 1, outboxCount(t, f, "party.created"), "step 7: party_created through the outbox")
}

func TestParty_CreateFlow_NonAuthoritativeSourceIsACandidateWithProposedFacts(t *testing.T) {
	f := newORGFixture(t)
	req := createReq("req-1", "Nova Holdings Limited")
	req.SourceClass = domain.SourceTenantAsserted
	res := f.createParty(t, req)
	assert.Equal(t, domain.PartyCandidate, res.Party.Status)

	v, err := f.s.GetPartyView(f.ctx, res.Party.PartyID, time.Now().UTC(), time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Empty(t, v.Names, "an asserted name is not yet the party's name")
	require.Len(t, v.PendingFacts, 2)
	assert.Contains(t, v.QualityFlags, "NO_VERIFIED_LEGAL_NAME")
	assert.Contains(t, v.QualityFlags, "UNVERIFIED_FACTS_PENDING")
}

func TestParty_CreateFlow_ReplayReturnsTheFirstAnswer(t *testing.T) { // idempotent on create_request_id
	f := newORGFixture(t)
	req := createReq("req-1", "Nova Holdings Limited")
	first := f.createParty(t, req)
	again := f.createParty(t, req)

	assert.True(t, again.Replayed)
	assert.Equal(t, domain.OutcomeCreated, again.Outcome)
	assert.Equal(t, first.Party.PartyID, again.Party.PartyID)
	assert.Equal(t, 1, partyCount(t, f))
	assert.Equal(t, 1, outboxCount(t, f, "party.created"), "a replay emits nothing")

	other := createReq("req-1", "Different Name Plc")
	_, err := f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: other, Actor: "p", Fingerprint: other.Fingerprint()})
	assert.ErrorIs(t, err, registry.ErrCreateRequestMismatch, "the same id carrying another request is refused")
	assert.Equal(t, 1, partyCount(t, f))
}

func TestParty_CreateFlow_VerifiedIdentifierReturnsTheExistingParty(t *testing.T) { // s10.1 step 4
	f := newORGFixture(t)
	a := createReq("req-1", "Nova Holdings Limited")
	a.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeCompanyRegistration, Scope: "GB", Value: "12345678"}}
	first := f.createParty(t, a)

	b := createReq("req-2", "Totally Different Trading Name")
	b.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeCompanyRegistration, Scope: "GB", Value: "12-345-678"}}
	res := f.createParty(t, b)

	assert.Equal(t, domain.OutcomeMatchedExisting, res.Outcome)
	assert.Equal(t, first.Party.PartyID, res.Party.PartyID)
	require.NotNil(t, res.Match)
	assert.Equal(t, domain.TierA, res.Match.Tier)
	assert.Equal(t, 1, partyCount(t, f), "no duplicate was created")

	replay := f.createParty(t, b)
	assert.True(t, replay.Replayed)
	assert.Equal(t, domain.OutcomeMatchedExisting, replay.Outcome)
	assert.Equal(t, first.Party.PartyID, replay.Party.PartyID)
}

func TestParty_CreateFlow_SourceSystemReferenceDedupsAnIngest(t *testing.T) {
	f := newORGFixture(t)
	a := createReq("ingest-1", "Nova Holdings Limited")
	a.SourceSystem, a.SourceRef = "crm", "C-42"
	first := f.createParty(t, a)

	b := createReq("ingest-2", "Nova Holdings Ltd (renamed in the CRM)")
	b.SourceSystem, b.SourceRef = "crm", "C-42"
	res := f.createParty(t, b)
	assert.Equal(t, domain.OutcomeMatchedExisting, res.Outcome)
	assert.Equal(t, first.Party.PartyID, res.Party.PartyID)

	c := createReq("ingest-3", "Quantum Freight")
	c.SourceSystem, c.SourceRef = "erp", "C-42"
	assert.Equal(t, domain.OutcomeCreated, f.createParty(t, c).Outcome, "the same reference in another system is another party")
}

func TestParty_CreateFlow_AmbiguityOpensACaseAndCreatesNothing(t *testing.T) { // s10.1 step 5
	f := newORGFixture(t)
	first := f.createParty(t, createReq("req-1", "Nova Holdings Limited"))

	for name, mutate := range map[string]func(*domain.CreatePartyRequest){
		"same name and country (tier B)": func(r *domain.CreatePartyRequest) {},
		"near name (tier C)":             func(r *domain.CreatePartyRequest) { r.Names[0].Value = "Nova Holding Ltd" },
		"same name, another country":     func(r *domain.CreatePartyRequest) { r.Country = "IE" },
	} {
		req := createReq("req-"+name, "Nova Holdings Ltd")
		mutate(&req)
		res := f.createParty(t, req)
		require.Equal(t, domain.OutcomeReviewRequired, res.Outcome, name)
		require.NotNil(t, res.Case, name)
		assert.Equal(t, domain.CaseDuplicateReview, res.Case.CaseType)
		assert.Equal(t, domain.CaseOpen, res.Case.Status)
		require.NotEmpty(t, res.Candidates)
		assert.Equal(t, first.Party.PartyID, res.Candidates[0].PartyID)
		assert.Nil(t, res.Party, "no party was guessed")
	}
	assert.Equal(t, 1, partyCount(t, f), "ambiguity never creates a duplicate")
	assert.Equal(t, 3, outboxCount(t, f, "master.conflict.opened"))

	req := createReq("req-replay", "Nova Holdings Ltd")
	a, b := f.createParty(t, req), f.createParty(t, req)
	assert.Equal(t, a.Case.CaseID, b.Case.CaseID, "a replay returns the same case")
	assert.True(t, b.Replayed)

	got, err := f.s.GetResolutionCase(f.ctx, a.Case.CaseID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, a.Case.Candidates, got.Candidates)

	var held int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM resolution_cases WHERE case_id = $1 AND held_request IS NOT NULL`, a.Case.CaseID).Scan(&held))
	assert.Equal(t, 1, held, "the held request is kept for the steward")
}

func TestParty_CreateFlow_ConflictingIdentifiersAreTierD(t *testing.T) {
	f := newORGFixture(t)
	a := createReq("req-a", "Alpha Limited")
	a.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "LEI-A"}}
	b := createReq("req-b", "Bravo Limited")
	b.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "LEI-B"}}
	pa, pb := f.createParty(t, a), f.createParty(t, b)

	c := createReq("req-c", "Charlie Limited")
	c.Identifiers = []domain.PartyIdentifierInput{{Scheme: domain.SchemeLEI, Value: "LEI-A"}, {Scheme: domain.SchemeLEI, Value: "LEI-B"}}
	res := f.createParty(t, c)
	require.Equal(t, domain.OutcomeReviewRequired, res.Outcome)
	assert.Equal(t, domain.CaseIdentifierConflict, res.Case.CaseType)
	ids := []string{res.Candidates[0].PartyID, res.Candidates[1].PartyID}
	assert.ElementsMatch(t, []string{pa.Party.PartyID, pb.Party.PartyID}, ids)
	assert.Equal(t, 2, partyCount(t, f))
}

func TestParty_CreateFlow_ConcurrentCreatesOfOnePartyMakeOne(t *testing.T) { // never creates a duplicate
	f := newORGFixture(t)
	var wg sync.WaitGroup
	results := make([]*domain.CreatePartyResult, 16)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together
			// No identifier: nothing but the lock stands between two creates of one name and a duplicate.
			req := createReq(uuid.NewString(), "Nova Holdings Limited")
			res, err := f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: req, Actor: "p", Fingerprint: req.Fingerprint()})
			if err == nil {
				results[i] = res
			}
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	for _, r := range results {
		require.NotNil(t, r)
		if r.Outcome == domain.OutcomeCreated {
			created++
		}
	}
	assert.Equal(t, 1, created)
	assert.Equal(t, 1, partyCount(t, f))
}

func TestParty_CreateFlow_LegalEntityLinkIsOneToOne(t *testing.T) {
	f := newORGFixture(t)
	req := createReq("req-1", "Original Name Ltd")
	req.LegalEntityID = f.entityID
	res := f.createParty(t, req)
	require.Equal(t, domain.OutcomeCreated, res.Outcome)
	require.NotNil(t, res.Party.LegalEntityID)
	assert.Equal(t, f.entityID, *res.Party.LegalEntityID)

	again := createReq("req-2", "A Name The Registry Does Not Know")
	again.LegalEntityID = f.entityID
	got := f.createParty(t, again)
	assert.Equal(t, domain.OutcomeMatchedExisting, got.Outcome, "a legal entity is one party, whatever its names")
	assert.Equal(t, res.Party.PartyID, got.Party.PartyID)

	other := createReq("req-3", "Elsewhere Ltd")
	other.LegalEntityID = uuid.NewString()
	_, err := f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: other, Actor: "p", Fingerprint: other.Fingerprint()})
	assert.Error(t, err, "a party cannot stand for a legal entity that does not exist")

	// An entity that exists in ANOTHER tenant is refused too: a plain foreign key would accept it.
	foreign := secondTenant(t, f)
	cross := createReq("req-4", "Cross Tenant Ltd")
	cross.LegalEntityID = foreign.entityID
	_, err = f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: cross, Actor: "p", Fingerprint: cross.Fingerprint()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "own tenant")
}

func TestParty_Facts_AVerifiedFactIsNeverOverwritten(t *testing.T) { // s19: never silently overwrites
	f := newORGFixture(t)
	p := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party

	propose := func(version int64, ft domain.FactType, value, from, class string) *registry.PartyFactOutcome {
		out, err := f.s.ProposePartyFact(f.ctx, p.PartyID, domain.ProposeFactRequest{
			FactType: ft, Value: value, ValidFrom: from, SourceClass: domain.SourceClass(class),
			EvidenceRef: "doc-" + value, ExpectedMasterVersion: version}, "p-steward", false)
		require.NoError(t, err)
		return out
	}

	// restating the verified fact is not a change
	same := propose(1, domain.FactLegalName, "NOVA HOLDINGS LTD", "2025-01-01", "AUTHORITY_REGISTRY")
	assert.True(t, same.NoOp)
	assert.Equal(t, int64(1), same.Party.MasterVersion)

	// a contradicting fact, even from an authority, is held and a case is opened
	out := propose(1, domain.FactLegalName, "Nova Group Limited", "2025-06-01", "AUTHORITY_REGISTRY")
	assert.Equal(t, domain.FactProposed, out.Fact.Status)
	require.NotNil(t, out.Case)
	assert.Equal(t, domain.CaseFactConflict, out.Case.CaseType)
	assert.Equal(t, int64(2), out.Party.MasterVersion)

	v, err := f.s.GetPartyView(f.ctx, p.PartyID, time.Now().UTC(), time.Now().UTC(), false)
	require.NoError(t, err)
	require.Len(t, v.Names, 1)
	assert.Equal(t, "Nova Holdings Limited", v.Names[0].Value, "the verified name stands")
	require.Len(t, v.PendingFacts, 1)
	assert.Contains(t, v.QualityFlags, "OPEN_RESOLUTION_CASE")
	assert.Equal(t, 1, outboxCount(t, f, "master.conflict.opened"))

	// a tenant-asserted trading name is held for a steward, and co-exists with the legal name
	tn := propose(2, domain.FactTradingName, "Nova", "2025-01-01", "TENANT_ASSERTED")
	assert.Equal(t, domain.FactProposed, tn.Fact.Status)
	assert.Nil(t, tn.Case, "a non-conflicting assertion is pending, not a dispute")

	// an authority trading name verifies
	auth := propose(3, domain.FactTradingName, "Nova Group", "2025-01-01", "AUTHORITY_DOCUMENT")
	assert.Equal(t, domain.FactVerified, auth.Fact.Status)
	assert.Equal(t, 1, outboxCount(t, f, "party.fact.published"), "only the verified trading name is published as a fact; creation is party.created")
}

func TestParty_Facts_ExpectedMasterVersionIsRequiredAndChecked(t *testing.T) { // s19.1
	f := newORGFixture(t)
	p := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party
	req := domain.ProposeFactRequest{FactType: domain.FactTradingName, Value: "Nova", ValidFrom: "2025-01-01",
		SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e"}

	_, err := f.s.ProposePartyFact(f.ctx, p.PartyID, req, "p", false)
	assert.ErrorIs(t, err, registry.ErrVersionRequired)

	req.ExpectedMasterVersion = 7
	_, err = f.s.ProposePartyFact(f.ctx, p.PartyID, req, "p", false)
	assert.ErrorIs(t, err, registry.ErrVersionConflict, "last write wins is prohibited")

	req.ExpectedMasterVersion = 1
	_, err = f.s.ProposePartyFact(f.ctx, p.PartyID, req, "p", false)
	require.NoError(t, err)
	_, err = f.s.ProposePartyFact(f.ctx, p.PartyID, req, "p", false)
	assert.ErrorIs(t, err, registry.ErrVersionConflict, "the stale writer must reload")

	_, err = f.s.ProposePartyFact(f.ctx, uuid.NewString(), req, "p", false)
	assert.ErrorIs(t, err, registry.ErrNotFound)
}

func TestParty_Facts_TwoStewardsRaceOneWins(t *testing.T) {
	f := newORGFixture(t)
	p := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.s.ProposePartyFact(f.ctx, p.PartyID, domain.ProposeFactRequest{
				FactType: domain.FactDisplayName, Value: uuid.NewString(), ValidFrom: "2025-01-01",
				SourceClass: domain.SourceTenantAsserted, EvidenceRef: "e", ExpectedMasterVersion: 1}, "p", false)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			assert.ErrorIs(t, err, registry.ErrVersionConflict)
		}
	}
	assert.Equal(t, 1, ok)
}

func TestParty_AsOf_AnswersWhatWasTrueAndWhatWasKnown(t *testing.T) { // s8 figure 4: valid time and knowledge time
	f := newORGFixture(t)
	req := createReq("req-1", "Alpha Ltd")
	req.ValidFrom = "2025-01-01"
	p := f.createParty(t, req).Party
	beforeCorrection := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)

	// The authority later says the name was "Alpha Limited" from 2025-01-10 and "Alpha Ltd" only until then. The
	// record we held is superseded, never edited, and the corrected pair is recorded now.
	_, err := f.pool.Exec(context.Background(), `UPDATE party_facts SET status = 'SUPERSEDED', superseded_at = NOW()
		WHERE party_id = $1 AND fact_type = 'LEGAL_NAME'`, p.PartyID)
	require.NoError(t, err)
	_, err = f.pool.Exec(context.Background(), `INSERT INTO party_facts (fact_id, tenant_id, party_id, fact_type, value, value_normalized, tokens,
		valid_from, valid_to, status, source_class, evidence_ref, proposed_by, recorded_version)
		VALUES (gen_random_uuid(), $1, $2, 'LEGAL_NAME', 'Alpha Ltd', 'alpha', '{alpha}', '2025-01-01', '2025-01-10', 'VERIFIED', 'AUTHORITY_REGISTRY', 'e', 'p', 2),
		       (gen_random_uuid(), $1, $2, 'LEGAL_NAME', 'Alpha Limited', 'alpha', '{alpha}', '2025-01-10', NULL, 'VERIFIED', 'AUTHORITY_REGISTRY', 'e', 'p', 2)`,
		f.tenantID, p.PartyID)
	require.NoError(t, err)

	name := func(asOf string, knownAt time.Time) string {
		d, _ := domain.ParseDate(asOf)
		v, err := f.s.GetPartyView(f.ctx, p.PartyID, d, knownAt, false)
		require.NoError(t, err)
		if len(v.Names) == 0 {
			return ""
		}
		return v.Names[0].Value
	}
	now := time.Now().UTC()
	assert.Equal(t, "Alpha Ltd", name("2025-01-05", now))
	assert.Equal(t, "Alpha Limited", name("2025-02-01", now))
	assert.Equal(t, "", name("2024-12-01", now), "before the fact began there is no name")
	assert.Equal(t, "Alpha Ltd", name("2025-02-01", beforeCorrection), "as it was known before the correction arrived")

	early, err := f.s.GetPartyView(f.ctx, p.PartyID, now, p.CreatedAt.Add(-time.Hour), false)
	require.NoError(t, err)
	assert.Nil(t, early, "ZoikoSuite did not yet know the party")
}

func TestParty_Roles_AreExplicitTimeAwareAndNeverDeleted(t *testing.T) {
	f := newORGFixture(t)
	p := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party

	add, err := f.s.AddPartyRole(f.ctx, p.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleSupplier, ValidFrom: "2025-01-01",
		EvidenceRef: "contract-1", ExpectedMasterVersion: 1}, "p-steward", false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), add.Party.MasterVersion)

	_, err = f.s.AddPartyRole(f.ctx, p.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleSupplier, ValidFrom: "2025-02-01",
		EvidenceRef: "contract-2", ExpectedMasterVersion: 2}, "p", false)
	assert.ErrorIs(t, err, registry.ErrConflict, "one live role of a type")

	rm, err := f.s.RemovePartyRole(f.ctx, p.PartyID, add.Role.RoleID, domain.RemovePartyRoleRequest{
		EndDate: "2025-06-30", Reason: "contract ended", ExpectedMasterVersion: 2}, "p-steward", false)
	require.NoError(t, err)
	assert.Equal(t, "2025-06-30", rm.Role.ValidTo)
	_, err = f.s.RemovePartyRole(f.ctx, p.PartyID, add.Role.RoleID, domain.RemovePartyRoleRequest{
		EndDate: "2025-06-30", Reason: "again", ExpectedMasterVersion: 3}, "p", false)
	assert.ErrorIs(t, err, registry.ErrStateConflict)

	now := time.Now().UTC()
	d := func(s string) time.Time { x, _ := domain.ParseDate(s); return x }
	during, _ := f.s.GetPartyView(f.ctx, p.PartyID, d("2025-03-01"), now, false)
	after, _ := f.s.GetPartyView(f.ctx, p.PartyID, d("2025-09-01"), now, false)
	assert.Len(t, during.Roles, 1, "the role held in March")
	assert.Empty(t, after.Roles, "and not in September")

	// the removed row is history, and a new live role of the same type can be added
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM party_roles WHERE party_id = $1`, p.PartyID).Scan(&n))
	assert.Equal(t, 1, n)
	_, err = f.s.AddPartyRole(f.ctx, p.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleSupplier, ValidFrom: "2025-09-01",
		EvidenceRef: "contract-3", ExpectedMasterVersion: 3}, "p", false)
	require.NoError(t, err)

	_, err = f.pool.Exec(context.Background(), `DELETE FROM party_facts WHERE party_id = $1`, p.PartyID)
	assert.Error(t, err, "facts are never deleted")
	_, err = f.pool.Exec(context.Background(), `UPDATE party_facts SET value = 'Rewritten' WHERE party_id = $1`, p.PartyID)
	assert.Error(t, err, "a fact is immutable; it is superseded")
	_, err = f.pool.Exec(context.Background(), `UPDATE party_create_requests SET fingerprint = 'x'`)
	assert.Error(t, err)

	assert.Equal(t, 1, outboxCount(t, f, "party.role.removed"))
	assert.Equal(t, 2, outboxCount(t, f, "party.role.added"))
}

func TestParty_Identifiers_LinkFindMaskAndConflict(t *testing.T) {
	f := newORGFixture(t)
	a := f.createParty(t, createReq("req-a", "Alpha Limited")).Party
	b := f.createParty(t, createReq("req-b", "Bravo Limited")).Party

	link := func(partyID string, version int64, scheme domain.IdentifierScheme, value string) *registry.PartyIdentifierOutcome {
		out, err := f.s.LinkPartyIdentifier(f.ctx, partyID, domain.LinkIdentifierRequest{
			PartyIdentifierInput: domain.PartyIdentifierInput{Scheme: scheme, Scope: "GB", Value: value,
				SourceClass: domain.SourceAuthorityRegistry, EvidenceRef: "e"},
			ExpectedMasterVersion: version}, "p-steward", false)
		require.NoError(t, err)
		return out
	}

	out := link(a.PartyID, 1, domain.SchemeTaxID, "1234567890")
	require.NotNil(t, out.Identifier)
	assert.Equal(t, "VERIFIED", out.Identifier.VerificationStatus)
	assert.Equal(t, int64(2), out.Party.MasterVersion)

	again := link(a.PartyID, 2, domain.SchemeTaxID, "123-456-7890")
	assert.True(t, again.NoOp, "linking the same identifier again is not a change")
	assert.Equal(t, int64(2), again.Party.MasterVersion)

	// another party asking for the same identifier opens a case; nothing is linked
	clash := link(b.PartyID, 1, domain.SchemeTaxID, "1234567890")
	assert.Nil(t, clash.Identifier)
	require.NotNil(t, clash.Case)
	assert.Equal(t, domain.CaseIdentifierConflict, clash.Case.CaseType)
	assert.Len(t, clash.Case.Candidates, 2)
	assert.Equal(t, int64(1), clash.Party.MasterVersion, "a refused link changes nothing")

	found, err := f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeTaxID, "gb", "1234 5678 90")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, a.PartyID, found[0].PartyID)
	assert.Equal(t, "Alpha Limited", found[0].LegalName)
	none, _ := f.s.FindPartiesByIdentifier(f.ctx, domain.SchemeTaxID, "IE", "1234567890")
	assert.Empty(t, none, "scope is part of the identity")

	masked, _ := f.s.GetPartyView(f.ctx, a.PartyID, time.Now().UTC(), time.Now().UTC(), false)
	var shown string
	for _, i := range masked.Identifiers {
		if i.Scheme == domain.SchemeTaxID {
			assert.True(t, i.Masked)
			shown = i.Value
		}
	}
	assert.Equal(t, "******7890", shown, "a sensitive identifier is masked")
	revealed, _ := f.s.GetPartyView(f.ctx, a.PartyID, time.Now().UTC(), time.Now().UTC(), true)
	for _, i := range revealed.Identifiers {
		if i.Scheme == domain.SchemeTaxID {
			assert.Equal(t, "1234567890", i.Value)
			assert.False(t, i.Masked)
		}
	}

	assert.Equal(t, 1, outboxCount(t, f, "party.identifier.linked"))
	var payload string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT payload::text FROM event_outbox WHERE event_type = 'party.identifier.linked' AND tenant_id = $1`, f.tenantID).Scan(&payload))
	assert.NotContains(t, payload, "1234567890", "an event never carries an identifier value")
}

func TestParty_Search(t *testing.T) {
	f := newORGFixture(t)
	nova := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party
	f.createParty(t, createReq("req-2", "Zenith Traders Limited"))
	person := createReq("req-3", "Nova Person")
	person.PartyKind = domain.PartyNaturalPerson
	f.createParty(t, person)
	_, err := f.s.AddPartyRole(f.ctx, nova.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleCustomer, ValidFrom: "2025-01-01",
		EvidenceRef: "e", ExpectedMasterVersion: 1}, "p", false)
	require.NoError(t, err)

	names := func(q domain.PartySearchQuery) []string {
		out, _, err := f.s.SearchParties(f.ctx, q)
		require.NoError(t, err)
		var n []string
		for _, s := range out {
			n = append(n, s.LegalName)
		}
		return n
	}
	assert.ElementsMatch(t, []string{"Nova Holdings Limited", "Nova Person"}, names(domain.PartySearchQuery{Text: "nova"}))
	assert.Equal(t, []string{"Nova Holdings Limited"}, names(domain.PartySearchQuery{Text: "NOVA", Kind: domain.PartyLegalEntity}))
	assert.Equal(t, []string{"Nova Holdings Limited"}, names(domain.PartySearchQuery{Role: domain.RoleCustomer}))
	assert.Len(t, names(domain.PartySearchQuery{}), 3)
	assert.Empty(t, names(domain.PartySearchQuery{Text: "quantum"}))

	page1, next, err := f.s.SearchParties(f.ctx, domain.PartySearchQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.NotEmpty(t, next)
	page2, next2, err := f.s.SearchParties(f.ctx, domain.PartySearchQuery{Limit: 2, After: next})
	require.NoError(t, err)
	assert.Len(t, page2, 1)
	assert.Empty(t, next2)
	assert.NotEqual(t, page1[0].PartyID, page2[0].PartyID)

	_, _, err = f.s.SearchParties(f.ctx, domain.PartySearchQuery{After: "not-a-uuid"})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestParty_TenantIsolation(t *testing.T) {
	f := newORGFixture(t)
	p := f.createParty(t, createReq("req-1", "Nova Holdings Limited")).Party
	other := secondTenant(t, f)
	require.NotEqual(t, f.tenantID, other.tenantID)

	// the same create_request_id and the same name in another tenant is a new party there
	res := other.createParty(t, createReq("req-1", "Nova Holdings Limited"))
	assert.Equal(t, domain.OutcomeCreated, res.Outcome, "cross-tenant similarity is never a candidate")
	assert.NotEqual(t, p.PartyID, res.Party.PartyID)

	v, err := other.s.GetPartyView(other.ctx, p.PartyID, time.Now().UTC(), time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, v, "another tenant's party is not found")
	c, err := other.s.GetResolutionCase(other.ctx, uuid.NewString())
	require.NoError(t, err)
	assert.Nil(t, c)
	_, err = other.s.AddPartyRole(other.ctx, p.PartyID, domain.AddPartyRoleRequest{RoleType: domain.RoleCustomer, ValidFrom: "2025-01-01",
		EvidenceRef: "e", ExpectedMasterVersion: 1}, "p", false)
	assert.ErrorIs(t, err, registry.ErrNotFound)
}

// The races above are probabilistic, so this pins the mechanism itself: while another session holds the tenant's
// create lock, a create waits; it proceeds when the lock is released. A different tenant is not held up.
func TestParty_CreateFlow_WaitsForThePerTenantLock(t *testing.T) {
	f := newORGFixture(t)
	other := secondTenant(t, f)
	ctx := context.Background()

	conn, err := f.pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	lockSQL := `SELECT pg_advisory_lock(hashtextextended('party-create:' || $1::text, 0))`
	_, err = conn.Exec(ctx, lockSQL, f.tenantID)
	require.NoError(t, err)

	done := make(chan struct{})
	var createErr error
	go func() {
		defer close(done)
		req := createReq("req-lock", "Nova Holdings Limited")
		_, createErr = f.s.CreateParty(f.ctx, registry.PartyCreateCommand{Request: req, Actor: "p", Fingerprint: req.Fingerprint()})
	}()

	select {
	case <-done:
		t.Fatal("the create ran while another session held the tenant's create lock")
	case <-time.After(400 * time.Millisecond):
	}

	otherReq := createReq("req-other", "Nova Holdings Limited")
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		_, _ = other.s.CreateParty(other.ctx, registry.PartyCreateCommand{Request: otherReq, Actor: "p", Fingerprint: otherReq.Fingerprint()})
	}()
	select {
	case <-otherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("another tenant's create was held up by this tenant's lock")
	}

	_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('party-create:' || $1::text, 0))`, f.tenantID)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the create did not proceed once the lock was released")
	}
	require.NoError(t, createErr)
	assert.Equal(t, 1, partyCount(t, f))
}
