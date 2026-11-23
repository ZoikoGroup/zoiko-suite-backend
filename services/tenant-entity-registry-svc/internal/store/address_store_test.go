package store_test

// ORG-08 addresses against a real Postgres: structured source kept beside the normalized form, versions that are
// never edited, verification as an observation, the registered-office rule, links and tenant isolation.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func addrFields(line, locality, postal, country string) domain.AddressFields {
	return domain.AddressFields{
		Lines: []string{line}, Locality: locality, PostalCode: postal, Country: country,
		SourceClass: domain.SourceTenantAsserted, EvidenceRef: "onboarding-form", ValidFrom: "2025-01-01",
		CorrelationID: "corr-addr",
	}
}

func (f *orgFixture) address(t *testing.T, typ domain.AddressType, af domain.AddressFields) *registry.AddressOutcome {
	t.Helper()
	cmd := registry.AddressCreateCommand{Request: domain.CreateAddressRequest{AddressType: typ, AddressFields: af}, Actor: "p-steward"}
	cmd.Fingerprint = cmd.Request.Fingerprint()
	out, err := f.s.CreateAddress(f.ctx, cmd)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) verify(t *testing.T, id string, version int) *registry.VerifyOutcome {
	t.Helper()
	out, err := f.s.VerifyAddress(f.ctx, id, domain.VerifyAddressRequest{ExpectedVersion: version}, "p-verifier", false,
		registry.LocalAddressVerifier{}.Verify)
	require.NoError(t, err)
	return out
}

func TestAddress_CreateKeepsTheSourceAndAddsTheNormalizedForm(t *testing.T) {
	f := newORGFixture(t)
	af := addrFields("12 High Street", "London", "sw1a 1aa", "GB")
	af.Formatted = "12 High Street, London SW1A 1AA"
	out := f.address(t, domain.AddressBilling, af)

	v := out.Version
	assert.Equal(t, 1, v.VersionNo)
	assert.Equal(t, []string{"12 High Street"}, v.Lines, "the source line is kept as entered")
	assert.Equal(t, "sw1a 1aa", v.PostalCode, "the source postal code is kept as entered")
	assert.Equal(t, "12 HIGH ST|LONDON||SW1A1AA|GB", v.Normalized)
	assert.Equal(t, "12 High Street, London SW1A 1AA", v.Formatted)
	assert.Equal(t, domain.AddressUnverified, v.VerificationState)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'address.created' AND tenant_id = $1`, f.tenantID))
}

func TestAddress_CreateIsIdempotentOnTheRequestId(t *testing.T) {
	f := newORGFixture(t)
	req := domain.CreateAddressRequest{CreateRequestID: "addr-1", AddressType: domain.AddressOperating, AddressFields: addrFields("1 Main St", "Leeds", "LS1 1AA", "GB")}
	cmd := registry.AddressCreateCommand{Request: req, Actor: "p", Fingerprint: req.Fingerprint()}
	first, err := f.s.CreateAddress(f.ctx, cmd)
	require.NoError(t, err)
	assert.False(t, first.Replayed)

	again, err := f.s.CreateAddress(f.ctx, cmd)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Address.AddressID, again.Address.AddressID)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM addresses WHERE tenant_id = $1`, f.tenantID))

	other := req
	other.Locality = "York"
	_, err = f.s.CreateAddress(f.ctx, registry.AddressCreateCommand{Request: other, Actor: "p", Fingerprint: other.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrCreateRequestMismatch, "the same id carrying different content is refused")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM addresses WHERE tenant_id = $1`, f.tenantID))
}

func TestAddress_ConcurrentCreatesWithOneRequestIdMakeOneAddress(t *testing.T) {
	f := newORGFixture(t)
	req := domain.CreateAddressRequest{CreateRequestID: "race", AddressType: domain.AddressOperating, AddressFields: addrFields("1 Main St", "Leeds", "LS1 1AA", "GB")}
	cmd := registry.AddressCreateCommand{Request: req, Actor: "p", Fingerprint: req.Fingerprint()}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.CreateAddress(f.ctx, cmd)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM addresses WHERE tenant_id = $1`, f.tenantID))
}

