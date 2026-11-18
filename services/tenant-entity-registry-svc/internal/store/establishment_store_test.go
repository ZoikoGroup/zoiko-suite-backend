package store_test

// ORG-08 establishments against a real Postgres: separate from the address, a named lifecycle recorded as
// transitions, as-of reconstruction, segregation of duties on material ones, and the registered office on the
// address model.

import (
	"context"
	"encoding/json"
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
	proposer  = "p-proposer"
	activator = "p-activator"
)

func (f *orgFixture) establishment(t *testing.T, typ domain.EstablishmentType, requestID string) *registry.EstablishmentOutcome {
	t.Helper()
	addr := f.address(t, domain.AddressOperating, addrFields("9 Branch Road", "Leeds", "LS1 1AA", "GB"))
	req := domain.CreateEstablishmentRequest{CreateRequestID: requestID, LegalEntityID: f.entityID, EstablishmentType: typ, Name: "Leeds Branch",
		AddressID: addr.Address.AddressID, ValidFrom: "2025-01-01", SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "incorporation-cert"}
	out, err := f.s.CreateEstablishment(f.ctx, registry.EstablishmentCreateCommand{Request: req, Actor: proposer, Fingerprint: req.Fingerprint()})
	require.NoError(t, err)
	return out
}

func (f *orgFixture) change(t *testing.T, id string, to domain.EstablishmentStatus, version int64, actor, eff string) (*registry.EstablishmentOutcome, error) {
	t.Helper()
	return f.s.ChangeEstablishmentStatus(f.ctx, id, domain.ChangeEstablishmentStatusRequest{ExpectedVersion: version, To: to, EffectiveDate: eff,
		Reason: "reason", EvidenceRef: "evidence"}, actor, false)
}

func TestEstablishment_StartsProposedAndHasItsOwnIdSeparateFromTheAddress(t *testing.T) {
	f := newORGFixture(t)
	out := f.establishment(t, domain.EstBranch, "")
	e := out.Establishment
	assert.Equal(t, domain.EstProposed, e.Status)
	assert.True(t, e.Material)
	assert.NotEqual(t, e.AddressID, e.EstablishmentID, "an establishment is not its address")
	assert.Equal(t, 1, out.Transition.Seq)
	assert.Equal(t, domain.EstProposed, out.Transition.To)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'establishment.created' AND tenant_id = $1`, f.tenantID))

	// An address by itself creates nothing: no establishment exists until one is recorded from evidence.
	f.address(t, domain.AddressOperating, addrFields("1 Lonely St", "York", "YO1 1AA", "GB"))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM establishments WHERE tenant_id = $1`, f.tenantID))
}

func TestEstablishment_CreateIsIdempotentAndValidatesWhatItPointsAt(t *testing.T) {
	f := newORGFixture(t)
	first := f.establishment(t, domain.EstOperatingSite, "est-1")

	req := domain.CreateEstablishmentRequest{CreateRequestID: "est-1", LegalEntityID: f.entityID, EstablishmentType: domain.EstOperatingSite, Name: "Leeds Branch",
		AddressID: first.Establishment.AddressID, ValidFrom: "2025-01-01", SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "incorporation-cert"}
	again, err := f.s.CreateEstablishment(f.ctx, registry.EstablishmentCreateCommand{Request: req, Actor: proposer, Fingerprint: req.Fingerprint()})
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Establishment.EstablishmentID, again.Establishment.EstablishmentID)

	req.Name = "Another Name"
	_, err = f.s.CreateEstablishment(f.ctx, registry.EstablishmentCreateCommand{Request: req, Actor: proposer, Fingerprint: req.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrCreateRequestMismatch)

	bad := req
	bad.CreateRequestID, bad.AddressID = "", uuid.NewString()
	_, err = f.s.CreateEstablishment(f.ctx, registry.EstablishmentCreateCommand{Request: bad, Actor: proposer, Fingerprint: bad.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "an address that does not exist is refused")

	bad = req
	bad.CreateRequestID, bad.LegalEntityID = "", uuid.NewString()
	_, err = f.s.CreateEstablishment(f.ctx, registry.EstablishmentCreateCommand{Request: bad, Actor: proposer, Fingerprint: bad.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrNotFound, "an entity that does not exist is refused")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM establishments WHERE tenant_id = $1`, f.tenantID))
}

func TestEstablishment_AMaterialOneIsNotActivatedByItsProposer(t *testing.T) {
	f := newORGFixture(t)
	branch := f.establishment(t, domain.EstBranch, "")
	id := branch.Establishment.EstablishmentID

	_, err := f.change(t, id, domain.EstActive, 1, proposer, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrSelfApproval, "no single operator creates and legally activates a material establishment")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM establishment_transitions WHERE establishment_id = $1`, id))

	ok, err := f.change(t, id, domain.EstActive, 1, activator, "2025-02-01")
	require.NoError(t, err)
	assert.Equal(t, domain.EstActive, ok.Establishment.Status)
	assert.Equal(t, activator, ok.Establishment.ActivatedBy)
	assert.Equal(t, int64(2), ok.Establishment.RecordVersion)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'establishment.activated' AND tenant_id = $1`, f.tenantID))

	// A non-material type may be activated by its proposer.
	site := f.establishment(t, domain.EstOperatingSite, "")
	_, err = f.change(t, site.Establishment.EstablishmentID, domain.EstActive, 1, proposer, "2025-02-01")
	require.NoError(t, err)
}

func TestEstablishment_TheDatabaseHoldsTheSegregationRuleToo(t *testing.T) {
	f := newORGFixture(t)
	branch := f.establishment(t, domain.EstPermanent, "")
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE establishments SET status = 'ACTIVE', activated_by = created_by WHERE establishment_id = $1`, branch.Establishment.EstablishmentID)
	require.Error(t, err, "a material establishment cannot be activated by its creator even if the service is bypassed")
}

