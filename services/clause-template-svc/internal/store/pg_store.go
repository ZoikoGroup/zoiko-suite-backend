// Package store provides the PostgreSQL implementation of
// clause-template-svc's persistence layer.
//
// Every method wraps its work in setRLS, which sets app.tenant_id on the
// transaction, but that was NOT sufficient on its own: this pool connects as
// a Postgres superuser (same DATABASE_URL pattern as every other service on
// this platform), and Postgres superusers unconditionally bypass Row Level
// Security no matter what policies exist — ENABLE ROW LEVEL SECURITY is also
// skipped for a table's owner unless FORCE ROW LEVEL SECURITY is set, and
// here the owner is postgres too. Unlike board-resolutions-svc and
// contract-lifecycle-svc, this store additionally carried NO explicit
// tenant_id predicate of its own in any query — GetClause, UpdateClause,
// GetTemplate and UpdateTemplate all read and wrote by id alone, with
// nothing scoping them to a tenant at all. Migration 000002 adds FORCE; the
// explicit predicates below are the belt to that policy's braces, same
// defence-in-depth already applied in both of those other services after
// the identical bug was found live in each.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/middleware"
)

type Store interface {
	CreateClause(ctx context.Context, c *domain.Clause) error
	GetClause(ctx context.Context, id string) (*domain.Clause, error)
	ListClauses(ctx context.Context, legalEntityID, category string) ([]domain.Clause, error)
	// UpdateClause is LEG-06's named CreateClauseVersion command: it may
	// only act while the clause is DRAFT or LEGAL_REVIEW.
	UpdateClause(ctx context.Context, c *domain.Clause, changeSummary string) error
	SubmitClauseForLegalReview(ctx context.Context, id, submittedBy string) (*domain.Clause, error)
	ApproveClause(ctx context.Context, id, approvedBy string) (*domain.Clause, error)
	ActivateClause(ctx context.Context, id, activatedBy string) (*domain.Clause, error)
	RetireClause(ctx context.Context, id, retiredBy string) (*domain.Clause, error)
	SupersedeClause(ctx context.Context, id, supersededBy string) (*domain.Clause, error)
	ListClauseVersions(ctx context.Context, clauseID string) ([]domain.ClauseVersion, error)

	CreateTemplate(ctx context.Context, t *domain.ContractTemplate) error
	GetTemplate(ctx context.Context, id string) (*domain.ContractTemplate, error)
	ListTemplates(ctx context.Context, legalEntityID, contractType string) ([]domain.ContractTemplate, error)
	UpdateTemplate(ctx context.Context, t *domain.ContractTemplate) error
	ApproveTemplate(ctx context.Context, id, approvedBy string) (*domain.ContractTemplate, error)

	CreateDeviationRule(ctx context.Context, d *domain.DeviationRule) error
	GetDeviationRule(ctx context.Context, id string) (*domain.DeviationRule, error)
	ListDeviationRules(ctx context.Context, legalEntityID, jurisdictionID, status string) ([]domain.DeviationRule, error)
	ApproveDeviationRule(ctx context.Context, id, approvedBy string) (*domain.DeviationRule, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// setRLS installs the caller's tenant for the transaction and refuses an
// empty one rather than defaulting it — see middleware/tenant.go's doc
// comment for why a missing tenant must fail, not pool into a shared bucket.
func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) (string, error) {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return "", domain.ErrTenantMissing
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}

func (s *PgStore) snapshotClauseVersion(ctx context.Context, tx pgx.Tx, c *domain.Clause, summary string) error {
	v := &domain.ClauseVersion{
		VersionID:      "clv-" + uuid.New().String(),
		ClauseID:       c.ClauseID,
		TenantID:       c.TenantID,
		VersionNumber:  c.Version,
		Status:         c.Status,
		Title:          c.Title,
		Body:           c.Body,
		JurisdictionID: c.JurisdictionID,
		EffectiveFrom:  c.EffectiveFrom,
		EffectiveTo:    c.EffectiveTo,
		ChangeSummary:  summary,
		CreatedBy:      c.CreatedBy,
		CreatedAt:      time.Now().UTC(),
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO clause_versions
			(version_id, clause_id, tenant_id, version_number, status, title, body,
			 jurisdiction_id, effective_from, effective_to, change_summary, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		v.VersionID, v.ClauseID, v.TenantID, v.VersionNumber, string(v.Status), v.Title, v.Body,
		v.JurisdictionID, v.EffectiveFrom, v.EffectiveTo, v.ChangeSummary, v.CreatedBy, v.CreatedAt,
	)
	return err
}

// effectiveDateColumns is the SELECT fragment for the two effective-date
// columns, which MUST be read as text. effective_from/effective_to are DATE
// columns, but domain.Clause declares them as string / *string because the
// API contract is a plain "YYYY-MM-DD", not an RFC3339 timestamp — same
// asymmetric read/write bug already fixed in contract-lifecycle-svc's and
// board-resolutions-svc's stores (pgx encodes a Go string into a DATE
// parameter on write, but cannot decode a DATE into a *string on a plain
// SELECT). TO_CHAR rather than ::TEXT so the format does not depend on the
// session's DateStyle.
const effectiveDateColumns = `TO_CHAR(effective_from, 'YYYY-MM-DD'), TO_CHAR(effective_to, 'YYYY-MM-DD')`

const clauseColumns = `clause_id, tenant_id, legal_entity_id, title, category, body, status, version,
	       jurisdiction_id, ` + effectiveDateColumns + `, authored_by_ai,
	       submitted_at, submitted_by, approved_at, approved_by, activated_at, activated_by,
	       retired_at, retired_by, superseded_by, created_by, created_at, updated_at`

func scanClause(row pgx.Row) (*domain.Clause, error) {
	var c domain.Clause
	var category, status string
	err := row.Scan(
		&c.ClauseID, &c.TenantID, &c.LegalEntityID, &c.Title, &category, &c.Body, &status, &c.Version,
		&c.JurisdictionID, &c.EffectiveFrom, &c.EffectiveTo, &c.AuthoredByAI,
		&c.SubmittedAt, &c.SubmittedBy, &c.ApprovedAt, &c.ApprovedBy, &c.ActivatedAt, &c.ActivatedBy,
		&c.RetiredAt, &c.RetiredBy, &c.SupersededBy, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrClauseNotFound
		}
		return nil, err
	}
	c.Category = domain.ClauseCategory(category)
	c.Status = domain.Status(status)
	return &c, nil
}

func (s *PgStore) CreateClause(ctx context.Context, c *domain.Clause) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if c.ClauseID == "" {
		c.ClauseID = "cls-" + uuid.New().String()
	}
	c.TenantID = tenantID
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = domain.StatusDraft
	}
	c.Version = 1

	_, err = tx.Exec(ctx, `
		INSERT INTO clauses
			(clause_id, tenant_id, legal_entity_id, title, category, body, status, version,
			 jurisdiction_id, effective_from, effective_to, authored_by_ai, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		c.ClauseID, c.TenantID, c.LegalEntityID, c.Title, string(c.Category), c.Body,
		string(c.Status), c.Version, c.JurisdictionID, c.EffectiveFrom, c.EffectiveTo, c.AuthoredByAI,
		c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert clause: %w", err)
	}
	if err := s.snapshotClauseVersion(ctx, tx, c, "Initial draft"); err != nil {
		return fmt.Errorf("snapshot clause version: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetClause(ctx context.Context, id string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := scanClause(tx.QueryRow(ctx,
		`SELECT `+clauseColumns+` FROM clauses WHERE clause_id = $1 AND tenant_id = $2`, id, tenantID))
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return c, nil
}

// lockClause reads a clause FOR UPDATE inside an open transaction — the row
// lock that makes a read-then-write lifecycle transition atomic.
func (s *PgStore) lockClause(ctx context.Context, tx pgx.Tx, id, tenantID string) (*domain.Clause, error) {
	return scanClause(tx.QueryRow(ctx,
		`SELECT `+clauseColumns+` FROM clauses WHERE clause_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID))
}

