package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// BIZ-03: a missing variable schema blocks publication. Wording that uses a variable the
// schema does not declare would render it as blank text at send time, so validation
// refuses it and the version stays DRAFT.
func TestPgStore_ValidateTemplate_RefusesUndeclaredVariables(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("local"))
	ctx := tenantCtx("tenant-biz03-vars")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	cases := []struct {
		name, content string
		schema        []string
		undeclared    []string
	}{
		{"no schema at all", "<p>Hello {{.name}}</p>", nil, []string{"name"}},
		{"partial schema", "<p>{{.name}} owes {{.amount}}</p>", []string{"name"}, []string{"amount"}},
		{"used inside a condition", "{{if .vip}}<p>{{.name}}</p>{{end}}", []string{"name"}, []string{"vip"}},
	}
	for _, c := range cases {
		v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
			TemplateID: tmpl.TemplateID, Locale: "en-US", Content: c.content, VariableSchema: c.schema, CreatedByPrincipalID: "owner-1",
		})
		require.NoError(t, err, c.name)
		_, err = s.ValidateTemplate(ctx, v.VersionID)
		var got domain.ErrTemplateVariablesUndeclared
		require.True(t, errors.As(err, &got), "%s: got %v", c.name, err)
		assert.Equal(t, c.undeclared, got.Undeclared, c.name)

		after, err := s.GetTemplateVersion(ctx, v.VersionID)
		require.NoError(t, err)
		assert.Equal(t, domain.TemplateVersionDraft, after.Status, "%s: a refused validation must leave the version DRAFT", c.name)
	}

	// Fully declared (and unused declared variables) still validate.
	ok, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>{{.name}}</p>", VariableSchema: []string{"name", "spare"}, CreatedByPrincipalID: "owner-1",
	})
	require.NoError(t, err)
	_, err = s.ValidateTemplate(ctx, ok.VersionID)
	require.NoError(t, err)
}

// BIZ-03 tenant isolation, as the NOSUPERUSER NOBYPASSRLS app role: tenant B cannot read or
// change tenant A's templates or versions, and a raw count under B's tenant context sees none.
func TestPgStore_Templates_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("local"))
	a, b := tenantCtx("tenant-biz03-a"), tenantCtx("tenant-biz03-b")

	tmpl := newTestTemplate(t, s, a, "owner-a")
	mk := func(content string) *domain.TemplateVersion {
		v, err := s.CreateVersion(a, domain.CreateVersionParams{
			TemplateID: tmpl.TemplateID, Locale: "en-US", Content: content, VariableSchema: []string{"name"}, CreatedByPrincipalID: "owner-a",
		})
		require.NoError(t, err)
		return v
	}
	v1, v2 := mk("<p>one {{.name}}</p>"), mk("<p>two {{.name}}</p>")
	_, err := s.ValidateTemplate(a, v1.VersionID)
	require.NoError(t, err)
	_, err = s.ApproveTemplate(a, domain.ApproveVersionParams{VersionID: v1.VersionID, ApprovedByPrincipalID: "approver-a"})
	require.NoError(t, err)
	_, err = s.PublishTemplate(a, domain.PublishVersionParams{VersionID: v1.VersionID, PublishedByPrincipalID: "approver-a"})
	require.NoError(t, err)

	_, err = s.GetTemplate(b, tmpl.TemplateID)
	assert.ErrorIs(t, err, domain.ErrTemplateNotFound)
	_, err = s.GetTemplateVersion(b, v1.VersionID)
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.GetPublishedVersion(b, tmpl.TemplateID, "en-US")
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.ListLocales(b, tmpl.TemplateID)
	assert.ErrorIs(t, err, domain.ErrTemplateNotFound)
	_, err = s.CompareVersions(b, v1.VersionID, v2.VersionID)
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.RenderPreview(b, domain.RenderPreviewParams{VersionID: v1.VersionID, Placeholders: true})
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)

	_, err = s.CreateVersion(b, domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>x</p>", CreatedByPrincipalID: "owner-b"})
	assert.ErrorIs(t, err, domain.ErrTemplateNotFound)
	_, err = s.ValidateTemplate(b, v2.VersionID)
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.ApproveTemplate(b, domain.ApproveVersionParams{VersionID: v2.VersionID, ApprovedByPrincipalID: "approver-b"})
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.PublishTemplate(b, domain.PublishVersionParams{VersionID: v1.VersionID, PublishedByPrincipalID: "approver-b"})
	assert.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
	_, err = s.RetireTemplate(b, domain.RetireTemplateParams{TemplateID: tmpl.TemplateID, RetiredByPrincipalID: "owner-b"})
	assert.ErrorIs(t, err, domain.ErrTemplateNotFound)

	// A's data is unchanged: still ACTIVE, v1 still the published version, v2 still DRAFT.
	got, err := s.GetTemplate(a, tmpl.TemplateID)
	require.NoError(t, err)
	assert.Equal(t, "ACTIVE", got.Status)
	pub, err := s.GetPublishedVersion(a, tmpl.TemplateID, "en-US")
	require.NoError(t, err)
	assert.Equal(t, v1.VersionID, pub.VersionID)
	d2, err := s.GetTemplateVersion(a, v2.VersionID)
	require.NoError(t, err)
	assert.Equal(t, domain.TemplateVersionDraft, d2.Status)

	// Raw probe: with B's tenant context and no tenant predicate, the database itself shows B nothing.
	bg := context.Background()
	tx, err := pool.Begin(bg)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(bg) }()
	_, err = tx.Exec(bg, "SELECT set_config('app.tenant_id', 'tenant-biz03-b', true)")
	require.NoError(t, err)
	for _, table := range []string{"template_definitions", "template_versions"} {
		var n int
		require.NoError(t, tx.QueryRow(bg, "SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Zero(t, n, table)
	}
}