func TestAddress_AmendAddsAVersionAndTheOldOneStaysReadable(t *testing.T) {
	f := newORGFixture(t)
	a := f.address(t, domain.AddressBilling, addrFields("1 Old Road", "Leeds", "LS1 1AA", "GB"))

	am := addrFields("2 New Road", "Leeds", "LS2 2BB", "GB")
	am.ValidFrom = "2026-03-01"
	out, err := f.s.AmendAddress(f.ctx, a.Address.AddressID, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "moved", AddressFields: am}, "p", false)
	require.NoError(t, err)
	assert.Equal(t, 2, out.Version.VersionNo)
	assert.Equal(t, 2, out.Address.CurrentVersion)

	past, err := f.s.GetAddress(f.ctx, a.Address.AddressID, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), true)
	require.NoError(t, err)
	require.NotNil(t, past.Version)
	assert.Equal(t, []string{"1 Old Road"}, past.Version.Lines, "as of a past date the old address is what was in force")
	assert.Equal(t, domain.AddressSuperseded, past.Version.VerificationState)
	assert.Equal(t, "2026-03-01", past.Version.ValidTo)

	now, err := f.s.GetAddress(f.ctx, a.Address.AddressID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), true)
	require.NoError(t, err)
	assert.Equal(t, []string{"2 New Road"}, now.Version.Lines)
	assert.Len(t, now.History, 2)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'address.superseded' AND tenant_id = $1`, f.tenantID))

	before, err := f.s.GetAddress(f.ctx, a.Address.AddressID, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	assert.Nil(t, before.Version, "before the first version began there was nothing in force")
}

func TestAddress_AmendChecksTheVersionAndRefusesWhatChangesNothingOrGoesBackwards(t *testing.T) {
	f := newORGFixture(t)
	a := f.address(t, domain.AddressBilling, addrFields("1 Old Road", "Leeds", "LS1 1AA", "GB"))
	id := a.Address.AddressID

	same := addrFields("1 Old Road", "Leeds", "LS1 1AA", "GB")
	noop, err := f.s.AmendAddress(f.ctx, id, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "r", AddressFields: same}, "p", false)
	require.NoError(t, err)
	assert.True(t, noop.NoOp, "identical content is not a new version")
	assert.Equal(t, 1, noop.Version.VersionNo)

	next := addrFields("2 New Road", "Leeds", "LS2 2BB", "GB")
	next.ValidFrom = "2026-01-01"
	_, err = f.s.AmendAddress(f.ctx, id, domain.AmendAddressRequest{ExpectedVersion: 9, Reason: "r", AddressFields: next}, "p", false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.AmendAddress(f.ctx, id, domain.AmendAddressRequest{Reason: "r", AddressFields: next}, "p", false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)

	back := next
	back.ValidFrom = "2024-01-01"
	_, err = f.s.AmendAddress(f.ctx, id, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "r", AddressFields: back}, "p", false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "an amendment must start after the current version")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM address_versions WHERE address_id = $1`, id))
}

