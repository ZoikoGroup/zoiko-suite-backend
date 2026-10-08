package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func publishedIntentFor(t *testing.T, s *store.PgStore, tenant, key string) (*domain.CommunicationIntent, *domain.IntentVersion) {
	t.Helper()
	i := newIntent(t, s, tenant, key)
	v := publishVersion(t, s, tenant, draftVersion(t, s, tenant, i.IntentID, nil), nil)
	return i, v
}

func boundTemplateDef(t *testing.T, s *store.PgStore, tenant, intentID string) (*domain.TemplateDefinition, error) {
	t.Helper()
	return s.CreateTemplate(tenantCtx(tenant), domain.CreateTemplateParams{LegalEntityID: "le-us", Name: "Payslip", BusinessPurpose: "notice",
		OwnerPrincipalID: "owner", IntentID: intentID})
}

func TestTemplateBinding_IsRefusedForAnIntentThatCannotGovern(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-bind"
	good, _ := publishedIntentFor(t, s, tenant, "payroll.payslip_available")

	tmpl, err := boundTemplateDef(t, s, tenant, good.IntentID)
	require.NoError(t, err)
	require.NotNil(t, tmpl.IntentID)
	assert.Equal(t, good.IntentID, *tmpl.IntentID)
	got, err := s.GetTemplate(tenantCtx(tenant), tmpl.TemplateID)
	require.NoError(t, err)
	require.NotNil(t, got.IntentID, "the binding is stored and read back")

	_, err = boundTemplateDef(t, s, tenant, uuid.NewString())
	assert.ErrorIs(t, err, domain.ErrIntentNotFound, "no such intent")
	_, err = boundTemplateDef(t, s, tenant, "not-a-uuid")
	assert.ErrorIs(t, err, domain.ErrIntentNotFound)

	other, _ := publishedIntentFor(t, s, "tenant-bind-other", "payroll.payslip_available")
	_, err = boundTemplateDef(t, s, tenant, other.IntentID)
	assert.ErrorIs(t, err, domain.ErrIntentNotFound, "another tenant's intent looks like no intent")

	retired := newIntent(t, s, tenant, "payroll.retired_notice")
	_, err = s.RetireIntent(tenantCtx(tenant), retired.IntentID, "owner")
	require.NoError(t, err)
	_, err = boundTemplateDef(t, s, tenant, retired.IntentID)
	assert.ErrorIs(t, err, domain.ErrIntentRetired)

	// A template of a different legal entity cannot borrow the intent.
	_, err = s.CreateTemplate(tenantCtx(tenant), domain.CreateTemplateParams{LegalEntityID: "le-uk", Name: "x", BusinessPurpose: "y", OwnerPrincipalID: "owner", IntentID: good.IntentID})
	assert.ErrorIs(t, err, domain.ErrIntentInvalid)
}

func TestTemplateBinding_IsFixedAndTenantCheckedByTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	bg := context.Background()
	a, _ := publishedIntentFor(t, s, "tenant-bind-db", "payroll.payslip_available")
	b, _ := publishedIntentFor(t, s, "tenant-bind-db", "payroll.bonus_notice")
	foreign, _ := publishedIntentFor(t, s, "tenant-bind-db-other", "payroll.payslip_available")
	tmpl, err := boundTemplateDef(t, s, "tenant-bind-db", a.IntentID)
	require.NoError(t, err)

	_, err = pool.Exec(bg, `UPDATE template_definitions SET intent_id=$2::uuid WHERE template_id=$1`, tmpl.TemplateID, b.IntentID)
	assert.Error(t, err, "a template cannot be rebound to a more permissive intent after review")
	_, err = pool.Exec(bg, `UPDATE template_definitions SET intent_id=NULL WHERE template_id=$1`, tmpl.TemplateID)
	assert.Error(t, err, "nor unbound")
	plain, err := s.CreateTemplate(tenantCtx("tenant-bind-db"), domain.CreateTemplateParams{LegalEntityID: "le-us", Name: "plain", BusinessPurpose: "y", OwnerPrincipalID: "owner"})
	require.NoError(t, err)
	_, err = pool.Exec(bg, `UPDATE template_definitions SET intent_id=$2::uuid WHERE template_id=$1`, plain.TemplateID, a.IntentID)
	assert.Error(t, err, "an unbound template cannot be bound later either")
	_, err = pool.Exec(bg, `INSERT INTO template_definitions (tenant_id, legal_entity_id, name, business_purpose, owner_principal_id, intent_id)
		VALUES ('tenant-bind-db','le-us','x','y','o',$1::uuid)`, foreign.IntentID)
	assert.Error(t, err, "a template cannot be bound to another tenant's intent, even bypassing the service")
}