func (s *PgStore) ListClauses(ctx context.Context, legalEntityID, category string) ([]domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT `+clauseColumns+`
		FROM clauses
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		  AND ($3 = '' OR category = $3)
		ORDER BY created_at DESC`, tenantID, legalEntityID, category,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Clause
	for rows.Next() {
		c, err := scanClause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// UpdateClause is LEG-06's named CreateClauseVersion command. It may only
// act while the clause is DRAFT or LEGAL_REVIEW — an approved clause
// version is effective-dated evidence (§8.1), not a field to edit in place.
func (s *PgStore) UpdateClause(ctx context.Context, c *domain.Clause, changeSummary string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	current, err := s.lockClause(ctx, tx, c.ClauseID, tenantID)
	if err != nil {
		return err
	}
	if current.Status != domain.StatusDraft && current.Status != domain.StatusLegalReview {
		return domain.ErrWrongStatus
	}

	c.Version = current.Version + 1
	c.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE clauses
		SET title=$1, category=$2, body=$3, jurisdiction_id=$4, effective_to=$5, version=$6, updated_at=$7
		WHERE clause_id=$8 AND tenant_id=$9`,
		c.Title, string(c.Category), c.Body, c.JurisdictionID, c.EffectiveTo, c.Version, c.UpdatedAt,
		c.ClauseID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("update clause: %w", err)
	}
	if err := s.snapshotClauseVersion(ctx, tx, c, changeSummary); err != nil {
		return fmt.Errorf("snapshot clause version: %w", err)
	}

	return tx.Commit(ctx)
}