func TestEstablishment_LifecycleOnlyMovesForward(t *testing.T) {
	f := newORGFixture(t)
	est := f.establishment(t, domain.EstOperatingSite, "")
	id := est.Establishment.EstablishmentID

	_, err := f.change(t, id, domain.EstInactive, 1, activator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrStateConflict, "PROPOSED cannot go straight to INACTIVE")

	a, err := f.change(t, id, domain.EstActive, 1, activator, "2025-02-01")
	require.NoError(t, err)
	d, err := f.change(t, id, domain.EstInactive, a.Establishment.RecordVersion, activator, "2025-06-01")
	require.NoError(t, err)
	r, err := f.change(t, id, domain.EstActive, d.Establishment.RecordVersion, activator, "2025-09-01")
	require.NoError(t, err)
	c, err := f.change(t, id, domain.EstClosed, r.Establishment.RecordVersion, activator, "2025-12-31")
	require.NoError(t, err)
	assert.Equal(t, "2025-12-31", c.Establishment.ValidTo, "closing sets the end of the presence")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'establishment.closed' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'establishment.deactivated' AND tenant_id = $1`, f.tenantID))

	_, err = f.change(t, id, domain.EstActive, c.Establishment.RecordVersion, activator, "2026-01-05")
	require.ErrorIs(t, err, registry.ErrStateConflict, "CLOSED is final")

	view, err := f.s.GetEstablishment(f.ctx, id, time.Now().UTC(), true)
	require.NoError(t, err)
	require.Len(t, view.History, 5)
	assert.Equal(t, domain.EstProposed, view.History[0].To)
	assert.Equal(t, domain.EstInactive, view.History[2].To)
	assert.Equal(t, domain.EstActive, view.History[2].From)
}

func TestEstablishment_ChecksVersionAndDates(t *testing.T) {
	f := newORGFixture(t)
	est := f.establishment(t, domain.EstOperatingSite, "")
	id := est.Establishment.EstablishmentID

	_, err := f.change(t, id, domain.EstActive, 9, activator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.change(t, id, domain.EstActive, 0, activator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.change(t, id, domain.EstActive, 1, activator, "2024-06-01")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "before the establishment began")

	a, err := f.change(t, id, domain.EstActive, 1, activator, "2025-06-01")
	require.NoError(t, err)
	_, err = f.change(t, id, domain.EstInactive, a.Establishment.RecordVersion, activator, "2025-05-01")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "before the last change")
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM establishment_transitions WHERE establishment_id = $1`, id))
}

