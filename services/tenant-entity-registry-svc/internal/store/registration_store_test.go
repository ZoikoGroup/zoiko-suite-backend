package store_test

// ORG-09 registrations against a real Postgres: the lifecycle and its history, "unverified cannot be active" held by
// the table, segregation of duties on material registrations, candidate uniqueness, supersession and the expiry sweep
// across tenants.

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
	regCreator  = "p-reg-creator"
	regVerifier = "p-reg-verifier"
	regRenewer  = "p-reg-renewer"
)

func day(offset int) string { return time.Now().UTC().AddDate(0, 0, offset).Format("2006-01-02") }

func (f *orgFixture) regReq(typ domain.RegistrationType, number string) domain.CreateRegistrationRequest {
	return domain.CreateRegistrationRequest{
		RegistrationType: typ, IssuingAuthority: "Companies House", Jurisdiction: "GB", Number: number,
		HolderLegalEntityID: f.entityID, EffectiveFrom: "2024-01-01", ExpiryDate: day(200),
		SourceClass: domain.SourceAuthorityDocument, EvidenceRef: "certificate-1",
	}
}

func (f *orgFixture) createReg(t *testing.T, req domain.CreateRegistrationRequest) *registry.RegistrationOutcome {
	t.Helper()
	out, err := f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: req, Actor: regCreator, Fingerprint: req.Fingerprint()})
	require.NoError(t, err)
	return out
}

func (f *orgFixture) verifyReg(t *testing.T, id string, version int64, actor string) (*registry.RegistrationOutcome, error) {
	t.Helper()
	return f.s.VerifyRegistration(f.ctx, id, domain.VerifyRegistrationRequest{ExpectedVersion: version, EvidenceRef: "registry-lookup"}, actor, false)
}

func (f *orgFixture) activeReg(t *testing.T, typ domain.RegistrationType, number string) *domain.Registration {
	t.Helper()
	c := f.createReg(t, f.regReq(typ, number))
	v, err := f.verifyReg(t, c.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)
	return &v.Registration
}

func TestRegistration_StartsDraftAndNeverReadsAsInForce(t *testing.T) {
	f := newORGFixture(t)
	out := f.createReg(t, f.regReq(domain.RegCompany, "gb 123-456"))
	r := out.Registration
	assert.Equal(t, domain.RegDraft, r.Status)
	assert.Equal(t, "gb 123-456", r.Number, "the number is kept as entered")
	assert.Nil(t, r.VerifiedAt)
	assert.Equal(t, 1, out.Transition.Seq)
	r.Evaluate(time.Now().UTC())
	assert.False(t, r.InForce, "s4.9: an unverified registration cannot appear active")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.created' AND tenant_id = $1`, f.tenantID))

	found, err := f.s.FindRegistrationsByNumber(f.ctx, "GB123456", "", "companies house", "gb")
	require.NoError(t, err)
	require.Len(t, found, 1, "the number and the authority are matched on their normalized forms")
	assert.Equal(t, r.RegistrationID, found[0].RegistrationID)
}