func TestAddress_ConcurrentAmendmentsOneWins(t *testing.T) {
	f := newORGFixture(t)
	a := f.address(t, domain.AddressBilling, addrFields("1 Old Road", "Leeds", "LS1 1AA", "GB"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			am := addrFields(strings.Repeat("N", i+1)+" Road", "Leeds", "LS2 2BB", "GB")
			am.ValidFrom = "2026-01-01"
			_, err := f.s.AmendAddress(f.ctx, a.Address.AddressID, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "r", AddressFields: am}, "p", false)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if assert.ErrorIs(t, err, registry.ErrVersionConflict) {
				conflicts++
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	assert.Equal(t, 4, conflicts)
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM address_versions WHERE address_id = $1`, a.Address.AddressID))
}

func TestAddress_VerificationIsAnObservationAndNeverChangesTheAddress(t *testing.T) {
	f := newORGFixture(t)

	ok := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "ls1 1aa", "GB"))
	res := f.verify(t, ok.Address.AddressID, 1)
	assert.Equal(t, domain.VerifyPassed, res.Observation.Result)
	assert.Equal(t, domain.AddressVerified, res.Version.VerificationState)
	assert.Equal(t, []string{"1 Main Street"}, res.Version.Lines, "verifying never alters what was entered")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'address.verified' AND tenant_id = $1`, f.tenantID))

	bad := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "12345", "GB"))
	res = f.verify(t, bad.Address.AddressID, 1)
	assert.Equal(t, domain.VerifyFailed, res.Observation.Result)
	assert.Contains(t, res.Observation.Findings, "POSTAL_CODE_FORMAT")
	assert.Equal(t, domain.AddressUnverified, res.Version.VerificationState)

	low := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Nairobi", "00100", "KE"))
	res = f.verify(t, low.Address.AddressID, 1)
	assert.Equal(t, domain.VerifyLowConfidence, res.Observation.Result, "a country with no postal rule is surfaced, not passed")
	assert.Equal(t, domain.AddressUnverified, res.Version.VerificationState)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'address.verified' AND tenant_id = $1`, f.tenantID),
		"only a confident pass publishes address.verified")

	view, err := f.s.GetAddress(f.ctx, low.Address.AddressID, time.Now().UTC(), false)
	require.NoError(t, err)
	require.Len(t, view.Verifications, 1)
	assert.Equal(t, 0.5, view.Verifications[0].Confidence)

	// A later verification adds a row; it does not replace the first.
	f.verify(t, low.Address.AddressID, 1)
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM address_verifications WHERE address_id = $1`, low.Address.AddressID))

	_, err = f.s.VerifyAddress(f.ctx, low.Address.AddressID, domain.VerifyAddressRequest{ExpectedVersion: 7}, "p", false, registry.LocalAddressVerifier{}.Verify)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
}

func TestAddress_AmendingAVerifiedAddressStartsUnverifiedAgain(t *testing.T) {
	f := newORGFixture(t)
	a := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "LS1 1AA", "GB"))
	f.verify(t, a.Address.AddressID, 1)
	am := addrFields("9 Other Street", "Leeds", "LS1 1AA", "GB")
	am.ValidFrom = "2026-01-01"
	out, err := f.s.AmendAddress(f.ctx, a.Address.AddressID, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "moved", AddressFields: am}, "p", false)
	require.NoError(t, err)
	assert.Equal(t, domain.AddressUnverified, out.Version.VerificationState, "a verification does not carry to a new address")
}

func TestAddress_RegisteredNeedsAuthorityEvidenceOnAmendToo(t *testing.T) {
	f := newORGFixture(t)
	af := addrFields("1 Registry Way", "Leeds", "LS1 1AA", "GB")
	af.SourceClass = domain.SourceAuthorityRegistry
	a := f.address(t, domain.AddressRegistered, af)
	f.verify(t, a.Address.AddressID, 1)

	am := addrFields("2 Registry Way", "Leeds", "LS1 1AA", "GB")
	am.ValidFrom = "2026-01-01" // tenant asserted: not authority evidence
	_, err := f.s.AmendAddress(f.ctx, a.Address.AddressID, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "moved", AddressFields: am}, "p", false)
	require.ErrorIs(t, err, registry.ErrSourceUnverified)

	req := domain.CreateAddressRequest{AddressType: domain.AddressRegistered, AddressFields: addrFields("3 Way", "Leeds", "LS1 1AA", "GB")}
	require.ErrorIs(t, req.Validate(), domain.ErrRegisteredNeedsAuthority)
}

