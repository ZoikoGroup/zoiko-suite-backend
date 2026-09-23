package store_test

import (
	"context"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func newTestTemplate(t *testing.T, s *store.PgStore, ctx context.Context, owner string) *domain.TemplateDefinition {
	t.Helper()
	tmpl, err := s.CreateTemplate(ctx, domain.CreateTemplateParams{
		LegalEntityID: "le-us", Name: "Password Reset", BusinessPurpose: "Notify a reset", OwnerPrincipalID: owner,
	})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	return tmpl
}

// TestPgStore_CreateVersion_RejectsRetiredTemplate proves CreateVersion
// refuses new content once a template is RETIRED — the real DB-backed
// proof, not just the handler-level stub check.
func TestPgStore_CreateVersion_RejectsRetiredTemplate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	if _, err := s.RetireTemplate(ctx, domain.RetireTemplateParams{TemplateID: tmpl.TemplateID, RetiredByPrincipalID: "owner-1"}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	_, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>hi</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != domain.ErrTemplateRetired {
		t.Fatalf("expected ErrTemplateRetired, got %v", err)
	}
}

// TestPgStore_ApproveTemplate_RejectsSelfApproval is the
// negative-controlled proof of the maker-checker requirement — every
// version, not only ones flagged externally binding (Wave 1 decision).
func TestPgStore_ApproveTemplate_RejectsSelfApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>hi {{.name}}</p>",
		VariableSchema: []string{"name"}, CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if _, err := s.ValidateTemplate(ctx, v.VersionID); err != nil {
		t.Fatalf("validate: %v", err)
	}

	_, err = s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "owner-1"})
	if err != domain.ErrTemplateVersionSelfApproval {
		t.Fatalf("expected ErrTemplateVersionSelfApproval, got %v", err)
	}

	// Negative control at the DB layer — the CHECK constraint refuses the
	// same self-approval even bypassing the store entirely.
	_, err = pool.Exec(context.Background(),
		`UPDATE template_versions SET status='APPROVED', approved_by_principal_id=$1, approved_at=now() WHERE version_id=$2`,
		"owner-1", v.VersionID)
	if err == nil {
		t.Fatal("expected the CHECK constraint to refuse a self-approval written directly")
	}
}

// TestPgStore_PublishTemplate_SupersedesPriorPublished_SameLocale proves
// the forward-link supersession mechanism, plus the negative control
// that a superseded/terminal row can never be changed again.
func TestPgStore_PublishTemplate_SupersedesPriorPublished_SameLocale(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	v1, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v1</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := s.ValidateTemplate(ctx, v1.VersionID); err != nil {
		t.Fatalf("validate v1: %v", err)
	}
	if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v1.VersionID, ApprovedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("approve v1: %v", err)
	}
	published1, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v1.VersionID, PublishedByPrincipalID: "approver-1"})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	v2, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v2</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if _, err := s.ValidateTemplate(ctx, v2.VersionID); err != nil {
		t.Fatalf("validate v2: %v", err)
	}
	if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v2.VersionID, ApprovedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("approve v2: %v", err)
	}
	if _, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v2.VersionID, PublishedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	current, err := s.GetPublishedVersion(ctx, tmpl.TemplateID, "en-US")
	if err != nil {
		t.Fatalf("get published: %v", err)
	}
	if current.VersionID != v2.VersionID {
		t.Fatalf("expected v2 to be current, got %s", current.VersionID)
	}

	superseded, err := s.GetTemplateVersion(ctx, published1.VersionID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if superseded.Status != domain.TemplateVersionSuperseded {
		t.Fatalf("expected v1 SUPERSEDED, got %s", superseded.Status)
	}
	if superseded.SupersededByVersionID == nil || *superseded.SupersededByVersionID != v2.VersionID {
		t.Fatalf("expected v1.superseded_by_version_id == v2, got %v", superseded.SupersededByVersionID)
	}

	// Negative control: the trigger refuses any further change to a
	// terminal (SUPERSEDED) row, including re-pointing the forward link.
	_, err = pool.Exec(context.Background(),
		`UPDATE template_versions SET superseded_by_version_id = $1 WHERE version_id = $2`,
		v1.VersionID, published1.VersionID)
	if err == nil {
		t.Fatal("expected the trigger to refuse mutating a SUPERSEDED row")
	}
}

