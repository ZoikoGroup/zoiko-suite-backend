package store_test

// MDM-01 party contact points against a real Postgres: maker-checker in the store and in the table, tenant isolation,
// duplicates, the final RETIRED state, as_of reads, immutability, events that never carry the address, and a down
// migration that refuses while contact points exist.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	cpMaker   = "p-contact-maker"
	cpChecker = "p-contact-checker"
)

func (f *orgFixture) contactParty(t *testing.T) *domain.Party {
	t.Helper()
	return f.party(t, "cp-"+uuid.NewString()[:8], "Contact Subject "+uuid.NewString()[:6]+" Limited", "LEI-"+uuid.NewString()[:8])
}

func (f *orgFixture) proposeContact(t *testing.T, partyID, value string, purpose domain.ContactPurpose) *domain.ContactPoint {
	t.Helper()
	c, err := f.tryPropose(partyID, value, purpose, nil)
	require.NoError(t, err)
	return c
}

func (f *orgFixture) tryPropose(partyID, value string, purpose domain.ContactPurpose, from *time.Time) (*domain.ContactPoint, error) {
	req, err := domain.CreateContactPointRequest{Channel: domain.ContactEmail, Value: value, Purpose: purpose,
		SourceClass: domain.SourceTenantAsserted, ValidFrom: from}.Normalized()
	if err != nil {
		return nil, err
	}
	out, err := f.s.CreateContactPoint(f.ctx, registry.ContactPointCreateCommand{PartyID: partyID, Request: req, Actor: cpMaker})
	if err != nil {
		return nil, err
	}
	return &out.ContactPoint, nil
}

func (f *orgFixture) verifyContact(id string, version int64, actor string) (*registry.ContactPointOutcome, error) {
	return f.s.VerifyContactPoint(f.ctx, id, domain.VerifyContactPointRequest{ExpectedVersion: version, EvidenceRef: "mail-roundtrip"}, actor, false)
}

func (f *orgFixture) retireContact(id string, version int64) (*registry.ContactPointOutcome, error) {
	return f.s.RetireContactPoint(f.ctx, id, domain.RetireContactPointRequest{ExpectedVersion: version, Reason: "bounced"}, cpChecker, false)
}

func (f *orgFixture) contacts(t *testing.T, partyID string, q domain.ContactPointQuery) []domain.ContactPoint {
	t.Helper()
	rows, err := f.s.ListPartyContactPoints(f.ctx, partyID, q)
	require.NoError(t, err)
	return rows
}

func TestPartyContactPoint_ProposeVerifyRetireAndTheOutcomeNeverEchoesTheAddress(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	c := f.proposeContact(t, p.PartyID, "  Ops@Example.COM ", domain.ContactBilling)
	assert.Equal(t, domain.ContactProposed, c.Status)
	assert.Empty(t, c.Value, "a command outcome never carries the address")
	assert.Equal(t, domain.ContactDigest("Ops@example.com"), c.ValueDigest)
	assert.Equal(t, int64(1), c.RecordVersion)
	assert.Nil(t, c.ValidTo)

	rows := f.contacts(t, p.PartyID, domain.ContactPointQuery{})
	require.Len(t, rows, 1)
	assert.Equal(t, "Ops@example.com", rows[0].Value, "normalised: trimmed, domain lower-cased")
	assert.Equal(t, domain.ContactBilling, rows[0].Purpose)

	v, err := f.verifyContact(c.ContactPointID, 1, cpChecker)
	require.NoError(t, err)
	assert.Equal(t, domain.ContactVerified, v.ContactPoint.Status)
	assert.Equal(t, int64(2), v.ContactPoint.RecordVersion)
	assert.Equal(t, cpChecker, v.ContactPoint.VerifiedBy)
	assert.NotNil(t, v.ContactPoint.VerifiedAt)
	assert.Equal(t, "mail-roundtrip", v.ContactPoint.EvidenceRef)
	assert.Empty(t, v.ContactPoint.Value)

	verified := f.contacts(t, p.PartyID, domain.ContactPointQuery{Status: domain.ContactVerified})
	require.Len(t, verified, 1)
	assert.Empty(t, f.contacts(t, p.PartyID, domain.ContactPointQuery{Status: domain.ContactProposed}))

	r, err := f.retireContact(c.ContactPointID, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.ContactRetired, r.ContactPoint.Status)
	require.NotNil(t, r.ContactPoint.ValidTo)
	assert.Empty(t, f.contacts(t, p.PartyID, domain.ContactPointQuery{}), "a retired contact point is no longer in force now")

	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'party.contact_point.proposed' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'party.contact_point.verified' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'party.contact_point.retired' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type LIKE 'party.contact_point.%' AND payload::text ILIKE '%example.com%'`),
		"no event carries the address")
	assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type LIKE 'party.contact_point.%' AND payload::text LIKE '%'||$1||'%'`, c.ValueDigest))
}

