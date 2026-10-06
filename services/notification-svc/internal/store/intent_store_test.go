package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func newIntent(t *testing.T, s *store.PgStore, tenant, key string) *domain.CommunicationIntent {
	t.Helper()
	i, err := s.CreateIntent(tenantCtx(tenant), domain.CreateIntentParams{LegalEntityID: "le-us", IntentKey: key,
		DisplayName: "Payslip available", DomainOwner: "Payroll", CreatedByPrincipalID: "owner"})
	require.NoError(t, err)
	return i
}

func payslipContract() map[string]domain.VariableSpec {
	return map[string]domain.VariableSpec{
		"first_name": {Type: "STRING", Required: true, Sensitivity: "S1"},
		"period":     {Type: "STRING", Required: true, Sensitivity: "S0"},
		"net_pay":    {Type: "NUMBER", Required: true, Sensitivity: "S3"},
		"link":       {Type: "URL", Required: false, Sensitivity: "S1"},
	}
}

func draftVersion(t *testing.T, s *store.PgStore, tenant, intentID string, tweak func(*domain.CreateIntentVersionParams)) *domain.IntentVersion {
	t.Helper()
	p := domain.CreateIntentVersionParams{IntentID: intentID, PurposeClass: "T0", EvidenceClass: "E2",
		AllowedChannels: []string{"EMAIL", "IN_APP"}, VariableContract: payslipContract(), CreatedByPrincipalID: "maker"}
	if tweak != nil {
		tweak(&p)
	}
	v, err := s.CreateIntentVersion(tenantCtx(tenant), p)
	require.NoError(t, err)
	return v
}

func publishVersion(t *testing.T, s *store.PgStore, tenant string, v *domain.IntentVersion, effective *time.Time) *domain.IntentVersion {
	t.Helper()
	ctx := tenantCtx(tenant)
	_, err := s.ValidateIntentVersion(ctx, v.VersionID)
	require.NoError(t, err)
	_, err = s.ApproveIntentVersion(ctx, domain.ApproveIntentVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "checker"})
	require.NoError(t, err)
	out, err := s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "checker", EffectiveFrom: effective})
	require.NoError(t, err)
	return out
}

func TestIntent_StableIdentityKeyShapeUniquenessAndIsolation(t *testing.T) {
	s := store.New(openTestPool(t))
	i := newIntent(t, s, "tenant-int", "payroll.payslip_available")
	assert.Equal(t, "ACTIVE", i.Status)

	_, err := s.CreateIntent(tenantCtx("tenant-int"), domain.CreateIntentParams{LegalEntityID: "le-us", IntentKey: "payroll.payslip_available", DisplayName: "x", DomainOwner: "y", CreatedByPrincipalID: "o"})
	assert.ErrorIs(t, err, domain.ErrIntentKeyTaken)
	for _, bad := range []string{"Payroll.Payslip", "payroll..x", ".x", "payroll payslip", "1payroll", ""} {
		_, err := s.CreateIntent(tenantCtx("tenant-int"), domain.CreateIntentParams{LegalEntityID: "le-us", IntentKey: bad, DisplayName: "x", DomainOwner: "y", CreatedByPrincipalID: "o"})
		assert.ErrorIs(t, err, domain.ErrIntentInvalid, "key %q", bad)
	}
	// The same key in another tenant is a different intent; its own tenant cannot see this one.
	other := newIntent(t, s, "tenant-int-other", "payroll.payslip_available")
	assert.NotEqual(t, i.IntentID, other.IntentID)
	_, err = s.GetIntent(tenantCtx("tenant-int-other"), i.IntentID)
	assert.ErrorIs(t, err, domain.ErrIntentNotFound, "another tenant cannot read this intent")
	_, err = s.GetIntent(tenantCtx("tenant-int"), "not-a-uuid")
	assert.ErrorIs(t, err, domain.ErrIntentNotFound)
}