// TestPgStore_RetireTemplate_RetiresPublishedVersionsAcrossLocales
// proves RetireTemplate's cross-locale reach: every PUBLISHED version of
// the template moves to RETIRED, not just one locale.
func TestPgStore_RetireTemplate_RetiresPublishedVersionsAcrossLocales(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	var published []*domain.TemplateVersion
	for _, locale := range []string{"en-US", "fr-FR"} {
		v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
			TemplateID: tmpl.TemplateID, Locale: locale, Content: "<p>hi</p>", CreatedByPrincipalID: "owner-1",
		})
		if err != nil {
			t.Fatalf("create version %s: %v", locale, err)
		}
		if _, err := s.ValidateTemplate(ctx, v.VersionID); err != nil {
			t.Fatalf("validate %s: %v", locale, err)
		}
		if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "approver-1"}); err != nil {
			t.Fatalf("approve %s: %v", locale, err)
		}
		pv, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "approver-1"})
		if err != nil {
			t.Fatalf("publish %s: %v", locale, err)
		}
		published = append(published, pv)
	}

	if _, err := s.RetireTemplate(ctx, domain.RetireTemplateParams{TemplateID: tmpl.TemplateID, RetiredByPrincipalID: "owner-1"}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	for _, pv := range published {
		got, err := s.GetTemplateVersion(ctx, pv.VersionID)
		if err != nil {
			t.Fatalf("get version: %v", err)
		}
		if got.Status != domain.TemplateVersionRetired {
			t.Fatalf("expected %s RETIRED after template retire, got %s", got.Locale, got.Status)
		}
	}

	// Negative control: retiring the same template twice is refused.
	if _, err := s.RetireTemplate(ctx, domain.RetireTemplateParams{TemplateID: tmpl.TemplateID, RetiredByPrincipalID: "owner-1"}); err != domain.ErrTemplateAlreadyRetired {
		t.Fatalf("expected ErrTemplateAlreadyRetired, got %v", err)
	}
}

// TestPgStore_ValidateTemplate_RejectsMalformedContent proves the
// html/template parse gate actually refuses broken markup rather than
// letting it reach REVIEW.
func TestPgStore_ValidateTemplate_RejectsMalformedContent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>{{.broken</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	if _, err := s.ValidateTemplate(ctx, v.VersionID); err != domain.ErrTemplateContentInvalid {
		t.Fatalf("expected ErrTemplateContentInvalid, got %v", err)
	}
}

// TestPgStore_RenderPreview_RefusesPartialRender proves RenderPreview
// refuses to render a version whose required variables are not all
// supplied, and renders correctly once they are.
func TestPgStore_RenderPreview_RefusesPartialRender(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>Hello {{.first_name}}</p>",
		VariableSchema: []string{"first_name"}, CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	if _, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID, Variables: map[string]string{}}); err == nil {
		t.Fatal("expected an error for missing required variables")
	}

	result, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID, Variables: map[string]string{"first_name": "Ada"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if result.RenderedContent != "<p>Hello Ada</p>" {
		t.Fatalf("expected rendered content with substituted variable, got %q", result.RenderedContent)
	}
}

