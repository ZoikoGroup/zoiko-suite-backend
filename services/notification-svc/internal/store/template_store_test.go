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