func TestIntentVersion_LifecycleAndMakerChecker(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx("tenant-int")
	i := newIntent(t, s, "tenant-int", "hr.review_invitation")
	v := draftVersion(t, s, "tenant-int", i.IntentID, nil)
	assert.Equal(t, 1, v.VersionNumber)
	assert.Equal(t, "DRAFT", v.Status)
	assert.Equal(t, 200, v.VariableContract["first_name"].MaxLength, "a STRING without a length gets the default")

	_, err := s.ApproveIntentVersion(ctx, domain.ApproveIntentVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "checker"})
	assert.ErrorIs(t, err, domain.ErrIntentVersionNotReview, "cannot approve a draft")
	_, err = s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "checker"})
	assert.ErrorIs(t, err, domain.ErrIntentVersionNotApproved, "cannot publish an unapproved version")

	_, err = s.ValidateIntentVersion(ctx, v.VersionID)
	require.NoError(t, err)
	_, err = s.ValidateIntentVersion(ctx, v.VersionID)
	assert.ErrorIs(t, err, domain.ErrIntentVersionNotDraft)
	_, err = s.ApproveIntentVersion(ctx, domain.ApproveIntentVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "maker"})
	assert.ErrorIs(t, err, domain.ErrIntentVersionSelfApproval)
	approved, err := s.ApproveIntentVersion(ctx, domain.ApproveIntentVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "checker"})
	require.NoError(t, err)
	assert.Equal(t, "APPROVED", approved.Status)
	pub, err := s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "checker"})
	require.NoError(t, err)
	require.NotNil(t, pub.EffectiveFrom)
	require.NotNil(t, pub.PublishedAt)

	// A second version gets the next number.
	assert.Equal(t, 2, draftVersion(t, s, "tenant-int", i.IntentID, nil).VersionNumber)
}

func TestIntentVersion_RefusesAnInvalidContractBeforeStoringIt(t *testing.T) {
	s := store.New(openTestPool(t))
	i := newIntent(t, s, "tenant-int", "billing.invoice_issued")
	bad := map[string]func(*domain.CreateIntentVersionParams){
		"unknown purpose class":          func(p *domain.CreateIntentVersionParams) { p.PurposeClass = "Z9" },
		"unknown evidence class":         func(p *domain.CreateIntentVersionParams) { p.EvidenceClass = "E9" },
		"no channels":                    func(p *domain.CreateIntentVersionParams) { p.AllowedChannels = nil },
		"unknown channel":                func(p *domain.CreateIntentVersionParams) { p.AllowedChannels = []string{"CARRIER_PIGEON"} },
		"duplicate channel":              func(p *domain.CreateIntentVersionParams) { p.AllowedChannels = []string{"EMAIL", "EMAIL"} },
		"marketing allowed on security":  func(p *domain.CreateIntentVersionParams) { p.PurposeClass, p.MarketingAllowed = "S0", true },
		"marketing allowed on transact.": func(p *domain.CreateIntentVersionParams) { p.MarketingAllowed = true },
		"variable without a sensitivity": func(p *domain.CreateIntentVersionParams) {
			p.VariableContract["x"] = domain.VariableSpec{Type: "STRING"}
		},
		"variable without a type": func(p *domain.CreateIntentVersionParams) {
			p.VariableContract["x"] = domain.VariableSpec{Sensitivity: "S1"}
		},
		"variable with a bad name": func(p *domain.CreateIntentVersionParams) {
			p.VariableContract["Bad-Name"] = domain.VariableSpec{Type: "STRING", Sensitivity: "S1"}
		},
		"max_length on a number": func(p *domain.CreateIntentVersionParams) {
			p.VariableContract["n"] = domain.VariableSpec{Type: "NUMBER", Sensitivity: "S1", MaxLength: 5}
		},
		"max_length beyond the limit": func(p *domain.CreateIntentVersionParams) {
			p.VariableContract["s"] = domain.VariableSpec{Type: "STRING", Sensitivity: "S1", MaxLength: 99999}
		},
	}
	for name, tweak := range bad {
		p := domain.CreateIntentVersionParams{IntentID: i.IntentID, PurposeClass: "T0", EvidenceClass: "E2", AllowedChannels: []string{"EMAIL"},
			VariableContract: payslipContract(), CreatedByPrincipalID: "maker"}
		tweak(&p)
		_, err := s.CreateIntentVersion(tenantCtx("tenant-int"), p)
		assert.ErrorIs(t, err, domain.ErrIntentInvalid, name)
	}
	// Marketing may be allowed on a marketing intent.
	ok := draftVersion(t, s, "tenant-int", i.IntentID, func(p *domain.CreateIntentVersionParams) { p.PurposeClass, p.MarketingAllowed = "M1", true })
	assert.True(t, ok.MarketingAllowed)
}