// SubmitClauseForLegalReview moves DRAFT -> LEGAL_REVIEW.
func (s *PgStore) SubmitClauseForLegalReview(ctx context.Context, id, submittedBy string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := s.lockClause(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.StatusDraft {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	c.Status = domain.StatusLegalReview
	c.SubmittedBy = &submittedBy
	c.SubmittedAt = &now
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE clauses SET status=$1, submitted_by=$2, submitted_at=$3, updated_at=$4
		WHERE clause_id=$5 AND tenant_id=$6`,
		string(c.Status), c.SubmittedBy, c.SubmittedAt, c.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ApproveClause moves LEGAL_REVIEW -> APPROVED. Maker-checker (LEG-06 §8:
// "maker-checker for approved standard language") is re-checked against the
// locked row: the principal who submitted the clause for review may not be
// the one who approves it.
func (s *PgStore) ApproveClause(ctx context.Context, id, approvedBy string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := s.lockClause(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.StatusLegalReview {
		return nil, domain.ErrWrongStatus
	}
	if c.SubmittedBy != nil && *c.SubmittedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}

	now := time.Now().UTC()
	c.Status = domain.StatusApproved
	c.ApprovedBy = &approvedBy
	c.ApprovedAt = &now
	c.UpdatedAt = now
	c.Version++

	if _, err := tx.Exec(ctx, `
		UPDATE clauses SET status=$1, approved_by=$2, approved_at=$3, version=$4, updated_at=$5
		WHERE clause_id=$6 AND tenant_id=$7`,
		string(c.Status), c.ApprovedBy, c.ApprovedAt, c.Version, c.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := s.snapshotClauseVersion(ctx, tx, c, "Clause approved"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ActivateClause moves APPROVED -> ACTIVE: the clause becomes available for
// contract assembly (FindApprovedClause).
func (s *PgStore) ActivateClause(ctx context.Context, id, activatedBy string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := s.lockClause(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.StatusApproved {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	c.Status = domain.StatusActive
	c.ActivatedBy = &activatedBy
	c.ActivatedAt = &now
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE clauses SET status=$1, activated_by=$2, activated_at=$3, updated_at=$4
		WHERE clause_id=$5 AND tenant_id=$6`,
		string(c.Status), c.ActivatedBy, c.ActivatedAt, c.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// RetireClause moves ACTIVE -> RETIRED.
func (s *PgStore) RetireClause(ctx context.Context, id, retiredBy string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := s.lockClause(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.StatusActive {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	c.Status = domain.StatusRetired
	c.RetiredBy = &retiredBy
	c.RetiredAt = &now
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE clauses SET status=$1, retired_by=$2, retired_at=$3, updated_at=$4
		WHERE clause_id=$5 AND tenant_id=$6`,
		string(c.Status), c.RetiredBy, c.RetiredAt, c.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// SupersedeClause moves ACTIVE -> SUPERSEDED, recording the replacement
// clause. The old clause's own version history remains addressable — a
// contract already executed against it retains the original version
// (§8.1), which only means something if this clause is never retroactively
// changed or deleted.
func (s *PgStore) SupersedeClause(ctx context.Context, id, supersededBy string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	c, err := s.lockClause(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.StatusActive {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	c.Status = domain.StatusSuperseded
	c.SupersededBy = &supersededBy
	c.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE clauses SET status=$1, superseded_by=$2, updated_at=$3
		WHERE clause_id=$4 AND tenant_id=$5`,
		string(c.Status), c.SupersededBy, c.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) ListClauseVersions(ctx context.Context, clauseID string) ([]domain.ClauseVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT version_id, clause_id, tenant_id, version_number, status, title, body,
		       jurisdiction_id, `+effectiveDateColumns+`, change_summary, created_by, created_at
		FROM clause_versions
		WHERE clause_id=$1 AND tenant_id=$2
		ORDER BY version_number ASC`, clauseID, tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ClauseVersion
	for rows.Next() {
		var v domain.ClauseVersion
		var status string
		if err := rows.Scan(
			&v.VersionID, &v.ClauseID, &v.TenantID, &v.VersionNumber, &status, &v.Title, &v.Body,
			&v.JurisdictionID, &v.EffectiveFrom, &v.EffectiveTo, &v.ChangeSummary, &v.CreatedBy, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		v.Status = domain.Status(status)
		out = append(out, v)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

const templateColumns = `template_id, tenant_id, legal_entity_id, title, contract_type, COALESCE(description,''), clause_ids,
	       status, version, jurisdiction_id, ` + effectiveDateColumns + `, approved_at, approved_by,
	       created_by, created_at, updated_at`

func scanTemplate(row pgx.Row) (*domain.ContractTemplate, error) {
	var t domain.ContractTemplate
	var status string
	err := row.Scan(
		&t.TemplateID, &t.TenantID, &t.LegalEntityID, &t.Title, &t.ContractType, &t.Description, &t.ClauseIDs,
		&status, &t.Version, &t.JurisdictionID, &t.EffectiveFrom, &t.EffectiveTo, &t.ApprovedAt, &t.ApprovedBy,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTemplateNotFound
		}
		return nil, err
	}
	t.Status = domain.Status(status)
	return &t, nil
}

func (s *PgStore) CreateTemplate(ctx context.Context, t *domain.ContractTemplate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if t.TemplateID == "" {
		t.TemplateID = "tmpl-" + uuid.New().String()
	}
	t.TenantID = tenantID
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = domain.StatusDraft
	}
	t.Version = 1
	if t.ClauseIDs == nil {
		t.ClauseIDs = []string{}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO contract_templates
			(template_id, tenant_id, legal_entity_id, title, contract_type, description, clause_ids,
			 status, version, jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		t.TemplateID, t.TenantID, t.LegalEntityID, t.Title, t.ContractType, t.Description, t.ClauseIDs,
		string(t.Status), t.Version, t.JurisdictionID, t.EffectiveFrom, t.EffectiveTo,
		t.CreatedBy, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert template: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetTemplate(ctx context.Context, id string) (*domain.ContractTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	t, err := scanTemplate(tx.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM contract_templates WHERE template_id = $1 AND tenant_id = $2`, id, tenantID))
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return t, nil
}

func (s *PgStore) ListTemplates(ctx context.Context, legalEntityID, contractType string) ([]domain.ContractTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT `+templateColumns+`
		FROM contract_templates
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		  AND ($3 = '' OR contract_type = $3)
		ORDER BY created_at DESC`, tenantID, legalEntityID, contractType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ContractTemplate
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateTemplate(ctx context.Context, t *domain.ContractTemplate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	current, err := scanTemplate(tx.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM contract_templates WHERE template_id = $1 AND tenant_id = $2 FOR UPDATE`,
		t.TemplateID, tenantID))
	if err != nil {
		return err
	}
	if current.Status != domain.StatusDraft {
		return domain.ErrWrongStatus
	}

	t.Version = current.Version + 1
	t.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE contract_templates
		SET title=$1, contract_type=$2, description=$3, clause_ids=$4, jurisdiction_id=$5, effective_to=$6, version=$7, updated_at=$8
		WHERE template_id=$9 AND tenant_id=$10`,
		t.Title, t.ContractType, t.Description, t.ClauseIDs, t.JurisdictionID, t.EffectiveTo, t.Version, t.UpdatedAt,
		t.TemplateID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}

	return tx.Commit(ctx)
}

// ApproveTemplate moves DRAFT -> ACTIVE. TemplateApproved is a named
// canonical event (LEG-06 §8); unlike clauses, the spec names no
// intermediate review state for templates.
func (s *PgStore) ApproveTemplate(ctx context.Context, id, approvedBy string) (*domain.ContractTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	t, err := scanTemplate(tx.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM contract_templates WHERE template_id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID))
	if err != nil {
		return nil, err
	}
	if t.Status != domain.StatusDraft {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	t.Status = domain.StatusActive
	t.ApprovedBy = &approvedBy
	t.ApprovedAt = &now
	t.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE contract_templates SET status=$1, approved_by=$2, approved_at=$3, updated_at=$4
		WHERE template_id=$5 AND tenant_id=$6`,
		string(t.Status), t.ApprovedBy, t.ApprovedAt, t.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

const deviationColumns = `deviation_id, tenant_id, legal_entity_id, clause_id, jurisdiction_id,
	       risk_classification, description, status, proposed_by, proposed_at, approved_by, approved_at`

func scanDeviation(row pgx.Row) (*domain.DeviationRule, error) {
	var d domain.DeviationRule
	var risk, status string
	err := row.Scan(
		&d.DeviationID, &d.TenantID, &d.LegalEntityID, &d.ClauseID, &d.JurisdictionID,
		&risk, &d.Description, &status, &d.ProposedBy, &d.ProposedAt, &d.ApprovedBy, &d.ApprovedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrDeviationNotFound
		}
		return nil, err
	}
	d.RiskClassification = domain.RiskClassification(risk)
	d.Status = domain.DeviationStatus(status)
	return &d, nil
}

func (s *PgStore) CreateDeviationRule(ctx context.Context, d *domain.DeviationRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if d.DeviationID == "" {
		d.DeviationID = "dev-" + uuid.New().String()
	}
	d.TenantID = tenantID
	d.ProposedAt = time.Now().UTC()
	if d.Status == "" {
		d.Status = domain.DeviationStatusProposed
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO clause_deviation_rules
			(deviation_id, tenant_id, legal_entity_id, clause_id, jurisdiction_id,
			 risk_classification, description, status, proposed_by, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		d.DeviationID, d.TenantID, d.LegalEntityID, d.ClauseID, d.JurisdictionID,
		string(d.RiskClassification), d.Description, string(d.Status), d.ProposedBy, d.ProposedAt,
	)
	if err != nil {
		return fmt.Errorf("insert deviation rule: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetDeviationRule(ctx context.Context, id string) (*domain.DeviationRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	d, err := scanDeviation(tx.QueryRow(ctx,
		`SELECT `+deviationColumns+` FROM clause_deviation_rules WHERE deviation_id = $1 AND tenant_id = $2`, id, tenantID))
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return d, nil
}

func (s *PgStore) ListDeviationRules(ctx context.Context, legalEntityID, jurisdictionID, status string) ([]domain.DeviationRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT `+deviationColumns+`
		FROM clause_deviation_rules
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		  AND ($3 = '' OR jurisdiction_id = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY proposed_at DESC`, tenantID, legalEntityID, jurisdictionID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.DeviationRule
	for rows.Next() {
		d, err := scanDeviation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// ApproveDeviationRule moves PROPOSED -> APPROVED. Accountable approval
// (LEG-06 §8.1) is enforced the same maker-checker way as ApproveClause: the
// proposer may not approve their own deviation.
func (s *PgStore) ApproveDeviationRule(ctx context.Context, id, approvedBy string) (*domain.DeviationRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	d, err := scanDeviation(tx.QueryRow(ctx,
		`SELECT `+deviationColumns+` FROM clause_deviation_rules WHERE deviation_id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID))
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DeviationStatusProposed {
		return nil, domain.ErrWrongStatus
	}
	if d.ProposedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}

	now := time.Now().UTC()
	d.Status = domain.DeviationStatusApproved
	d.ApprovedBy = &approvedBy
	d.ApprovedAt = &now

	if _, err := tx.Exec(ctx, `
		UPDATE clause_deviation_rules SET status=$1, approved_by=$2, approved_at=$3
		WHERE deviation_id=$4 AND tenant_id=$5`,
		string(d.Status), d.ApprovedBy, d.ApprovedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