// The wording must stay inside the contract that governs it.
func TestTemplateBinding_WordingMustConformToTheContract(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-bind-conform"
	ctx := tenantCtx(tenant)
	i, _ := publishedIntentFor(t, s, tenant, "payroll.payslip_available")
	tmpl, err := boundTemplateDef(t, s, tenant, i.IntentID)
	require.NoError(t, err)

	create := func(schema []string, subject string, subjectVars []string) (*domain.TemplateVersion, error) {
		return s.CreateVersion(ctx, domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US",
			Content: "<p>{{.first_name}} {{.period}}</p>", VariableSchema: schema, Subject: subject, SubjectVariables: subjectVars, CreatedByPrincipalID: "owner"})
	}
	ok, err := create([]string{"first_name", "period"}, "Payslip for {{.period}}", []string{"period"})
	require.NoError(t, err)
	assert.Equal(t, "DRAFT", ok.Status)

	// A variable the intent does not govern.
	_, err = create([]string{"first_name", "ssn"}, "", nil)
	assert.ErrorIs(t, err, domain.ErrTemplateVariableInvalid)
	// INV-17 by sensitivity, not by the author's say-so: net_pay is S3, so it can never be in a subject.
	_, err = create([]string{"first_name", "net_pay"}, "Net pay {{.net_pay}}", []string{"net_pay"})
	assert.ErrorIs(t, err, domain.ErrSubjectInvalid)
	// A subject variable the contract does not declare.
	_, err = create([]string{"first_name"}, "Hello {{.first_name}}", []string{"first_name", "ghost"})
	assert.Error(t, err)

	// An unbound template is not held to any contract.
	plain, err := s.CreateTemplate(ctx, domain.CreateTemplateParams{LegalEntityID: "le-us", Name: "plain", BusinessPurpose: "y", OwnerPrincipalID: "owner"})
	require.NoError(t, err)
	_, err = s.CreateVersion(ctx, domain.CreateVersionParams{TemplateID: plain.TemplateID, Locale: "en-US", Content: "<p>{{.anything}}</p>",
		VariableSchema: []string{"anything"}, CreatedByPrincipalID: "owner"})
	assert.NoError(t, err)

	// Validation re-checks conformance against the contract in force now.
	_, err = s.ValidateTemplate(ctx, ok.VersionID)
	assert.NoError(t, err)
}

// Wording cannot be authored against a contract that does not exist yet.
func TestTemplateBinding_NeedsAnIntentVersionInForce(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-bind-early"
	i := newIntent(t, s, tenant, "payroll.payslip_available") // identity only, nothing published
	tmpl, err := boundTemplateDef(t, s, tenant, i.IntentID)
	require.NoError(t, err)
	_, err = s.CreateVersion(tenantCtx(tenant), domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>x</p>", CreatedByPrincipalID: "owner"})
	assert.ErrorIs(t, err, domain.ErrIntentNotEffective)
}

// INV-04: the notification records the exact intent version, fixed for good, and its events say so.
func TestNotification_IsPinnedToTheIntentVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-pin"
	ctx := tenantCtx(tenant)
	_, v := publishedIntentFor(t, s, tenant, "payroll.payslip_available")

	n := newNotification(tenant, "entity-1", "recipient-1", "corr-pin-1")
	n.CommunicationClass, n.IntentVersionID = "T0", v.VersionID
	created, err := s.CreateNotification(ctx, n)
	require.NoError(t, err)
	require.True(t, created)
	got, err := s.GetNotification(ctx, n.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, v.VersionID, got.IntentVersionID)

	_, err = pool.Exec(context.Background(), `UPDATE notifications SET intent_version_id=NULL WHERE notification_id=$1`, n.NotificationID)
	assert.Error(t, err, "the pin cannot be removed")
	other := draftVersion(t, s, tenant, v.IntentID, nil)
	_, err = pool.Exec(context.Background(), `UPDATE notifications SET intent_version_id=$2::uuid WHERE notification_id=$1`, n.NotificationID, other.VersionID)
	assert.Error(t, err, "nor moved to another version")

	bad := newNotification(tenant, "entity-1", "recipient-1", "corr-pin-2")
	bad.IntentVersionID = uuid.NewString()
	_, err = s.CreateNotification(ctx, bad)
	assert.Error(t, err, "a pin must name a real intent version")

	require.NoError(t, complete(t, s, tenant, n.NotificationID, "SENT", "", receiptFor("<pin-1@example.com>")))
	evs := outboxFor(t, pool, n.NotificationID)
	require.Contains(t, evs, "notification.sent")
	assert.Equal(t, v.VersionID, evs["notification.sent"]["intent_version_id"], "events name the exact intent version")
}

func TestMigration000021_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000021_intent_binding.down.sql", "000021_intent_binding.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name='template_definitions' AND column_name='intent_id') OR (table_name='notifications' AND column_name='intent_version_id')`).Scan(&n))
	assert.Equal(t, 2, n)
}