func TestRegistration_TheTableRefusesActiveWithoutVerificationAndKeepsHistory(t *testing.T) {
	f := newORGFixture(t)
	draft := f.createReg(t, f.regReq(domain.RegLicense, "LIC-1"))
	id := draft.Registration.RegistrationID
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
	require.Error(t, exec(`UPDATE registrations SET status = 'ACTIVE' WHERE registration_id = $1`, id), "ACTIVE needs a verification")
	require.Error(t, exec(`UPDATE registrations SET status = 'ACTIVE', verified_at = now(), verified_by = created_by WHERE registration_id = $1`, id),
		"a material registration cannot be verified by its creator even if the service is bypassed")
	require.NoError(t, exec(`UPDATE registrations SET status = 'ACTIVE', verified_at = now(), verified_by = 'someone-else' WHERE registration_id = $1`, id))
	require.Error(t, exec(`UPDATE registrations SET number = 'CHANGED' WHERE registration_id = $1`, id), "identity is immutable")
	require.Error(t, exec(`UPDATE registrations SET holder_legal_entity_id = NULL, holder_party_id = gen_random_uuid() WHERE registration_id = $1`, id))
	require.Error(t, exec(`DELETE FROM registrations WHERE registration_id = $1`, id))
	require.Error(t, exec(`UPDATE registration_history SET reason = 'x' WHERE registration_id = $1`, id), "history is append-only")
	require.Error(t, exec(`DELETE FROM registration_history WHERE registration_id = $1`, id))
	require.Error(t, exec(`UPDATE registrations SET evidence_hash = 'not-a-hash' WHERE registration_id = $1`, id))
	require.Error(t, exec(`INSERT INTO registrations (registration_id, tenant_id, registration_type, issuing_authority, authority_normalized, jurisdiction, number,
		number_normalized, holder_legal_entity_id, holder_party_id, material, effective_from, source_class, evidence_ref, created_by)
		VALUES (gen_random_uuid(), $1, 'LICENSE', 'A', 'A', 'GB', '1', '1', $2, NULL, true, '2025-01-01', 'TENANT_ASSERTED', 'e', 'x')`,
		f.tenantID, uuid.NewString()), "a holder is a legal entity of the tenant")
}

func TestRegistration_CreateIsIdempotentAndRefusesALiveDuplicate(t *testing.T) {
	f := newORGFixture(t)
	req := f.regReq(domain.RegCompany, "DUP-1")
	req.CreateRequestID = "reg-1"
	first := f.createReg(t, req)

	again, err := f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: req, Actor: regCreator, Fingerprint: req.Fingerprint()})
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Registration.RegistrationID, again.Registration.RegistrationID)

	other := req
	other.Number = "DIFFERENT"
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: other, Actor: regCreator, Fingerprint: other.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrCreateRequestMismatch)

	dup := f.regReq(domain.RegCompany, "dup 1")
	dup.IssuingAuthority = "COMPANIES HOUSE."
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: dup, Actor: regCreator, Fingerprint: dup.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrConflict, "authority, type, jurisdiction and number name one live registration")

	// A different type or jurisdiction is a different registration.
	diffType := f.regReq(domain.RegPermit, "DUP-1")
	f.createReg(t, diffType)

	// Once the first is revoked the number is free again.
	_, err = f.s.ChangeRegistrationStatus(f.ctx, first.Registration.RegistrationID, "RevokeRegistration", domain.RegRevoked,
		domain.RegistrationEndRequest{ExpectedVersion: 1, Reason: "withdrawn", EvidenceRef: "e"}, regVerifier, false)
	require.NoError(t, err)
	f.createReg(t, dup)
	assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM registrations WHERE tenant_id = $1`, f.tenantID))
}

func TestRegistration_ConcurrentCreatesOfOneNumberMakeOne(t *testing.T) {
	f := newORGFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := f.regReq(domain.RegCompany, "RACE-1")
			_, err := f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: req, Actor: regCreator, Fingerprint: req.Fingerprint()})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				assert.ErrorIs(t, err, registry.ErrConflict)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM registrations WHERE tenant_id = $1`, f.tenantID))
}

func TestRegistration_AMaterialOneIsNotVerifiedByItsCreator(t *testing.T) {
	f := newORGFixture(t)
	lic := f.createReg(t, f.regReq(domain.RegLicense, "LIC-9"))
	id := lic.Registration.RegistrationID
	assert.True(t, lic.Registration.Material)

	_, err := f.verifyReg(t, id, 1, regCreator)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM registration_history WHERE registration_id = $1`, id))

	ok, err := f.verifyReg(t, id, 1, regVerifier)
	require.NoError(t, err)
	assert.Equal(t, domain.RegActive, ok.Registration.Status)
	assert.Equal(t, regVerifier, ok.Registration.VerifiedBy)
	assert.Equal(t, "registry-lookup", ok.Registration.EvidenceRef, "the current evidence is the verifying evidence")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.verified' AND tenant_id = $1`, f.tenantID))

	// A non-material registration may be verified by whoever created it.
	co := f.createReg(t, f.regReq(domain.RegCompany, "CO-9"))
	_, err = f.verifyReg(t, co.Registration.RegistrationID, 1, regCreator)
	require.NoError(t, err)
}