func TestAddress_DatabaseKeepsHistoryWhateverTheServiceDoes(t *testing.T) {
	f := newORGFixture(t)
	a := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "LS1 1AA", "GB"))
	f.verify(t, a.Address.AddressID, 1)
	ctx := context.Background()
	id := a.Address.AddressID

	// Row-level security applies to the owner too, so work inside the tenant.
	exec := func(q string, args ...any) error {
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, q, args...)
		return err
	}
	require.Error(t, exec(`UPDATE address_versions SET locality = 'Elsewhere' WHERE address_id = $1`, id), "content is immutable")
	require.Error(t, exec(`UPDATE address_versions SET lines = ARRAY['x'] WHERE address_id = $1`, id))
	require.Error(t, exec(`DELETE FROM address_versions WHERE address_id = $1`, id), "versions are never deleted")
	require.Error(t, exec(`UPDATE address_verifications SET result = 'PASSED' WHERE address_id = $1`, id), "observations are append-only")
	require.Error(t, exec(`DELETE FROM address_verifications WHERE address_id = $1`, id))
	require.Error(t, exec(`INSERT INTO addresses (address_id, tenant_id, address_type, created_by) VALUES (gen_random_uuid(), $1, 'registered office', 'x')`, f.tenantID),
		"an address type outside the list is refused")
	require.Error(t, exec(`INSERT INTO addresses (address_id, tenant_id, address_type, created_by) VALUES (gen_random_uuid(), $1, 'X_', 'x')`, f.tenantID))
	require.NoError(t, exec(`INSERT INTO addresses (address_id, tenant_id, address_type, created_by) VALUES (gen_random_uuid(), $1, 'X_ARCHIVE', 'x')`, f.tenantID),
		"a domain-safe custom type is allowed")
}