func TestEstablishment_ConcurrentActivationsOneWins(t *testing.T) {
	f := newORGFixture(t)
	est := f.establishment(t, domain.EstOperatingSite, "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.change(t, est.Establishment.EstablishmentID, domain.EstActive, 1, activator, "2025-02-01")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM establishment_transitions WHERE establishment_id = $1`, est.Establishment.EstablishmentID))
}

func TestEstablishment_AsOfIsReconstructedFromTransitionsNotTheCurrentStatus(t *testing.T) {
	f := newORGFixture(t)
	est := f.establishment(t, domain.EstOperatingSite, "")
	id := est.Establishment.EstablishmentID
	a, _ := f.change(t, id, domain.EstActive, 1, activator, "2025-02-01")
	d, _ := f.change(t, id, domain.EstInactive, a.Establishment.RecordVersion, activator, "2025-06-01")
	r, _ := f.change(t, id, domain.EstActive, d.Establishment.RecordVersion, activator, "2025-09-01")
	_, err := f.change(t, id, domain.EstClosed, r.Establishment.RecordVersion, activator, "2025-12-31")
	require.NoError(t, err)

	at := func(y int, m time.Month, day int) int {
		v, err := f.s.EstablishmentsAsOf(f.ctx, f.entityID, time.Date(y, m, day, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		return len(v)
	}
	assert.Equal(t, 0, at(2025, 1, 15), "proposed is not active")
	assert.Equal(t, 1, at(2025, 3, 1), "active")
	assert.Equal(t, 0, at(2025, 7, 1), "inactive in the middle")
	assert.Equal(t, 1, at(2025, 10, 1), "active again")
	assert.Equal(t, 1, at(2025, 12, 30), "still active the day before it closed")
	assert.Equal(t, 0, at(2025, 12, 31), "closed on that date")
	assert.Equal(t, 0, at(2026, 2, 1), "closed")

	v, err := f.s.EstablishmentsAsOf(f.ctx, f.entityID, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, v, 1)
	assert.Equal(t, domain.EstActive, v[0].Establishment.Status, "the as-of view reports the status it had that day")
	require.NotNil(t, v[0].Address)
	assert.Equal(t, []string{"9 Branch Road"}, v[0].Address.Lines)
}

func TestEstablishment_ListByStatusAndTenantIsolation(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	est := a.establishment(t, domain.EstOperatingSite, "")
	a.establishment(t, domain.EstBranch, "")
	_, err := a.change(t, est.Establishment.EstablishmentID, domain.EstActive, 1, activator, "2025-02-01")
	require.NoError(t, err)

	all, err := a.s.ListEntityEstablishments(a.ctx, a.entityID, "")
	require.NoError(t, err)
	assert.Len(t, all, 2)
	active, err := a.s.ListEntityEstablishments(a.ctx, a.entityID, domain.EstActive)
	require.NoError(t, err)
	assert.Len(t, active, 1)

	_, err = b.s.ListEntityEstablishments(b.ctx, a.entityID, "")
	require.ErrorIs(t, err, registry.ErrNotFound, "another tenant cannot list an entity that is not its own")
	v, err := b.s.GetEstablishment(b.ctx, est.Establishment.EstablishmentID, time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, v)
	_, err = b.s.EstablishmentEntity(b.ctx, est.Establishment.EstablishmentID)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.ChangeEstablishmentStatus(b.ctx, est.Establishment.EstablishmentID, domain.ChangeEstablishmentStatusRequest{ExpectedVersion: 1, To: domain.EstActive, EvidenceRef: "e"}, activator, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestEstablishment_DatabaseKeepsHistory(t *testing.T) {
	f := newORGFixture(t)
	est := f.establishment(t, domain.EstOperatingSite, "")
	id := est.Establishment.EstablishmentID
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
	require.Error(t, exec(`DELETE FROM establishments WHERE establishment_id = $1`, id))
	require.Error(t, exec(`UPDATE establishments SET name = 'Renamed' WHERE establishment_id = $1`, id), "content is immutable")
	require.Error(t, exec(`UPDATE establishments SET address_id = $2 WHERE establishment_id = $1`, id, uuid.NewString()))
	require.Error(t, exec(`UPDATE establishment_transitions SET reason = 'x' WHERE establishment_id = $1`, id), "transitions are append-only")
	require.Error(t, exec(`DELETE FROM establishment_transitions WHERE establishment_id = $1`, id))
	require.Error(t, exec(`UPDATE establishments SET status = 'CLOSED' WHERE establishment_id = $1`, id), "closed needs an end date")
}

func structuredOffice(line, city, postal string) string {
	b, _ := json.Marshal(map[string]any{"lines": []string{line}, "city": city, "postcode": postal, "country": "gb"})
	return string(b)
}

func (f *orgFixture) changeOffice(t *testing.T, office, effective string, expected int64) error {
	t.Helper()
	ev := "registry-extract"
	from, err := time.Parse("2006-01-02", effective)
	require.NoError(t, err)
	_, err = f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.NewString(), TenantID: f.tenantID, LegalEntityID: f.entityID, LegalName: "Original Name Ltd",
		RegisteredOffice: &office, EffectiveFrom: from, ChangeReason: domain.ProfileChangeRegisteredOfficeChange,
		SourceEvidenceRef: &ev, CreatedByPrincipalID: "p-actor",
	}, expected, nil)
	return err
}

func TestRegisteredOffice_ABecomesAnAddressAndTheChangeKeepsTheOldOne(t *testing.T) {
	f := newORGFixture(t)
	require.NoError(t, f.changeOffice(t, structuredOffice("1 Registry Way", "Leeds", "LS1 1AA"), "2025-01-01", 1))
	require.NoError(t, f.changeOffice(t, structuredOffice("2 Registry Way", "Leeds", "LS2 2BB"), "2025-06-01", 2))

	cur, err := f.s.ListEntityAddresses(f.ctx, f.entityID, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	require.Len(t, cur, 1)
	assert.Equal(t, domain.AddressRegistered, cur[0].Link.LinkType)
	assert.Equal(t, []string{"2 Registry Way"}, cur[0].Version.Lines)
	assert.Equal(t, domain.SourceAuthorityDocument, cur[0].Version.SourceClass, "a registered address is only ever recorded from authority evidence")
	assert.Equal(t, "GB", cur[0].Version.Country)
	assert.Equal(t, "LS2 2BB", cur[0].Version.PostalCode)

	then, err := f.s.ListEntityAddresses(f.ctx, f.entityID, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	require.Len(t, then, 1)
	assert.Equal(t, []string{"1 Registry Way"}, then[0].Version.Lines, "as of before the move, the old office")

	all, err := f.s.ListEntityAddresses(f.ctx, f.entityID, time.Now().UTC(), true)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// The ORG-03 profile history is untouched: it still holds the office as given.
	versions, err := f.s.ListEntityProfileVersions(f.ctx, f.entityID)
	require.NoError(t, err)
	require.Len(t, versions, 3)
	assert.Contains(t, *versions[0].RegisteredOffice, "2 Registry Way")
}

func TestRegisteredOffice_OpaqueAndBackdatedChangesStayInTheProfileHistory(t *testing.T) {
	f := newORGFixture(t)
	// An office that is not a structured address is accepted exactly as before.
	require.NoError(t, f.changeOffice(t, `{"free_text":"Somewhere in Leeds"}`, "2025-01-01", 1))
	assert.Zero(t, f.count(t, `SELECT count(*) FROM entity_address_links WHERE tenant_id = $1`, f.tenantID))
	assert.Zero(t, f.count(t, `SELECT count(*) FROM addresses WHERE tenant_id = $1`, f.tenantID))

	require.NoError(t, f.changeOffice(t, structuredOffice("1 Registry Way", "Leeds", "LS1 1AA"), "2025-03-01", 2))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM entity_address_links WHERE tenant_id = $1`, f.tenantID))

	// A change that does not start after the current office began is a correction of history, not mirrored.
	require.NoError(t, f.changeOffice(t, structuredOffice("9 Old Way", "Leeds", "LS9 9ZZ"), "2025-02-01", 3))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM entity_address_links WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM addresses WHERE tenant_id = $1`, f.tenantID))
}

func TestRegisteredOffice_AnotherTenantsEntityIsNeverLinked(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	ctx := context.Background()
	addr := b.address(t, domain.AddressRegistered, func() domain.AddressFields {
		af := addrFields("1 Way", "Leeds", "LS1 1AA", "GB")
		af.SourceClass = domain.SourceAuthorityDocument
		return af
	}())
	tx, err := a.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, b.tenantID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO entity_address_links (link_id, tenant_id, legal_entity_id, address_id, link_type, valid_from, evidence_ref, linked_by)
		VALUES (gen_random_uuid(), $1, $2, $3, 'REGISTERED', '2025-01-01', 'x', 'x')`, b.tenantID, a.entityID, addr.Address.AddressID)
	require.Error(t, err, "an address links only to a legal entity of its own tenant")
}