func TestRegistration_VerifyChecksStateVersionAndExpiry(t *testing.T) {
	f := newORGFixture(t)
	c := f.createReg(t, f.regReq(domain.RegCompany, "V-1"))
	id := c.Registration.RegistrationID
	_, err := f.verifyReg(t, id, 9, regVerifier)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.verifyReg(t, id, 0, regVerifier)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.verifyReg(t, id, 1, regVerifier)
	require.NoError(t, err)
	_, err = f.verifyReg(t, id, 2, regVerifier)
	require.ErrorIs(t, err, registry.ErrStateConflict, "ACTIVE cannot be verified again")

	past := f.regReq(domain.RegCompany, "V-2")
	past.EffectiveFrom, past.ExpiryDate = "2020-01-01", "2021-01-01"
	p := f.createReg(t, past)
	_, err = f.verifyReg(t, p.Registration.RegistrationID, 1, regVerifier)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a registration already past its expiry is renewed, not verified")
	_, err = f.verifyReg(t, uuid.NewString(), 1, regVerifier)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestRegistration_RenewExtendsAndAMaterialRenewalNeedsSomeoneElse(t *testing.T) {
	f := newORGFixture(t)
	lic := f.activeReg(t, domain.RegLicense, "LIC-R")
	id := lic.RegistrationID
	renew := func(v int64, exp, actor string) (*registry.RegistrationOutcome, error) {
		return f.s.RenewRegistration(f.ctx, id, domain.RenewRegistrationRequest{ExpectedVersion: v, NewExpiryDate: exp, EvidenceRef: "renewal-cert"}, actor, false)
	}
	_, err := renew(2, day(400), regVerifier)
	require.ErrorIs(t, err, registry.ErrSelfApproval, "the principal who last made it valid cannot renew a material registration")
	_, err = renew(2, day(100), regRenewer)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a renewal extends the expiry")

	out, err := renew(2, day(400), regRenewer)
	require.NoError(t, err)
	assert.Equal(t, day(400), out.Registration.ExpiryDate)
	assert.Equal(t, domain.RegActive, out.Registration.Status)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.renewed' AND tenant_id = $1`, f.tenantID))

	_, err = renew(3, day(600), regRenewer)
	require.ErrorIs(t, err, registry.ErrSelfApproval, "and the renewer is now the last to make it valid")
	_, err = renew(3, day(600), regVerifier)
	require.NoError(t, err)

	draft := f.createReg(t, f.regReq(domain.RegCompany, "R-DRAFT"))
	_, err = f.s.RenewRegistration(f.ctx, draft.Registration.RegistrationID, domain.RenewRegistrationRequest{ExpectedVersion: 1, NewExpiryDate: day(900), EvidenceRef: "e"}, regRenewer, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "a DRAFT is verified, not renewed")
}

func TestRegistration_LifecycleAndHistory(t *testing.T) {
	f := newORGFixture(t)
	r := f.activeReg(t, domain.RegCompany, "L-1")
	id := r.RegistrationID
	end := func(cmd string, to domain.RegistrationStatus, v int64) (*registry.RegistrationOutcome, error) {
		return f.s.ChangeRegistrationStatus(f.ctx, id, cmd, to, domain.RegistrationEndRequest{ExpectedVersion: v, Reason: "r", EvidenceRef: "e"}, regVerifier, false)
	}
	s, err := end("SuspendRegistration", domain.RegSuspended, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.RegSuspended, s.Registration.Status)
	_, err = end("SuspendRegistration", domain.RegSuspended, 3)
	require.ErrorIs(t, err, registry.ErrStateConflict, "already suspended")

	re, err := f.verifyReg(t, id, 3, regVerifier)
	require.NoError(t, err)
	assert.Equal(t, domain.RegActive, re.Registration.Status, "a suspended registration is re-verified back to ACTIVE")

	e, err := end("ExpireRegistration", domain.RegExpired, 4)
	require.NoError(t, err)
	assert.Equal(t, domain.RegExpired, e.Registration.Status)
	rv, err := end("RevokeRegistration", domain.RegRevoked, 5)
	require.NoError(t, err)
	assert.Equal(t, domain.RegRevoked, rv.Registration.Status)
	_, err = end("SuspendRegistration", domain.RegSuspended, 6)
	require.ErrorIs(t, err, registry.ErrStateConflict, "REVOKED is final")
	_, err = f.verifyReg(t, id, 6, regVerifier)
	require.ErrorIs(t, err, registry.ErrStateConflict)

	h, err := f.s.RegistrationHistory(f.ctx, id)
	require.NoError(t, err)
	var cmds []string
	for _, x := range h {
		cmds = append(cmds, x.Command)
	}
	assert.Equal(t, []string{"CreateRegistration", "VerifyRegistration", "SuspendRegistration", "VerifyRegistration", "ExpireRegistration", "RevokeRegistration"}, cmds)
	assert.Equal(t, domain.RegSuspended, h[3].From)
	for i, x := range h {
		assert.Equal(t, i+1, x.Seq)
		assert.NotEmpty(t, x.Actor)
		assert.NotEmpty(t, x.EvidenceRef)
	}
	for _, name := range []string{"registration.suspended", "registration.expired", "registration.revoked"} {
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = $2 AND tenant_id = $1`, f.tenantID, name), name)
	}
	_, err = f.s.RegistrationHistory(f.ctx, uuid.NewString())
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestRegistration_ASuccessorSupersedesWhenItIsVerified(t *testing.T) {
	f := newORGFixture(t)
	old := f.activeReg(t, domain.RegCompany, "OLD-1")
	req := f.regReq(domain.RegCompany, "NEW-1")
	req.SupersedesID = old.RegistrationID
	nw := f.createReg(t, req)

	still, err := f.s.GetRegistration(f.ctx, old.RegistrationID)
	require.NoError(t, err)
	assert.Equal(t, domain.RegActive, still.Status, "the old one stands until the new one is proved")

	out, err := f.verifyReg(t, nw.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)
	require.NotNil(t, out.Superseded)
	assert.Equal(t, domain.RegSuperseded, out.Superseded.Status)
	assert.Equal(t, nw.Registration.RegistrationID, out.Superseded.SupersededByID)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.superseded' AND tenant_id = $1`, f.tenantID))
	h, err := f.s.RegistrationHistory(f.ctx, old.RegistrationID)
	require.NoError(t, err)
	assert.Equal(t, "SupersedeRegistration", h[len(h)-1].Command)

	// Only a registration of the same holder, and only once.
	again := f.regReq(domain.RegCompany, "NEW-2")
	again.SupersedesID = old.RegistrationID
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: again, Actor: regCreator, Fingerprint: again.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrStateConflict)
	bad := f.regReq(domain.RegCompany, "NEW-3")
	bad.SupersedesID = uuid.NewString()
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: bad, Actor: regCreator, Fingerprint: bad.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestRegistration_APartyCanHoldOneAndAnUnknownHolderIsRefused(t *testing.T) {
	f := newORGFixture(t)
	p := f.party(t, "reg-party", "Holder Limited", "LEI-REG")
	req := f.regReq(domain.RegPermit, "PERMIT-1")
	req.HolderLegalEntityID, req.HolderPartyID = "", p.PartyID
	out := f.createReg(t, req)
	assert.Equal(t, p.PartyID, out.Registration.HolderPartyID)
	assert.Empty(t, out.Registration.HolderLegalEntityID)

	list, _, err := f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{HolderPartyID: p.PartyID})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	nope := f.regReq(domain.RegPermit, "PERMIT-2")
	nope.HolderLegalEntityID, nope.HolderPartyID = "", uuid.NewString()
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: nope, Actor: regCreator, Fingerprint: nope.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrNotFound)
	nope.HolderPartyID, nope.HolderLegalEntityID = "", uuid.NewString()
	_, err = f.s.CreateRegistration(f.ctx, registry.RegistrationCreateCommand{Request: nope, Actor: regCreator, Fingerprint: nope.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestRegistration_ListFindAndExpiringReads(t *testing.T) {
	f := newORGFixture(t)
	soon := f.regReq(domain.RegCompany, "SOON")
	soon.ExpiryDate = day(10)
	later := f.regReq(domain.RegPermit, "LATER")
	later.ExpiryDate = day(300)
	noExpiry := f.regReq(domain.RegOtherGoverned, "NOEXP")
	noExpiry.ExpiryDate = ""
	var ids []string
	for _, r := range []domain.CreateRegistrationRequest{soon, later, noExpiry} {
		c := f.createReg(t, r)
		_, err := f.verifyReg(t, c.Registration.RegistrationID, 1, regVerifier)
		require.NoError(t, err)
		ids = append(ids, c.Registration.RegistrationID)
	}

	exp, err := f.s.ListExpiringRegistrations(f.ctx, 30, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, exp, 1)
	assert.Equal(t, ids[0], exp[0].RegistrationID)
	all, err := f.s.ListExpiringRegistrations(f.ctx, 365, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, ids[0], all[0].RegistrationID, "soonest first")
	lapsed, err := f.s.ListExpiringRegistrations(f.ctx, 0, time.Now().UTC().AddDate(0, 0, 20))
	require.NoError(t, err)
	require.Len(t, lapsed, 1, "one already past its date, still ACTIVE because the sweep has not run, is listed")
	lapsed[0].Evaluate(time.Now().UTC().AddDate(0, 0, 20))
	assert.True(t, lapsed[0].Lapsed)
	assert.False(t, lapsed[0].InForce, "expiry is surfaced from the dates, not from the sweep")

	byType, _, err := f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{Type: domain.RegPermit})
	require.NoError(t, err)
	assert.Len(t, byType, 1)
	byEntity, _, err := f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{HolderLegalEntityID: f.entityID, Status: domain.RegActive})
	require.NoError(t, err)
	assert.Len(t, byEntity, 3)
	p1, next, err := f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, p1, 2)
	require.NotEmpty(t, next)
	p2, next2, err := f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{Limit: 2, After: next})
	require.NoError(t, err)
	assert.Len(t, p2, 1)
	assert.Empty(t, next2)
	_, _, err = f.s.ListRegistrations(f.ctx, domain.RegistrationSearchQuery{After: "junk"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestRegistration_TenantsNeverSeeEachOther(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	r := a.activeReg(t, domain.RegCompany, "ISO-1")

	got, err := b.s.GetRegistration(b.ctx, r.RegistrationID)
	require.NoError(t, err)
	assert.Nil(t, got)
	found, err := b.s.FindRegistrationsByNumber(b.ctx, "ISO-1", "", "", "")
	require.NoError(t, err)
	assert.Empty(t, found)
	_, err = b.verifyReg(t, r.RegistrationID, 2, regVerifier)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.RegistrationHistory(b.ctx, r.RegistrationID)
	require.ErrorIs(t, err, registry.ErrNotFound)
	exp, err := b.s.ListExpiringRegistrations(b.ctx, 3650, time.Now().UTC())
	require.NoError(t, err)
	assert.Empty(t, exp, "the sweep capability does not leak into an ordinary tenant read")

	// Another tenant's entity cannot hold a registration here.
	req := a.regReq(domain.RegCompany, "X-1")
	req.HolderLegalEntityID = b.entityID
	_, err = a.s.CreateRegistration(a.ctx, registry.RegistrationCreateCommand{Request: req, Actor: regCreator, Fingerprint: req.Fingerprint()})
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestRegistrationSweep_ExpiresWarnsOnceAndWorksAcrossTenants(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)

	soon := a.regReq(domain.RegCompany, "SWEEP-SOON")
	soon.ExpiryDate = day(10)
	sc := a.createReg(t, soon)
	_, err := a.verifyReg(t, sc.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)

	gone := a.regReq(domain.RegCompany, "SWEEP-GONE")
	gone.ExpiryDate = day(5)
	gc := a.createReg(t, gone)
	_, err = a.verifyReg(t, gc.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)

	susp := a.regReq(domain.RegCompany, "SWEEP-SUSP")
	susp.ExpiryDate = day(6)
	pc := a.createReg(t, susp)
	pv, err := a.verifyReg(t, pc.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)
	_, err = a.s.ChangeRegistrationStatus(a.ctx, pc.Registration.RegistrationID, "SuspendRegistration", domain.RegSuspended,
		domain.RegistrationEndRequest{ExpectedVersion: pv.Registration.RecordVersion, Reason: "r", EvidenceRef: "e"}, regVerifier, false)
	require.NoError(t, err)

	other := b.regReq(domain.RegCompany, "SWEEP-B")
	other.ExpiryDate = day(3)
	oc := b.createReg(t, other)
	_, err = b.verifyReg(t, oc.Registration.RegistrationID, 1, regVerifier)
	require.NoError(t, err)

	// Nothing is due yet: only the ones inside the 30-day window warn; nothing has expired.
	res, err := a.s.SweepRegistrationExpiries(context.Background(), time.Now().UTC(), 30)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Expired)
	assert.Equal(t, 3, res.Warned, "two of tenant A's active registrations and tenant B's are inside the window; the suspended one is not warned")
	res, err = a.s.SweepRegistrationExpiries(context.Background(), time.Now().UTC(), 30)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Warned, "a window warns once")
	assert.Equal(t, 2, a.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.expiring' AND tenant_id = $1`, a.tenantID))
	assert.Equal(t, 1, a.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.expiring' AND tenant_id = $1`, b.tenantID))

	// Eight days on, two of A's and B's are past their dates.
	later := time.Now().UTC().AddDate(0, 0, 8)
	res, err = a.s.SweepRegistrationExpiries(context.Background(), later, 30)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Expired, "active and suspended registrations past their date expire, in both tenants")
	assert.Equal(t, 0, res.Warned)

	for _, id := range []string{gc.Registration.RegistrationID, pc.Registration.RegistrationID} {
		r, err := a.s.GetRegistration(a.ctx, id)
		require.NoError(t, err)
		assert.Equal(t, domain.RegExpired, r.Status)
	}
	rb, err := b.s.GetRegistration(b.ctx, oc.Registration.RegistrationID)
	require.NoError(t, err)
	assert.Equal(t, domain.RegExpired, rb.Status, "tenant B's was swept inside tenant B")
	h, err := b.s.RegistrationHistory(b.ctx, oc.Registration.RegistrationID)
	require.NoError(t, err)
	last := h[len(h)-1]
	assert.Equal(t, "ExpireRegistration", last.Command)
	assert.Equal(t, "system:expiry-sweep", last.Actor)
	assert.Equal(t, 1, a.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.expired' AND tenant_id = $1`, b.tenantID))
	assert.Equal(t, 2, a.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'registration.expired' AND tenant_id = $1`, a.tenantID))

	res, err = a.s.SweepRegistrationExpiries(context.Background(), later, 30)
	require.NoError(t, err)
	assert.Equal(t, registry.SweepResult{}, res, "nothing left to do, and an expired registration is not expired twice")

	// An expired registration is renewed back to ACTIVE, and the new date warns again in its window.
	rn, err := a.s.RenewRegistration(a.ctx, gc.Registration.RegistrationID,
		domain.RenewRegistrationRequest{ExpectedVersion: 3, NewExpiryDate: day(20), EvidenceRef: "renewed"}, regRenewer, false)
	require.NoError(t, err)
	assert.Equal(t, domain.RegActive, rn.Registration.Status)
	res, err = a.s.SweepRegistrationExpiries(context.Background(), later, 30)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Warned, "a renewal to a new date warns again")
}
