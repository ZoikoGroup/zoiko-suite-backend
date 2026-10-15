package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// version creates a version and takes it through the store to the wanted stage:
// "DRAFT", "REVIEW", "APPROVED" or "PUBLISHED".
func version(t *testing.T, s *store.PgStore, ctx context.Context, stage string) *domain.TemplateVersion {
	t.Helper()
	tmpl := newTestTemplate(t, s, ctx, "owner-edges")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>hi {{.name}}</p>",
		VariableSchema: []string{"name"}, CreatedByPrincipalID: "owner-edges",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	steps := []struct {
		stage string
		do    func() error
	}{
		{"REVIEW", func() error { _, e := s.ValidateTemplate(ctx, v.VersionID); return e }},
		{"APPROVED", func() error {
			_, e := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "approver-edges"})
			return e
		}},
		{"PUBLISHED", func() error {
			_, e := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "approver-edges"})
			return e
		}},
	}
	for _, st := range steps {
		if stage == "DRAFT" {
			break
		}
		if err := st.do(); err != nil {
			t.Fatalf("advance to %s: %v", st.stage, err)
		}
		if st.stage == stage {
			break
		}
	}
	return v
}

// F-08 / TC-02: the database refuses an illegal status change even when the SQL
// supplies all the evidence the legal change would, so only the EDGE is at issue.
func TestTemplateVersion_DatabaseRefusesIllegalStatusEdges(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-edges")
	bg := context.Background()

	type attack struct {
		name, stage, set string
	}
	full := `, validated_at=now(), approved_by_principal_id='someone-else', approved_at=now(), published_at=now(), superseded_by_version_id='` + uuid.NewString() + `', retired_at=now()`
	attacks := []attack{
		{"DRAFT straight to PUBLISHED", "DRAFT", "status='PUBLISHED'" + full},
		{"DRAFT straight to APPROVED", "DRAFT", "status='APPROVED'" + full},
		{"REVIEW straight to PUBLISHED", "REVIEW", "status='PUBLISHED'" + full},
		{"REVIEW back to DRAFT (no reject path yet)", "REVIEW", "status='DRAFT'"},
		{"APPROVED back to REVIEW", "APPROVED", "status='REVIEW'"},
		{"APPROVED straight to RETIRED", "APPROVED", "status='RETIRED', retired_at=now()"},
		{"PUBLISHED back to APPROVED", "PUBLISHED", "status='APPROVED'"},
		{"PUBLISHED back to DRAFT", "PUBLISHED", "status='DRAFT'"},
	}
	for _, a := range attacks {
		v := version(t, s, ctx, a.stage)
		_, err := pool.Exec(bg, `UPDATE template_versions SET `+a.set+` WHERE version_id=$1`, v.VersionID)
		if err == nil {
			t.Errorf("%s: the database accepted it", a.name)
		}
	}

	// A version cannot be created already published.
	tmpl := newTestTemplate(t, s, ctx, "owner-edges")
	_, err := pool.Exec(bg, `INSERT INTO template_versions (version_id, template_id, tenant_id, legal_entity_id, version_number, locale, content, content_hash, variable_schema, status, created_by_principal_id)
		VALUES ($1,$2,'tenant-edges','le-us',1,'en-US','<p>x</p>','h',ARRAY[]::text[],'PUBLISHED','x')`, uuid.NewString(), tmpl.TemplateID)
	if err == nil {
		t.Error("a version was inserted as PUBLISHED")
	}

	// Each legal edge still needs the evidence that goes with it.
	d := version(t, s, ctx, "DRAFT")
	if _, err := pool.Exec(bg, `UPDATE template_versions SET status='REVIEW' WHERE version_id=$1`, d.VersionID); err == nil {
		t.Error("DRAFT to REVIEW without validated_at was accepted")
	}
}

// The edges the store uses all still work, including supersession and retirement.
func TestTemplateVersion_LegalLifecycleStillWorks(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-edges-ok")

	v1 := version(t, s, ctx, "PUBLISHED")
	// A second version of the same template/locale supersedes the first.
	v2, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: v1.TemplateID, Locale: "en-US", Content: "<p>v2 {{.name}}</p>", VariableSchema: []string{"name"}, CreatedByPrincipalID: "owner-edges",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateTemplate(ctx, v2.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v2.VersionID, ApprovedByPrincipalID: "approver-edges"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v2.VersionID, PublishedByPrincipalID: "approver-edges"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplateVersion(ctx, v1.VersionID)
	if err != nil || got.Status != domain.TemplateVersionSuperseded {
		t.Fatalf("the first version should be SUPERSEDED, got %v %v", got, err)
	}
	if _, err := s.RetireTemplate(ctx, domain.RetireTemplateParams{TemplateID: v1.TemplateID, RetiredByPrincipalID: "owner-edges"}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got, _ := s.GetTemplateVersion(ctx, v2.VersionID); got.Status != domain.TemplateVersionRetired {
		t.Fatalf("the published version should be RETIRED, got %s", got.Status)
	}
}

// F-07 / NP-07: a variable the version does not declare is refused, not ignored.
func TestRenderPreview_RefusesUndeclaredVariables(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-edges-vars")
	v := version(t, s, ctx, "PUBLISHED")

	_, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID, Variables: map[string]string{"name": "Asha", "extra": "x", "also": "y"}})
	var unexpected domain.ErrTemplateVariablesUnexpected
	if !errors.As(err, &unexpected) {
		t.Fatalf("want ErrTemplateVariablesUnexpected, got %v", err)
	}
	if strings.Join(unexpected.Unexpected, ",") != "also,extra" {
		t.Errorf("should name every undeclared variable, sorted: %v", unexpected.Unexpected)
	}
	if out, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID, Variables: map[string]string{"name": "Asha"}}); err != nil || !strings.Contains(out.RenderedContent, "Asha") {
		t.Fatalf("declared variables must still render: %v", err)
	}
}

func TestMigration000017_DownThenUp(t *testing.T) {
	pool := openAdminTestPool(t)
	for _, f := range []string{"000017_template_version_status_edges.down.sql", "000017_template_version_status_edges.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM pg_trigger WHERE tgname='trg_template_version_status_edges'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("trigger present: n=%d err=%v", n, err)
	}
}
