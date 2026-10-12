package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func subjectVersion(t *testing.T, s *store.PgStore, tenant, subject string, subjectVars []string) *domain.TemplateVersion {
	t.Helper()
	ctx := tenantCtx(tenant)
	tmpl := newTestTemplate(t, s, ctx, "owner-subj")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>Hi {{.first_name}}, net pay {{.net_pay}} for {{.period}}</p>",
		VariableSchema: []string{"first_name", "net_pay", "period"}, Subject: subject, SubjectVariables: subjectVars,
		CreatedByPrincipalID: "owner-subj",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	return v
}

// F-05: the subject is stored with the version and comes back with it.
func TestTemplateSubject_IsStoredAndRendered(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	ctx := tenantCtx("tenant-subj")
	v := subjectVersion(t, s, "tenant-subj", "Payslip for {{.period}}", []string{"period"})
	if v.Subject == nil || *v.Subject != "Payslip for {{.period}}" || len(v.SubjectVariables) != 1 {
		t.Fatalf("stored subject = %v %v", v.Subject, v.SubjectVariables)
	}
	out, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID,
		Variables: map[string]string{"first_name": "Asha", "net_pay": "4,210.55", "period": "October 2026"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.RenderedSubject != "Payslip for October 2026" || strings.Contains(out.RenderedSubject, "4,210.55") {
		t.Errorf("rendered subject = %q", out.RenderedSubject)
	}
	// A value that would break the header is refused rather than cleaned (NP-08).
	_, err = s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID,
		Variables: map[string]string{"first_name": "Asha", "net_pay": "1", "period": "Oct\r\nBcc: x@example.com"}})
	if !errors.Is(err, domain.ErrSubjectInvalid) {
		t.Errorf("want ErrSubjectInvalid, got %v", err)
	}
}

// The subject is frozen with the body: no later UPDATE can change what was reviewed.
func TestTemplateSubject_IsFrozenByTheDatabase(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	v := subjectVersion(t, s, "tenant-subj", "Payslip for {{.period}}", []string{"period"})
	bg := context.Background()

	for name, set := range map[string]string{
		"change the subject":          `subject = 'Net pay {{.net_pay}}'`,
		"remove the subject":          `subject = NULL, subject_variables = '[]'::jsonb`,
		"widen the subject-safe list": `subject_variables = '["period","net_pay"]'::jsonb`,
	} {
		if _, err := pool.Exec(bg, `UPDATE template_versions SET `+set+` WHERE version_id=$1`, v.VersionID); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
}

// The database enforces the shape without the store.
func TestTemplateSubject_DatabaseRefusesBadShapes(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	tmpl := newTestTemplate(t, s, tenantCtx("tenant-subj"), "owner-subj")
	bg := context.Background()

	insert := func(subject, vars string) error {
		_, err := pool.Exec(bg, `INSERT INTO template_versions
			(template_id, tenant_id, legal_entity_id, version_number, locale, content, content_hash, variable_schema, subject, subject_variables, created_by_principal_id)
			VALUES ($1,'tenant-subj','le-us',(SELECT COALESCE(MAX(version_number),0)+1 FROM template_versions WHERE template_id=$1 AND locale='en-US'),'en-US','<p>x</p>','h','["period"]'::jsonb,
			        NULLIF($2,''), $3::jsonb, 'owner-subj')`, tmpl.TemplateID, subject, vars)
		return err
	}
	if err := insert("Payslip for {{.period}}", `["period"]`); err != nil {
		t.Fatalf("a valid subject must be accepted: %v", err)
	}
	for name, c := range map[string][2]string{
		"subject with a newline":              {"line one\nline two", `[]`},
		"subject-safe name not in the schema": {"Hello", `["ghost"]`},
		"subject-safe list without a subject": {"", `["period"]`},
		"subject too long":                    {strings.Repeat("a", 201), `[]`},
	} {
		if err := insert(c[0], c[1]); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
}

// An existing version has no subject and keeps working unchanged.
func TestTemplateSubject_LegacyVersionIsUnaffected(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	ctx := tenantCtx("tenant-subj")
	v := subjectVersion(t, s, "tenant-subj", "", nil)
	if v.Subject != nil || len(v.SubjectVariables) != 0 {
		t.Fatalf("a version created without a subject has none: %v %v", v.Subject, v.SubjectVariables)
	}
	out, err := s.RenderPreview(ctx, domain.RenderPreviewParams{VersionID: v.VersionID,
		Variables: map[string]string{"first_name": "A", "net_pay": "1", "period": "P"}})
	if err != nil || out.RenderedSubject != "" {
		t.Fatalf("legacy render: %v %q", err, out.RenderedSubject)
	}
}

// Creating a version with an unsafe subject is refused before anything is stored,
// and validation refuses one that slipped past (defence in depth).
func TestTemplateSubject_StoreRefusesUnsafeSubjects(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	ctx := tenantCtx("tenant-subj")
	tmpl := newTestTemplate(t, s, ctx, "owner-subj")
	for name, p := range map[string]domain.CreateVersionParams{
		"uses a variable not listed as subject-safe": {Subject: "Net {{.net_pay}}", SubjectVariables: []string{"period"}},
		"logic in the subject":                       {Subject: "{{if .period}}x{{end}}", SubjectVariables: []string{"period"}},
		"subject-safe variables without a subject":   {SubjectVariables: []string{"period"}},
	} {
		p.TemplateID, p.Locale, p.Content, p.CreatedByPrincipalID = tmpl.TemplateID, "en-US", "<p>{{.net_pay}}{{.period}}</p>", "owner-subj"
		p.VariableSchema = []string{"net_pay", "period"}
		if _, err := s.CreateVersion(ctx, p); !errors.Is(err, domain.ErrSubjectInvalid) {
			t.Errorf("%s: want ErrSubjectInvalid, got %v", name, err)
		}
	}
}

func TestMigration000018_DownThenUp(t *testing.T) {
	pool := openAdminTestPool(t)
	for _, f := range []string{"000018_template_version_subject.down.sql", "000018_template_version_subject.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='template_versions' AND column_name IN ('subject','subject_variables')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("subject columns: n=%d err=%v", n, err)
	}
}