func TestPartyContactPoint_TheProposerCannotVerifyInTheStoreOrInTheTable(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	c := f.proposeContact(t, p.PartyID, "ops@example.com", "")

	_, err := f.verifyContact(c.ContactPointID, 1, cpMaker)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM party_contact_points WHERE status = 'VERIFIED' AND tenant_id = $1`, f.tenantID))

	// The table refuses it too, whatever the store does.
	err = f.execTenant(t, `UPDATE party_contact_points SET status = 'VERIFIED', verified_by = created_by, verified_at = now(), evidence_ref = 'e' WHERE contact_point_id = $1`, c.ContactPointID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pcp_independent_verification")

	_, err = f.verifyContact(c.ContactPointID, 1, cpChecker)
	require.NoError(t, err)
}

func TestPartyContactPoint_VerifyNeedsEvidenceAndTheRightVersionAndState(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	c := f.proposeContact(t, p.PartyID, "ops@example.com", "")

	_, err := f.s.VerifyContactPoint(f.ctx, c.ContactPointID, domain.VerifyContactPointRequest{ExpectedVersion: 1}, cpChecker, false)
	require.Error(t, err, "the table refuses a verification without evidence even if the service were skipped")

	_, err = f.verifyContact(c.ContactPointID, 7, cpChecker)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.verifyContact(c.ContactPointID, 0, cpChecker)
	require.ErrorIs(t, err, registry.ErrVersionRequired)

	_, err = f.verifyContact(c.ContactPointID, 1, cpChecker)
	require.NoError(t, err)
	_, err = f.verifyContact(c.ContactPointID, 2, "p-third")
	require.ErrorIs(t, err, registry.ErrStateConflict, "already verified")
	_, err = f.verifyContact(uuid.NewString(), 1, cpChecker)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.verifyContact("not-a-uuid", 1, cpChecker)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestPartyContactPoint_RetireIsFinalAndNeedsAReason(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	c := f.proposeContact(t, p.PartyID, "ops@example.com", "")

	_, err := f.retireContact(c.ContactPointID, 9)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	r, err := f.retireContact(c.ContactPointID, 1)
	require.NoError(t, err, "a PROPOSED contact point can be retired")
	_, err = f.retireContact(c.ContactPointID, r.ContactPoint.RecordVersion)
	require.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = f.verifyContact(c.ContactPointID, r.ContactPoint.RecordVersion, cpChecker)
	require.ErrorIs(t, err, registry.ErrStateConflict, "RETIRED is final")

	// The table agrees: no way back, no rewrite.
	for _, q := range []string{
		`UPDATE party_contact_points SET status = 'PROPOSED', retired_by = NULL, retired_at = NULL, valid_to = NULL, retire_reason = NULL WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET retire_reason = 'changed' WHERE contact_point_id = $1`,
		`DELETE FROM party_contact_points WHERE contact_point_id = $1`,
	} {
		require.Error(t, f.execTenant(t, q, c.ContactPointID), q)
	}

	// A retired address can be proposed again.
	again := f.proposeContact(t, p.PartyID, "ops@example.com", "")
	assert.NotEqual(t, c.ContactPointID, again.ContactPointID)
}

func TestPartyContactPoint_DuplicatesAreRefusedWhileLiveAndAllowedAfterRetire(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	other := f.contactParty(t)
	f.proposeContact(t, p.PartyID, "ops@example.com", "")

	_, err := f.tryPropose(p.PartyID, " ops@EXAMPLE.COM", domain.ContactBilling, nil)
	require.ErrorIs(t, err, registry.ErrConflict, "same address after normalisation, even for another purpose")
	_, err = f.tryPropose(other.PartyID, "ops@example.com", "", nil)
	require.NoError(t, err, "another party may use the same address")
	_, err = f.tryPropose(p.PartyID, "second@example.com", "", nil)
	require.NoError(t, err)

	// The index is the last word.
	err = f.execTenant(t, `INSERT INTO party_contact_points (contact_point_id, tenant_id, party_id, channel, value, value_digest, source_class, created_by)
		SELECT gen_random_uuid(), tenant_id, party_id, channel, value, value_digest, source_class, created_by FROM party_contact_points WHERE party_id = $1 LIMIT 1`, p.PartyID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pcp_one_live_value")
}

func TestPartyContactPoint_PartyMustBeOfTheTenantAndUsable(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	p := a.contactParty(t)
	c := a.proposeContact(t, p.PartyID, "ops@example.com", "")

	_, err := b.tryPropose(p.PartyID, "ops@example.com", "", nil)
	require.ErrorIs(t, err, registry.ErrNotFound, "a party of another tenant is not found")
	_, err = a.tryPropose(uuid.NewString(), "ops@example.com", "", nil)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = a.tryPropose("junk", "ops@example.com", "", nil)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.ListPartyContactPoints(b.ctx, p.PartyID, domain.ContactPointQuery{})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.VerifyContactPoint(b.ctx, c.ContactPointID, domain.VerifyContactPointRequest{ExpectedVersion: 1, EvidenceRef: "e"}, cpChecker, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.RetireContactPoint(b.ctx, c.ContactPointID, domain.RetireContactPointRequest{ExpectedVersion: 1, Reason: "r"}, cpChecker, false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	probeRLS(t, a, b, p.PartyID)

	// An INACTIVE party takes no new contact point.
	_, err = a.pool.Exec(context.Background(), `UPDATE parties SET status = 'INACTIVE' WHERE party_id = $1`, p.PartyID)
	require.NoError(t, err)
	_, err = a.tryPropose(p.PartyID, "new@example.com", "", nil)
	require.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = a.verifyContact(c.ContactPointID, 1, cpChecker)
	require.ErrorIs(t, err, registry.ErrStateConflict, "nor a verification")
	_, err = a.retireContact(c.ContactPointID, 1)
	require.NoError(t, err, "but a contact point can always be retired")
}

func TestPartyContactPoint_AsOfReadsWhatWasInForce(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	past := time.Now().UTC().Add(-48 * time.Hour)
	future := time.Now().UTC().Add(48 * time.Hour)
	ended, err := f.tryPropose(p.PartyID, "old@example.com", "", &past)
	require.NoError(t, err)
	live := f.proposeContact(t, p.PartyID, "live@example.com", "")
	_, err = f.tryPropose(p.PartyID, "later@example.com", "", &future)
	require.NoError(t, err)

	r, err := f.retireContact(ended.ContactPointID, 1)
	require.NoError(t, err)
	endedAt := *r.ContactPoint.ValidTo

	names := func(rows []domain.ContactPoint) (out []string) {
		for _, c := range rows {
			out = append(out, c.Value)
		}
		return
	}
	now := f.contacts(t, p.PartyID, domain.ContactPointQuery{})
	assert.Equal(t, []string{"live@example.com"}, names(now))
	assert.Equal(t, live.ContactPointID, now[0].ContactPointID)

	assert.ElementsMatch(t, []string{"old@example.com"}, names(f.contacts(t, p.PartyID, domain.ContactPointQuery{AsOf: past.Add(time.Hour)})),
		"as it was before the live one began")
	assert.Empty(t, f.contacts(t, p.PartyID, domain.ContactPointQuery{AsOf: past.Add(-time.Hour)}), "before anything began")
	assert.NotContains(t, names(f.contacts(t, p.PartyID, domain.ContactPointQuery{AsOf: endedAt})), "old@example.com", "valid_to is exclusive")
	assert.Contains(t, names(f.contacts(t, p.PartyID, domain.ContactPointQuery{AsOf: endedAt.Add(-time.Microsecond)})), "old@example.com")
	assert.ElementsMatch(t, []string{"live@example.com", "later@example.com"}, names(f.contacts(t, p.PartyID, domain.ContactPointQuery{AsOf: future.Add(time.Hour)})))

	// Channel and status filters combine with as_of.
	assert.Empty(t, f.contacts(t, p.PartyID, domain.ContactPointQuery{Status: domain.ContactVerified}))
	assert.Len(t, f.contacts(t, p.PartyID, domain.ContactPointQuery{Channel: domain.ContactEmail, Status: domain.ContactProposed}), 1)
}

func TestPartyContactPoint_TableConstraintsAndImmutability(t *testing.T) {
	f := newORGFixture(t)
	p := f.contactParty(t)
	c := f.proposeContact(t, p.PartyID, "ops@example.com", "")
	insert := func(extra string) string {
		return `INSERT INTO party_contact_points (contact_point_id, tenant_id, party_id, channel, value, value_digest, purpose, status, source_class, created_by` + extra
	}
	base := func(channel, purpose, status, source string) error {
		return f.execTenant(t, insert(`) VALUES (gen_random_uuid(), $1, $2, $3, 'z@example.com', $4, $5, $6, $7, 'p')`),
			f.tenantID, p.PartyID, channel, strings.Repeat("b", 64), purpose, status, source)
	}
	require.NoError(t, base("EMAIL", "GENERAL", "PROPOSED", "TENANT_ASSERTED"))
	require.Error(t, base("SMS", "GENERAL", "PROPOSED", "TENANT_ASSERTED"), "EMAIL only")
	require.Error(t, base("EMAIL", "MARKETING", "PROPOSED", "TENANT_ASSERTED"))
	require.Error(t, base("EMAIL", "GENERAL", "ACTIVE", "TENANT_ASSERTED"))
	require.Error(t, base("EMAIL", "GENERAL", "PROPOSED", "GUESS"))
	require.Error(t, base("EMAIL", "GENERAL", "VERIFIED", "TENANT_ASSERTED"), "a VERIFIED row needs verified_by and verified_at")
	require.Error(t, f.execTenant(t, insert(`) VALUES (gen_random_uuid(), $1, $2, 'EMAIL', 'z@example.com', 'not-a-digest', 'GENERAL', 'PROPOSED', 'TENANT_ASSERTED', 'p')`), f.tenantID, p.PartyID))
	require.Error(t, f.execTenant(t, insert(`, valid_from, valid_to) VALUES (gen_random_uuid(), $1, $2, 'EMAIL', 'z@example.com', $3, 'GENERAL', 'RETIRED', 'TENANT_ASSERTED', 'p', now(), now() - interval '1 day')`),
		f.tenantID, p.PartyID, strings.Repeat("c", 64)), "valid_from < valid_to")
	require.Error(t, f.execTenant(t, insert(`) VALUES (gen_random_uuid(), $1, $2, 'EMAIL', $3, $4, 'GENERAL', 'PROPOSED', 'TENANT_ASSERTED', 'p')`),
		f.tenantID, p.PartyID, strings.Repeat("a", 250)+"@x.io", strings.Repeat("d", 64)), "value is at most 254 characters")
	require.Error(t, f.execTenant(t, insert(`) VALUES (gen_random_uuid(), $1, $2, 'EMAIL', 'z@example.com', $3, 'GENERAL', 'PROPOSED', 'TENANT_ASSERTED', 'p')`),
		f.tenantID, uuid.NewString(), strings.Repeat("e", 64)), "the party must exist in the tenant")

	for _, q := range []string{
		`UPDATE party_contact_points SET value = 'other@example.com' WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET value_digest = '` + strings.Repeat("f", 64) + `' WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET channel = 'EMAIL', party_id = gen_random_uuid() WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET purpose = 'LEGAL_NOTICE' WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET valid_from = valid_from - interval '1 day' WHERE contact_point_id = $1`,
		`UPDATE party_contact_points SET created_by = 'someone-else' WHERE contact_point_id = $1`,
		`DELETE FROM party_contact_points WHERE contact_point_id = $1`,
	} {
		require.Error(t, f.execTenant(t, q, c.ContactPointID), q)
	}
	// A verification is not rewritten.
	_, err := f.verifyContact(c.ContactPointID, 1, cpChecker)
	require.NoError(t, err)
	require.Error(t, f.execTenant(t, `UPDATE party_contact_points SET verified_by = 'p-other' WHERE contact_point_id = $1`, c.ContactPointID))
	require.Error(t, f.execTenant(t, `UPDATE party_contact_points SET status = 'PROPOSED', verified_by = NULL, verified_at = NULL WHERE contact_point_id = $1`, c.ContactPointID))
}

func TestPartyContactPoint_DownMigrationRefusesWhileContactPointsExist(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func() bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = 'party_contact_points'`).Scan(&n))
		return n > 0
	}

	p := f.contactParty(t)
	f.proposeContact(t, p.PartyID, "ops@example.com", "")
	_, err := f.pool.Exec(ctx, read("000028_party_contact_points.down.sql"))
	require.Error(t, err, "down is refused while contact points exist")
	assert.Contains(t, err.Error(), "000028 down refused")
	assert.True(t, exists())

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000028_party_contact_points.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists())
	_, err = f2.pool.Exec(ctx, read("000028_party_contact_points.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists())
}

// probeRLS reads and writes the table as a non-superuser role, so the policies are load-bearing: the pooled superuser
// would bypass them.
func probeRLS(t *testing.T, a, b *orgFixture, partyID string) {
	t.Helper()
	ctx := context.Background()
	var rls, forced bool
	require.NoError(t, a.pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'party_contact_points'::regclass`).Scan(&rls, &forced))
	assert.True(t, rls)
	assert.True(t, forced)

	role := "cp_probe_" + a.tenantID[:8]
	_, _ = a.pool.Exec(ctx, `DROP OWNED BY `+role)
	_, _ = a.pool.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	if _, err := a.pool.Exec(ctx, `CREATE ROLE `+role+` NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD 'probe'`); err != nil {
		t.Skipf("cannot create a probe role here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = a.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	for _, g := range []string{`GRANT USAGE ON SCHEMA public TO ` + role, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role} {
		_, err := a.pool.Exec(ctx, g)
		require.NoError(t, err)
	}
	probe, err := pgxpool.New(ctx, probeDSN(t, role, "probe"))
	if err != nil {
		t.Skipf("cannot open a probe connection: %v", err)
	}
	t.Cleanup(probe.Close)

	count := func(tenant string) int {
		tx, err := probe.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
		require.NoError(t, err)
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM party_contact_points`).Scan(&n))
		return n
	}
	assert.Equal(t, 1, count(a.tenantID))
	assert.Equal(t, 0, count(b.tenantID), "another tenant sees no contact point")
	assert.Equal(t, 0, count(uuid.NewString()))

	tx, err := probe.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, b.tenantID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO party_contact_points (contact_point_id, tenant_id, party_id, channel, value, value_digest, source_class, created_by)
		VALUES (gen_random_uuid(), $1, $2, 'EMAIL', 'x@example.com', $3, 'TENANT_ASSERTED', 'p')`, a.tenantID, partyID, strings.Repeat("a", 64))
	require.Error(t, err, "a row for another tenant is refused by the policy")
}
