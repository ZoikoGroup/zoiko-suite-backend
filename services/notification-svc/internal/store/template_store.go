// BIZ-03 Template persistence — see internal/domain/template.go for the
// lifecycle/immutability doc comment and migration 000005 for the
// schema/trigger this operates against.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

const templateDefinitionColumns = `
	template_id, tenant_id, legal_entity_id, name, business_purpose, owner_principal_id,
	status, created_at, retired_at, retired_by_principal_id
`

const templateVersionColumns = `
	version_id, template_id, tenant_id, legal_entity_id, version_number, locale,
	content, content_hash, variable_schema, branding_metadata, accessibility_metadata,
	status, created_by_principal_id, created_at, validated_at,
	approved_by_principal_id, approved_at, published_at, retired_at, superseded_by_version_id
`

func scanTemplateDefinition(s scannable, d *domain.TemplateDefinition) error {
	return s.Scan(&d.TemplateID, &d.TenantID, &d.LegalEntityID, &d.Name, &d.BusinessPurpose, &d.OwnerPrincipalID,
		&d.Status, &d.CreatedAt, &d.RetiredAt, &d.RetiredByPrincipalID)
}

func scanTemplateVersion(s scannable, v *domain.TemplateVersion) error {
	var schemaRaw []byte
	if err := s.Scan(&v.VersionID, &v.TemplateID, &v.TenantID, &v.LegalEntityID, &v.VersionNumber, &v.Locale,
		&v.Content, &v.ContentHash, &schemaRaw, &v.BrandingMetadata, &v.AccessibilityMetadata,
		&v.Status, &v.CreatedByPrincipalID, &v.CreatedAt, &v.ValidatedAt,
		&v.ApprovedByPrincipalID, &v.ApprovedAt, &v.PublishedAt, &v.RetiredAt, &v.SupersededByVersionID,
	); err != nil {
		return err
	}
	if len(schemaRaw) > 0 {
		if err := json.Unmarshal(schemaRaw, &v.VariableSchema); err != nil {
			return fmt.Errorf("decode variable_schema: %w", err)
		}
	}
	return nil
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// CreateTemplate creates a new template definition — BIZ-03's own
// CreateTemplate command. It does not create a version; CreateVersion
// does that separately, since a definition can exist with zero versions
// while its first draft is being authored.
func (s *PgStore) CreateTemplate(ctx context.Context, p domain.CreateTemplateParams) (*domain.TemplateDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO template_definitions (tenant_id, legal_entity_id, name, business_purpose, owner_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+templateDefinitionColumns,
			tenantID, p.LegalEntityID, p.Name, p.BusinessPurpose, p.OwnerPrincipalID,
		)
		return scanTemplateDefinition(row, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("template store unavailable: %w", err)
	}
	return &out, nil
}

// GetTemplate looks up a template definition, scoped to the caller's
// tenant.
func (s *PgStore) GetTemplate(ctx context.Context, templateID string) (*domain.TemplateDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+templateDefinitionColumns+` FROM template_definitions
			WHERE template_id = $1 AND tenant_id = $2`, templateID, tenantID)
		return scanTemplateDefinition(row, &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &out, nil
}

// CreateVersion inserts a new DRAFT version for an ACTIVE template
// definition — BIZ-03's own CreateVersion command. version_number is the
// next number for that (template_id, locale) pair, so each locale's own
// versions are independently numbered starting at 1.
func (s *PgStore) CreateVersion(ctx context.Context, p domain.CreateVersionParams) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if p.Locale == "" {
		return nil, domain.ErrTemplateLocaleRequired
	}
	schemaJSON, err := json.Marshal(p.VariableSchema)
	if err != nil {
		return nil, fmt.Errorf("encode variable_schema: %w", err)
	}

	var out domain.TemplateVersion
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var legalEntityID, status string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id, status FROM template_definitions
			WHERE template_id = $1 AND tenant_id = $2`, p.TemplateID, tenantID).Scan(&legalEntityID, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateNotFound
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		if status == "RETIRED" {
			return domain.ErrTemplateRetired
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM template_versions
			WHERE template_id = $1 AND locale = $2`, p.TemplateID, p.Locale).Scan(&nextVersion); err != nil {
			return fmt.Errorf("template store unavailable: %w", err)
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO template_versions (
				template_id, tenant_id, legal_entity_id, version_number, locale,
				content, content_hash, variable_schema, branding_metadata, accessibility_metadata,
				created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)
			RETURNING `+templateVersionColumns,
			p.TemplateID, tenantID, legalEntityID, nextVersion, p.Locale,
			p.Content, contentHash(p.Content), schemaJSON, nullIfEmpty(p.BrandingMetadata), nullIfEmpty(p.AccessibilityMetadata),
			p.CreatedByPrincipalID,
		)
		return scanTemplateVersion(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTemplateVersion is a read-only lookup used to authorize
// ApproveTemplate/PublishTemplate BEFORE any mutation runs — same
// fetch-then-authorize-then-mutate discipline as every other handler in
// this platform.
func (s *PgStore) GetTemplateVersion(ctx context.Context, versionID string) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+templateVersionColumns+` FROM template_versions
			WHERE version_id = $1 AND tenant_id = $2`, versionID, tenantID)
		return scanTemplateVersion(row, &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateVersionNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &out, nil
}

// ValidateTemplate checks a DRAFT version's content actually parses as
// html/template and that a variable schema is present, then moves it to
// REVIEW — BIZ-03's own ValidateTemplate command, the gate before
// approval can even be attempted.
func (s *PgStore) ValidateTemplate(ctx context.Context, versionID string) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var existing domain.TemplateVersion
		row := tx.QueryRow(ctx, `SELECT `+templateVersionColumns+` FROM template_versions
			WHERE version_id = $1 AND tenant_id = $2`, versionID, tenantID)
		if err := scanTemplateVersion(row, &existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotFound
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		if existing.Status != domain.TemplateVersionDraft {
			return domain.ErrTemplateVersionNotDraft
		}
		if err := validateTemplateContent(existing.Content); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, `
			UPDATE template_versions SET status = 'REVIEW', validated_at = now()
			WHERE version_id = $1 AND tenant_id = $2 AND status = 'DRAFT'
			RETURNING `+templateVersionColumns,
			versionID, tenantID,
		)
		if err := scanTemplateVersion(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotDraft
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproveTemplate moves a REVIEW version to APPROVED — BIZ-03's own
// ApproveTemplate command. Maker-checker is enforced for every version,
// not only ones flagged externally binding: the approving principal can
// never be the one who created it (also a DB CHECK constraint,
// migration 000005).
func (s *PgStore) ApproveTemplate(ctx context.Context, p domain.ApproveVersionParams) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var existing domain.TemplateVersion
		row := tx.QueryRow(ctx, `SELECT `+templateVersionColumns+` FROM template_versions
			WHERE version_id = $1 AND tenant_id = $2`, p.VersionID, tenantID)
		if err := scanTemplateVersion(row, &existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotFound
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		if existing.CreatedByPrincipalID == p.ApprovedByPrincipalID {
			return domain.ErrTemplateVersionSelfApproval
		}
		if existing.Status != domain.TemplateVersionReview {
			return domain.ErrTemplateVersionNotReview
		}

		row = tx.QueryRow(ctx, `
			UPDATE template_versions SET status = 'APPROVED', approved_by_principal_id = $3, approved_at = now()
			WHERE version_id = $1 AND tenant_id = $2 AND status = 'REVIEW'
			RETURNING `+templateVersionColumns,
			p.VersionID, tenantID, p.ApprovedByPrincipalID,
		)
		if err := scanTemplateVersion(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotReview
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PublishTemplate moves an APPROVED version to PUBLISHED — BIZ-03's own
// PublishTemplate command — and, in the same transaction, supersedes
// whichever version was previously PUBLISHED for that same
// (template_id, locale), forward-linking it via superseded_by_version_id
// exactly once. Mirrors document-vault-svc's SupersedeClassification.
func (s *PgStore) PublishTemplate(ctx context.Context, p domain.PublishVersionParams) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var existing domain.TemplateVersion
		row := tx.QueryRow(ctx, `SELECT `+templateVersionColumns+` FROM template_versions
			WHERE version_id = $1 AND tenant_id = $2`, p.VersionID, tenantID)
		if err := scanTemplateVersion(row, &existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotFound
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}
		if existing.Status != domain.TemplateVersionApproved {
			return domain.ErrTemplateVersionNotApproved
		}

		row = tx.QueryRow(ctx, `
			UPDATE template_versions SET status = 'PUBLISHED', published_at = now()
			WHERE version_id = $1 AND tenant_id = $2 AND status = 'APPROVED'
			RETURNING `+templateVersionColumns,
			p.VersionID, tenantID,
		)
		if err := scanTemplateVersion(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTemplateVersionNotApproved
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE template_versions SET status = 'SUPERSEDED', superseded_by_version_id = $4
			WHERE template_id = $1 AND locale = $2 AND status = 'PUBLISHED' AND version_id <> $3
		`, existing.TemplateID, existing.Locale, out.VersionID, out.VersionID); err != nil {
			return fmt.Errorf("template store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPublishedVersion returns the currently PUBLISHED version of a
// template for one locale — BIZ-03's own GetPublishedVersion query, and
// the one real consumers (BIZ-10 notification rendering) actually read.
func (s *PgStore) GetPublishedVersion(ctx context.Context, templateID, locale string) (*domain.TemplateVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+templateVersionColumns+` FROM template_versions
			WHERE template_id = $1 AND locale = $2 AND tenant_id = $3 AND status = 'PUBLISHED'`,
			templateID, locale, tenantID)
		return scanTemplateVersion(row, &out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateVersionNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &out, nil
}

// RetireTemplate retires the whole definition (blocking new versions)
// and, in the same transaction, retires every version of it still
// PUBLISHED across every locale — BIZ-03's own RetireTemplate command.
func (s *PgStore) RetireTemplate(ctx context.Context, p domain.RetireTemplateParams) (*domain.TemplateDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.TemplateDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE template_definitions SET status = 'RETIRED', retired_at = now(), retired_by_principal_id = $3
			WHERE template_id = $1 AND tenant_id = $2 AND status = 'ACTIVE'
			RETURNING `+templateDefinitionColumns,
			p.TemplateID, tenantID, p.RetiredByPrincipalID,
		)
		if err := scanTemplateDefinition(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if checkErr := tx.QueryRow(ctx, `SELECT true FROM template_definitions WHERE template_id = $1 AND tenant_id = $2`,
					p.TemplateID, tenantID).Scan(&exists); checkErr != nil {
					if errors.Is(checkErr, pgx.ErrNoRows) {
						return domain.ErrTemplateNotFound
					}
					return fmt.Errorf("template store unavailable: %w", checkErr)
				}
				return domain.ErrTemplateAlreadyRetired
			}
			return fmt.Errorf("template store unavailable: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE template_versions SET status = 'RETIRED', retired_at = now()
			WHERE template_id = $1 AND status = 'PUBLISHED'
		`, p.TemplateID); err != nil {
			return fmt.Errorf("template store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RenderPreview renders a version's content against supplied variables
// regardless of its status — BIZ-03's own RenderPreview query. Refuses a
// partial render rather than producing one with a blank field, same
// posture as internal/templates.Render.
func (s *PgStore) RenderPreview(ctx context.Context, p domain.RenderPreviewParams) (*domain.RenderPreviewResult, error) {
	v, err := s.GetTemplateVersion(ctx, p.VersionID)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, key := range v.VariableSchema {
		if p.Variables[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, domain.ErrTemplateVariablesMissing{VersionID: p.VersionID, Missing: missing}
	}

	tmpl, err := htmltemplate.New("preview").Parse(v.Content)
	if err != nil {
		return nil, domain.ErrTemplateContentInvalid
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p.Variables); err != nil {
		return nil, fmt.Errorf("render preview: %w", err)
	}
	return &domain.RenderPreviewResult{VersionID: p.VersionID, RenderedContent: buf.String()}, nil
}

// CompareVersions diffs two versions of the same template — BIZ-03's
// own CompareVersions query. Refuses to compare versions belonging to
// different templates; that is not a meaningful diff.
func (s *PgStore) CompareVersions(ctx context.Context, versionIDA, versionIDB string) (*domain.CompareVersionsResult, error) {
	a, err := s.GetTemplateVersion(ctx, versionIDA)
	if err != nil {
		return nil, err
	}
	b, err := s.GetTemplateVersion(ctx, versionIDB)
	if err != nil {
		return nil, err
	}
	if a.TemplateID != b.TemplateID {
		return nil, domain.ErrTemplateVersionsBelongToDifferentTemplates
	}

	aVars := make(map[string]bool, len(a.VariableSchema))
	for _, v := range a.VariableSchema {
		aVars[v] = true
	}
	bVars := make(map[string]bool, len(b.VariableSchema))
	for _, v := range b.VariableSchema {
		bVars[v] = true
	}
	var added, removed []string
	for _, v := range b.VariableSchema {
		if !aVars[v] {
			added = append(added, v)
		}
	}
	for _, v := range a.VariableSchema {
		if !bVars[v] {
			removed = append(removed, v)
		}
	}

	return &domain.CompareVersionsResult{
		VersionA: *a, VersionB: *b,
		ContentChanged:   a.ContentHash != b.ContentHash,
		VariablesAdded:   added,
		VariablesRemoved: removed,
	}, nil
}

// ListLocales returns every locale a template has versions in, along
// with each locale's latest version and — if any — which version
// currently governs it (PUBLISHED) — BIZ-03's own ListLocales query.
func (s *PgStore) ListLocales(ctx context.Context, templateID string) ([]domain.LocaleSummary, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.LocaleSummary
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := s.templateExists(ctx, tx, tenantID, templateID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (locale) locale, version_id, version_number, status
			FROM template_versions
			WHERE template_id = $1 AND tenant_id = $2
			ORDER BY locale, version_number DESC
		`, templateID, tenantID)
		if err != nil {
			return fmt.Errorf("template store unavailable: %w", err)
		}
		for rows.Next() {
			var ls domain.LocaleSummary
			if err := rows.Scan(&ls.Locale, &ls.LatestVersionID, &ls.LatestVersionNumber, &ls.LatestStatus); err != nil {
				rows.Close()
				return fmt.Errorf("template store unavailable: %w", err)
			}
			out = append(out, ls)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("template store unavailable: %w", rowsErr)
		}

		// A second query per locale, only after the first result set is
		// fully drained and closed — pgx refuses to interleave a new query
		// with an open one on the same connection ("conn busy").
		for i := range out {
			var publishedID string
			pubErr := tx.QueryRow(ctx, `
				SELECT version_id FROM template_versions
				WHERE template_id = $1 AND tenant_id = $2 AND locale = $3 AND status = 'PUBLISHED'
			`, templateID, tenantID, out[i].Locale).Scan(&publishedID)
			if pubErr == nil {
				out[i].PublishedVersionID = &publishedID
			} else if !errors.Is(pubErr, pgx.ErrNoRows) {
				return fmt.Errorf("template store unavailable: %w", pubErr)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// templateExists is a lightweight existence check used by queries that
// need to distinguish "template not found" from "template has no
// versions yet" (an empty ListLocales result is valid; a missing
// template is not).
func (s *PgStore) templateExists(ctx context.Context, tx pgx.Tx, tenantID, templateID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM template_definitions WHERE template_id = $1 AND tenant_id = $2`,
		templateID, tenantID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrTemplateNotFound
	}
	if err != nil {
		return false, fmt.Errorf("template store unavailable: %w", err)
	}
	return exists, nil
}

// validateTemplateContent proves the content is at least syntactically
// safe to render before it can ever reach REVIEW/APPROVED/PUBLISHED —
// the same html/template parser the real render path (internal/templates
// today; this version's own render path in a later wave) uses, so a
// version that validates here is guaranteed parseable there.
func validateTemplateContent(content string) error {
	if content == "" {
		return domain.ErrTemplateContentInvalid
	}
	if _, err := htmltemplate.New("validate").Parse(content); err != nil {
		return domain.ErrTemplateContentInvalid
	}
	return nil
}