// TestPgStore_CompareVersions_RejectsDifferentTemplates is the
// negative-controlled proof that comparing versions across two
// different templates is refused rather than silently diffed.
func TestPgStore_CompareVersions_RejectsDifferentTemplates(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmplA := newTestTemplate(t, s, ctx, "owner-1")
	vA, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmplA.TemplateID, Locale: "en-US", Content: "<p>a</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version a: %v", err)
	}
	tmplB := newTestTemplate(t, s, ctx, "owner-1")
	vB, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmplB.TemplateID, Locale: "en-US", Content: "<p>b</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version b: %v", err)
	}

	if _, err := s.CompareVersions(ctx, vA.VersionID, vB.VersionID); err != domain.ErrTemplateVersionsBelongToDifferentTemplates {
		t.Fatalf("expected ErrTemplateVersionsBelongToDifferentTemplates, got %v", err)
	}
}

// TestPgStore_CompareVersions_ReportsVariableAndContentDiff proves the
// real diff: content_changed reflects the content hash, and
// added/removed reflect the actual variable schema delta.
func TestPgStore_CompareVersions_ReportsVariableAndContentDiff(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")
	v1, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v1 {{.a}}</p>",
		VariableSchema: []string{"a"}, CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	v2, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v2 {{.b}}</p>",
		VariableSchema: []string{"b"}, CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}

	result, err := s.CompareVersions(ctx, v1.VersionID, v2.VersionID)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !result.ContentChanged {
		t.Fatal("expected content_changed to be true")
	}
	if len(result.VariablesAdded) != 1 || result.VariablesAdded[0] != "b" {
		t.Fatalf("expected variables_added = [b], got %v", result.VariablesAdded)
	}
	if len(result.VariablesRemoved) != 1 || result.VariablesRemoved[0] != "a" {
		t.Fatalf("expected variables_removed = [a], got %v", result.VariablesRemoved)
	}
}

// TestPgStore_ListLocales_ReportsLatestAndPublishedPerLocale proves
// ListLocales correctly separates "latest version" from "currently
// published version" for each locale — they can differ once a locale
// has an unpublished draft on top of an older published version.
func TestPgStore_ListLocales_ReportsLatestAndPublishedPerLocale(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	published, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v1</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := s.ValidateTemplate(ctx, published.VersionID); err != nil {
		t.Fatalf("validate v1: %v", err)
	}
	if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: published.VersionID, ApprovedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("approve v1: %v", err)
	}
	if _, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: published.VersionID, PublishedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	draft, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>v2 draft</p>", CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create v2 draft: %v", err)
	}
	if _, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "fr-FR", Content: "<p>bonjour</p>", CreatedByPrincipalID: "owner-1",
	}); err != nil {
		t.Fatalf("create fr-FR: %v", err)
	}

	locales, err := s.ListLocales(ctx, tmpl.TemplateID)
	if err != nil {
		t.Fatalf("list locales: %v", err)
	}
	if len(locales) != 2 {
		t.Fatalf("expected 2 locales, got %d: %+v", len(locales), locales)
	}
	byLocale := map[string]domain.LocaleSummary{}
	for _, ls := range locales {
		byLocale[ls.Locale] = ls
	}
	en := byLocale["en-US"]
	if en.LatestVersionID != draft.VersionID {
		t.Fatalf("expected en-US latest to be the draft, got %s", en.LatestVersionID)
	}
	if en.PublishedVersionID == nil || *en.PublishedVersionID != published.VersionID {
		t.Fatalf("expected en-US published to be the first version, got %v", en.PublishedVersionID)
	}
	fr := byLocale["fr-FR"]
	if fr.PublishedVersionID != nil {
		t.Fatalf("expected fr-FR to have no published version yet, got %v", *fr.PublishedVersionID)
	}
}

func TestPgStore_ListLocales_UnknownTemplate_ReturnsNotFound(t *testing.T) {
	s := store.New(openTestPool(t))
	_, err := s.ListLocales(tenantCtx("tenant-a"), "00000000-0000-0000-0000-000000000000")
	if err != domain.ErrTemplateNotFound {
		t.Fatalf("expected ErrTemplateNotFound, got %v", err)
	}
}