// The database refuses what the service would, without the service.
func TestIntentVersion_DatabaseGuards(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	bg := context.Background()
	i := newIntent(t, s, "tenant-int-db", "legal.consultation_notice")
	v := draftVersion(t, s, "tenant-int-db", i.IntentID, nil)

	// Content is immutable from the moment it is written.
	for name, set := range map[string]string{
		"change the purpose class": `purpose_class='S0'`,
		"change the contract":      `variable_contract='{}'::jsonb`,
		"change the channels":      `allowed_channels='["SMS"]'::jsonb`,
		"turn marketing on":        `marketing_allowed=true`,
	} {
		_, err := pool.Exec(bg, `UPDATE communication_intent_versions SET `+set+` WHERE version_id=$1`, v.VersionID)
		assert.Error(t, err, name)
	}
	// Illegal edges, even with all the evidence supplied.
	_, err := pool.Exec(bg, `UPDATE communication_intent_versions SET status='PUBLISHED', approved_by_principal_id='x', approved_at=now(), published_at=now(), effective_from=now() WHERE version_id=$1`, v.VersionID)
	assert.Error(t, err, "DRAFT straight to PUBLISHED")
	_, err = pool.Exec(bg, `UPDATE communication_intent_versions SET status='APPROVED', approved_by_principal_id='x', approved_at=now() WHERE version_id=$1`, v.VersionID)
	assert.Error(t, err, "DRAFT straight to APPROVED")
	_, err = pool.Exec(bg, `INSERT INTO communication_intent_versions (intent_id, tenant_id, legal_entity_id, version_number, purpose_class, evidence_class, allowed_channels, created_by_principal_id, status)
		VALUES ($1,'tenant-int-db','le-us',9,'T0','E1','["EMAIL"]','m','PUBLISHED')`, i.IntentID)
	assert.Error(t, err, "inserted already published")
	// Maker-checker in SQL.
	_, err = s.ValidateIntentVersion(tenantCtx("tenant-int-db"), v.VersionID)
	require.NoError(t, err)
	_, err = pool.Exec(bg, `UPDATE communication_intent_versions SET status='APPROVED', approved_by_principal_id='maker', approved_at=now() WHERE version_id=$1`, v.VersionID)
	assert.Error(t, err, "self-approval written directly")
	// Published versions and intents are never deleted, an intent identity never changes.
	_, err = s.ApproveIntentVersion(tenantCtx("tenant-int-db"), domain.ApproveIntentVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "checker"})
	require.NoError(t, err)
	pub, err := s.PublishIntentVersion(tenantCtx("tenant-int-db"), domain.PublishIntentVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "checker"})
	require.NoError(t, err)
	_, err = pool.Exec(bg, `DELETE FROM communication_intent_versions WHERE version_id=$1`, pub.VersionID)
	assert.Error(t, err)
	_, err = pool.Exec(bg, `UPDATE communication_intent_versions SET effective_from = effective_from + interval '1 day' WHERE version_id=$1`, pub.VersionID)
	assert.Error(t, err, "a published version's dates cannot move")
	_, err = pool.Exec(bg, `UPDATE communication_intents SET intent_key='other.key' WHERE intent_id=$1`, i.IntentID)
	assert.Error(t, err, "an intent identity is immutable")
	_, err = pool.Exec(bg, `DELETE FROM communication_intents WHERE intent_id=$1`, i.IntentID)
	assert.Error(t, err)
}

// NCD-01 4.5 and 9.2: what was in force at time T, as known at time K.
func TestIntent_EffectiveDatingAndKnowledgeTime(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-int-time"
	ctx := tenantCtx(tenant)
	i := newIntent(t, s, tenant, "tax.filing_reminder")

	_, err := s.EffectiveIntentVersion(ctx, i.IntentID, time.Now(), time.Now())
	assert.ErrorIs(t, err, domain.ErrIntentNotEffective, "nothing published yet")
	_, err = s.EffectiveIntentVersion(ctx, uuid.NewString(), time.Now(), time.Now())
	assert.ErrorIs(t, err, domain.ErrIntentNotFound)

	v1 := publishVersion(t, s, tenant, draftVersion(t, s, tenant, i.IntentID, nil), nil)
	future := time.Now().Add(48 * time.Hour)
	v2 := publishVersion(t, s, tenant, draftVersion(t, s, tenant, i.IntentID, func(p *domain.CreateIntentVersionParams) { p.EvidenceClass = "E3" }), &future)

	now := time.Now().Add(time.Second)
	got, err := s.EffectiveIntentVersion(ctx, i.IntentID, now, now)
	require.NoError(t, err)
	assert.Equal(t, v1.VersionID, got.VersionID, "a future-dated version is not used early")

	got, err = s.EffectiveIntentVersion(ctx, i.IntentID, future.Add(time.Minute), now)
	require.NoError(t, err)
	assert.Equal(t, v2.VersionID, got.VersionID, "once its date is reached, the new version applies")
	assert.Equal(t, "E3", got.EvidenceClass)

	// Knowledge time: asked as the platform knew it before v2 was published, v2 does not exist yet.
	got, err = s.EffectiveIntentVersion(ctx, i.IntentID, future.Add(time.Minute), v1.PublishedAt.Add(time.Microsecond))
	require.NoError(t, err)
	assert.Equal(t, v1.VersionID, got.VersionID, "as known before v2 was published, v1 applied then")

	// Before anything was effective.
	_, err = s.EffectiveIntentVersion(ctx, i.IntentID, v1.EffectiveFrom.Add(-time.Hour), now)
	assert.ErrorIs(t, err, domain.ErrIntentNotEffective)
}