func TestAddress_SearchUsesTheNormalizedFormAndPages(t *testing.T) {
	f := newORGFixture(t)
	f.address(t, domain.AddressBilling, addrFields("12 High Street", "London", "SW1A 1AA", "GB"))
	f.address(t, domain.AddressOperating, addrFields("12, High St.", "London", "SW1A 1AA", "GB"))
	f.address(t, domain.AddressBilling, addrFields("5 Rue Haute", "Paris", "75001", "FR"))

	got, _, err := f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{Text: "12 high street"})
	require.NoError(t, err)
	assert.Len(t, got, 2, "street and st. compare equal in the normalized form")

	got, _, err = f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{Country: "FR"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Paris", got[0].Locality)

	got, _, err = f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{PostalCode: "sw1a1aa", Type: domain.AddressOperating})
	require.NoError(t, err)
	assert.Len(t, got, 1)

	page1, next, err := f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.NotEmpty(t, next)
	page2, next2, err := f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{Limit: 2, After: next})
	require.NoError(t, err)
	assert.Len(t, page2, 1)
	assert.Empty(t, next2)
	assert.NotEqual(t, page1[0].AddressID, page2[0].AddressID)

	_, _, err = f.s.SearchAddresses(f.ctx, domain.AddressSearchQuery{After: "garbage"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestAddress_TenantsNeverSeeEachOthersAddresses(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	mine := a.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "LS1 1AA", "GB"))

	v, err := b.s.GetAddress(b.ctx, mine.Address.AddressID, time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, v)
	got, _, err := b.s.SearchAddresses(b.ctx, domain.AddressSearchQuery{})
	require.NoError(t, err)
	assert.Empty(t, got)
	_, err = b.s.AmendAddress(b.ctx, mine.Address.AddressID, domain.AmendAddressRequest{ExpectedVersion: 1, Reason: "r",
		AddressFields: addrFields("9 Elsewhere", "Leeds", "LS1 1AA", "GB")}, "p", false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestAddress_LinkToPartyInTheAddressOwnCapacity(t *testing.T) {
	f := newORGFixture(t)
	p := f.party(t, "pa", "Alpha Limited", "LEI-A")
	a := f.address(t, domain.AddressBilling, addrFields("1 Main Street", "Leeds", "LS1 1AA", "GB"))

	out, err := f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: a.Address.AddressID, LinkType: domain.AddressBilling,
		ValidFrom: "2025-02-01", EvidenceRef: "contract", ExpectedMasterVersion: p.MasterVersion}, "p", false)
	require.NoError(t, err)
	assert.Equal(t, p.MasterVersion+1, out.Party.MasterVersion, "linking changes the party, so its version moves")

	again, err := f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: a.Address.AddressID, LinkType: domain.AddressBilling,
		ValidFrom: "2025-02-01", EvidenceRef: "contract", ExpectedMasterVersion: out.Party.MasterVersion}, "p", false)
	require.NoError(t, err)
	assert.True(t, again.NoOp)
	assert.Equal(t, out.Link.LinkID, again.Link.LinkID)

	_, err = f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: a.Address.AddressID, LinkType: domain.AddressRemittance,
		EvidenceRef: "contract", ExpectedMasterVersion: out.Party.MasterVersion}, "p", false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a billing address is not linked as a remittance address")

	_, err = f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: a.Address.AddressID, LinkType: domain.AddressBilling,
		EvidenceRef: "contract", ExpectedMasterVersion: 1}, "p", false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)

	list, err := f.s.ListPartyAddresses(f.ctx, p.PartyID, time.Now().UTC(), false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, []string{"1 Main Street"}, list[0].Version.Lines)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'address.linked' AND tenant_id = $1`, f.tenantID))

	_, err = f.s.ListPartyAddresses(f.ctx, "00000000-0000-0000-0000-000000000000", time.Now().UTC(), false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestAddress_ARegisteredOfficeChangeEndsTheOldOneAndKeepsItQueryable(t *testing.T) {
	f := newORGFixture(t)
	p := f.party(t, "pb", "Bravo Limited", "LEI-B")
	reg := func(line string) domain.AddressFields {
		af := addrFields(line, "Leeds", "LS1 1AA", "GB")
		af.SourceClass = domain.SourceAuthorityRegistry
		return af
	}
	old := f.address(t, domain.AddressRegistered, reg("1 Registry Way"))
	nw := f.address(t, domain.AddressRegistered, reg("2 Registry Way"))

	l1, err := f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: old.Address.AddressID, LinkType: domain.AddressRegistered,
		ValidFrom: "2024-01-01", EvidenceRef: "registry-extract", ExpectedMasterVersion: p.MasterVersion}, "p", false)
	require.NoError(t, err)
	assert.Nil(t, l1.Ended)

	// A new registered office must start after the current one began.
	_, err = f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: nw.Address.AddressID, LinkType: domain.AddressRegistered,
		ValidFrom: "2023-01-01", EvidenceRef: "registry-extract", ExpectedMasterVersion: l1.Party.MasterVersion}, "p", false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)

	l2, err := f.s.LinkAddressToParty(f.ctx, p.PartyID, domain.LinkAddressRequest{AddressID: nw.Address.AddressID, LinkType: domain.AddressRegistered,
		ValidFrom: "2025-06-01", EvidenceRef: "registry-extract", ExpectedMasterVersion: l1.Party.MasterVersion}, "p", false)
	require.NoError(t, err)
	require.NotNil(t, l2.Ended)
	assert.Equal(t, old.Address.AddressID, l2.Ended.AddressID)
	assert.Equal(t, "2025-06-01", l2.Ended.ValidTo)

	then, err := f.s.ListPartyAddresses(f.ctx, p.PartyID, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	require.Len(t, then, 1)
	assert.Equal(t, []string{"1 Registry Way"}, then[0].Version.Lines, "as of before the move, the old office is the registered one")

	cur, err := f.s.ListPartyAddresses(f.ctx, p.PartyID, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	require.Len(t, cur, 1)
	assert.Equal(t, []string{"2 Registry Way"}, cur[0].Version.Lines)

	all, err := f.s.ListPartyAddresses(f.ctx, p.PartyID, time.Now().UTC(), true)
	require.NoError(t, err)
	assert.Len(t, all, 2, "the ended link is history, not deleted")

	// The database holds the rule even if the service is bypassed: one live registered office per party.
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO address_party_links (link_id, tenant_id, address_id, party_id, link_type, valid_from, evidence_ref, linked_by)
		VALUES (gen_random_uuid(), $1, $2, $3, 'REGISTERED', '2026-01-01', 'x', 'x')`, f.tenantID, old.Address.AddressID, p.PartyID)
	require.Error(t, err)
}