// Publication is never backdated and never reorders history.
func TestIntent_PublicationCannotBackdateOrReorder(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-int-order"
	ctx := tenantCtx(tenant)
	i := newIntent(t, s, tenant, "ops.maintenance_notice")
	v1 := draftVersion(t, s, tenant, i.IntentID, nil)
	late := time.Now().Add(72 * time.Hour)
	publishVersion(t, s, tenant, v1, &late)

	v2 := draftVersion(t, s, tenant, i.IntentID, nil)
	_, err := s.ValidateIntentVersion(ctx, v2.VersionID)
	require.NoError(t, err)
	_, err = s.ApproveIntentVersion(ctx, domain.ApproveIntentVersionParams{VersionID: v2.VersionID, ApprovedByPrincipalID: "checker"})
	require.NoError(t, err)

	past := time.Now().Add(-time.Hour)
	_, err = s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v2.VersionID, PublishedByPrincipalID: "checker", EffectiveFrom: &past})
	assert.ErrorIs(t, err, domain.ErrIntentEffectiveFromInvalid, "no backdating")
	earlier := time.Now().Add(24 * time.Hour) // before v1's 72h
	_, err = s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v2.VersionID, PublishedByPrincipalID: "checker", EffectiveFrom: &earlier})
	assert.ErrorIs(t, err, domain.ErrIntentEffectiveFromInvalid, "must be later than every published version")
	after := late.Add(time.Hour)
	_, err = s.PublishIntentVersion(ctx, domain.PublishIntentVersionParams{VersionID: v2.VersionID, PublishedByPrincipalID: "checker", EffectiveFrom: &after})
	assert.NoError(t, err)
}

func TestIntent_RetirementEndsTheFutureNotTheHistory(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-int-retire"
	ctx := tenantCtx(tenant)
	i := newIntent(t, s, tenant, "support.case_update")
	v := publishVersion(t, s, tenant, draftVersion(t, s, tenant, i.IntentID, nil), nil)

	retired, err := s.RetireIntent(ctx, i.IntentID, "owner")
	require.NoError(t, err)
	assert.Equal(t, "RETIRED", retired.Status)
	_, err = s.RetireIntent(ctx, i.IntentID, "owner")
	assert.ErrorIs(t, err, domain.ErrIntentRetired)
	_, err = s.RetireIntent(ctx, uuid.NewString(), "owner")
	assert.ErrorIs(t, err, domain.ErrIntentNotFound)

	after := retired.RetiredAt.Add(time.Hour)
	_, err = s.EffectiveIntentVersion(ctx, i.IntentID, after, after)
	assert.ErrorIs(t, err, domain.ErrIntentNotEffective, "after retirement nothing is in force")
	// A message from before the retirement still reconstructs, even asked about much later.
	got, err := s.EffectiveIntentVersion(ctx, i.IntentID, *v.EffectiveFrom, after)
	require.NoError(t, err)
	assert.Equal(t, v.VersionID, got.VersionID)

	_, err = s.CreateIntentVersion(ctx, domain.CreateIntentVersionParams{IntentID: i.IntentID, PurposeClass: "T0", EvidenceClass: "E1",
		AllowedChannels: []string{"EMAIL"}, VariableContract: map[string]domain.VariableSpec{}, CreatedByPrincipalID: "m"})
	assert.True(t, errors.Is(err, domain.ErrIntentRetired))
}

func TestMigration000020_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	// 000025, 000022 and 000021 depend on 000020, so they are rolled back first and re-applied last.
	for _, f := range []string{"000027_more_canonical_events.down.sql", "000026_notice_event_sequence.down.sql", "000025_regulated_notices.down.sql", "000022_privacy_binding_and_evidence.down.sql", "000021_intent_binding.down.sql", "000020_communication_intents.down.sql", "000020_communication_intents.up.sql", "000021_intent_binding.up.sql", "000022_privacy_binding_and_evidence.up.sql", "000025_regulated_notices.up.sql", "000026_notice_event_sequence.up.sql", "000027_more_canonical_events.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_name IN ('communication_intents','communication_intent_versions')`).Scan(&n))
	assert.Equal(t, 2, n)
}
