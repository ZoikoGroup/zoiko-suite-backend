// Package store provides the PostgreSQL implementation of the jurisdiction
// rules read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers, service, or domain packages.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ListParams controls pagination and filtering for Store.List.
// All filter fields are optional — zero value = no filter applied.
type ListParams struct {
	// JurisdictionType filters by type e.g. "COUNTRY", "STATE_PROVINCE".
	// Empty = return all types.
	JurisdictionType string

	// ActiveOnly = true limits results to active_flag=true and non-expired rows.
	ActiveOnly bool

	// Limit is the page size. 0 defaults to 50; max enforced at 200.
	Limit int

	// Offset is the zero-based page offset.
	Offset int
}

// FindRulesParams controls point-in-time rule lookup.
type FindRulesParams struct {
	// JurisdictionID is required.
	JurisdictionID string

	// Domain filters by rule_domain e.g. "PAYROLL", "TAX".
	// Empty string = all domains. Never use a Go nil here — see handler comment.
	Domain string

	// EffectiveAt is the point-in-time for the half-open interval query.
	// Zero value is treated as time.Now() inside FindRules.
	EffectiveAt time.Time

	// Limit is the page size. 0 defaults to 50; max enforced at 200.
	Limit int

	// Offset is the zero-based page offset.
	Offset int
}

// Store is the interface consumed by the handler for validation, queries, and admin mutations.
type Store interface {
	// FindByID returns the Jurisdiction with the given jurisdiction_id.
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)

	// List returns a paginated slice of jurisdictions matching params.
	List(ctx context.Context, params ListParams) ([]*domain.Jurisdiction, error)

	// FindAncestors walks the parent chain starting from jurisdictionID and
	// returns the ordered slice from immediate parent up to the root.
	FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error)

	// FindRules returns jurisdiction rules active at the given point in time.
	FindRules(ctx context.Context, params FindRulesParams) ([]*domain.JurisdictionRule, error)

	// CreateJurisdiction inserts a new jurisdiction idempotently.
	CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error)

	// DeactivateJurisdiction sets active_flag=false and updates audit columns.
	DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error)

	// FindRuleByID looks up a rule by ID.
	FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error)

	// CreateRule inserts a new rule idempotently.
	CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error)

	// TransitionRuleStatus atomically updates rule_status if current status is in allowedPriors.
	TransitionRuleStatus(ctx context.Context, ruleID, newStatus string, allowedPriors []string, actorID string) (*domain.JurisdictionRule, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// jurisdictionColumns is the standard SELECT column list shared by all queries.
// Order must match scanJurisdiction exactly.
const jurisdictionColumns = `
	jurisdiction_id,
	jurisdiction_code,
	jurisdiction_name,
	jurisdiction_type,
	parent_jurisdiction_id,
	authority_type,
	effective_from,
	effective_to,
	active_flag,
	created_at,
	created_by_principal_id,
	schema_version,
	updated_at,
	updated_by_principal_id`

// scanJurisdiction scans one row produced by a jurisdictionColumns SELECT.
func scanJurisdiction(row pgx.Row) (*domain.Jurisdiction, error) {
	j := &domain.Jurisdiction{}
	err := row.Scan(
		&j.JurisdictionID,
		&j.JurisdictionCode,
		&j.JurisdictionName,
		&j.JurisdictionType,
		&j.ParentJurisdictionID,
		&j.AuthorityType,
		&j.EffectiveFrom,
		&j.EffectiveTo,
		&j.ActiveFlag,
		&j.CreatedAt,
		&j.CreatedByPrincipalID,
		&j.SchemaVersion,
		&j.UpdatedAt,
		&j.UpdatedByPrincipalID,
	)
	return j, err
}

// FindByID looks up a jurisdiction by its UUID primary key.
func (s *PgStore) FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error) {
	const query = `
		SELECT ` + jurisdictionColumns + `
		FROM jurisdictions
		WHERE jurisdiction_id    = $1
		  AND active_flag        = TRUE
		  AND (effective_to IS NULL OR effective_to > NOW())`

	row := s.pool.QueryRow(ctx, query, jurisdictionID)
	j, err := scanJurisdiction(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg FindByID failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return j, nil
}

// CreateJurisdiction inserts a new jurisdiction or returns existing on dedup match.
func (s *PgStore) CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error) {
	if params.JurisdictionID == "" {
		params.JurisdictionID = uuid.New().String()
	}
	if params.SchemaVersion == "" {
		params.SchemaVersion = "1.0"
	}

	const query = `
		INSERT INTO jurisdictions (
			jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type,
			parent_jurisdiction_id, authority_type, effective_from, effective_to,
			active_flag, created_by_principal_id, schema_version
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (jurisdiction_code, jurisdiction_type, COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID))
		DO NOTHING
		RETURNING ` + jurisdictionColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionID, params.JurisdictionCode, params.JurisdictionName, params.JurisdictionType,
		params.ParentJurisdictionID, params.AuthorityType, params.EffectiveFrom, params.EffectiveTo,
		params.ActiveFlag, params.CreatedByPrincipalID, params.SchemaVersion,
	)

	j, err := scanJurisdiction(row)
	if err == nil {
		return j, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateJurisdiction failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on (jurisdiction_code, jurisdiction_type, parent_jurisdiction_id). Lookup existing record.
	const lookupQuery = `
		SELECT ` + jurisdictionColumns + `
		FROM jurisdictions
		WHERE jurisdiction_code = $1
		  AND jurisdiction_type = $2
		  AND COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID) = COALESCE($3::uuid, '00000000-0000-0000-0000-000000000000'::UUID);`

	row = s.pool.QueryRow(ctx, lookupQuery, params.JurisdictionCode, params.JurisdictionType, params.ParentJurisdictionID)
	j, err = scanJurisdiction(row)
	if err != nil {
		s.log.Error("pg CreateJurisdiction lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if j.JurisdictionName != params.JurisdictionName || j.AuthorityType != params.AuthorityType {
		s.log.Warn("jurisdiction dedup match but attribute mismatch (409 conflict)",
			zap.String("existing_id", j.JurisdictionID),
			zap.String("req_id", params.JurisdictionID),
		)
		return nil, false, domain.ErrConflict
	}

	return j, false, nil
}

// DeactivateJurisdiction sets active_flag=false on an existing jurisdiction.
func (s *PgStore) DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error) {
	const query = `
		UPDATE jurisdictions
		SET active_flag = FALSE, updated_at = NOW(), updated_by_principal_id = $2
		WHERE jurisdiction_id = $1
		RETURNING ` + jurisdictionColumns + `;`

	row := s.pool.QueryRow(ctx, query, jurisdictionID, actorID)
	j, err := scanJurisdiction(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg DeactivateJurisdiction failed", zap.String("id", jurisdictionID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return j, nil
}

// List returns a paginated, optionally-filtered slice of jurisdictions.
func (s *PgStore) List(ctx context.Context, params ListParams) ([]*domain.Jurisdiction, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	args := []any{}
	conditions := []string{}
	argIdx := 1

	if params.JurisdictionType != "" {
		conditions = append(conditions, fmt.Sprintf("jurisdiction_type = $%d", argIdx))
		args = append(args, params.JurisdictionType)
		argIdx++
	}
	if params.ActiveOnly {
		conditions = append(conditions,
			"active_flag = TRUE",
			"(effective_to IS NULL OR effective_to > NOW())",
		)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM   jurisdictions
		%s
		ORDER BY jurisdiction_code ASC
		LIMIT  $%d OFFSET $%d`,
		jurisdictionColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, params.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg List failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.Jurisdiction
	for rows.Next() {
		j, scanErr := scanJurisdiction(rows)
		if scanErr != nil {
			s.log.Error("pg List scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, j)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg List rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

const maxAncestorDepth = 20

// FindAncestors walks the parent chain of jurisdictionID iteratively.
func (s *PgStore) FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error) {
	const query = `SELECT ` + jurisdictionColumns + ` FROM jurisdictions WHERE jurisdiction_id = $1`

	start, err := scanJurisdiction(s.pool.QueryRow(ctx, query, jurisdictionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg FindAncestors: start lookup failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	var ancestors []*domain.Jurisdiction
	currentParentID := start.ParentJurisdictionID

	for depth := 0; depth < maxAncestorDepth && currentParentID != nil; depth++ {
		ancestor, scanErr := scanJurisdiction(s.pool.QueryRow(ctx, query, *currentParentID))
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				s.log.Warn("pg FindAncestors: dangling parent reference",
					zap.String("parent_jurisdiction_id", *currentParentID),
				)
				break
			}
			s.log.Error("pg FindAncestors: ancestor lookup failed",
				zap.String("parent_jurisdiction_id", *currentParentID),
				zap.Error(scanErr),
			)
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		ancestors = append(ancestors, ancestor)
		currentParentID = ancestor.ParentJurisdictionID
	}

	return ancestors, nil
}

// ruleColumns is the standard SELECT column list for jurisdiction_rules queries.
// Order must match scanJurisdictionRule exactly.
const ruleColumns = `
	jurisdiction_rule_id,
	jurisdiction_id,
	rule_domain,
	rule_code,
	rule_name,
	effective_from,
	effective_to,
	rule_payload,
	source_reference,
	external_feed_reference,
	rule_status,
	legal_drift_state,
	created_at,
	created_by_principal_id,
	schema_version,
	updated_at,
	updated_by_principal_id`

// scanJurisdictionRule scans one row produced by a ruleColumns SELECT.
func scanJurisdictionRule(row pgx.Row) (*domain.JurisdictionRule, error) {
	r := &domain.JurisdictionRule{}
	err := row.Scan(
		&r.JurisdictionRuleID,
		&r.JurisdictionID,
		&r.RuleDomain,
		&r.RuleCode,
		&r.RuleName,
		&r.EffectiveFrom,
		&r.EffectiveTo,
		&r.RulePayload,
		&r.SourceReference,
		&r.ExternalFeedReference,
		&r.RuleStatus,
		&r.LegalDriftState,
		&r.CreatedAt,
		&r.CreatedByPrincipalID,
		&r.SchemaVersion,
		&r.UpdatedAt,
		&r.UpdatedByPrincipalID,
	)
	return r, err
}

// FindRuleByID looks up a rule by ID without active status checks.
func (s *PgStore) FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error) {
	const query = `
		SELECT ` + ruleColumns + `
		FROM jurisdiction_rules
		WHERE jurisdiction_rule_id = $1;`

	row := s.pool.QueryRow(ctx, query, ruleID)
	r, err := scanJurisdictionRule(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRuleNotFound
		}
		s.log.Error("pg FindRuleByID failed", zap.String("id", ruleID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// CreateRule inserts a new rule idempotently.
func (s *PgStore) CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error) {
	if params.JurisdictionRuleID == "" {
		params.JurisdictionRuleID = uuid.New().String()
	}
	if params.SchemaVersion == "" {
		params.SchemaVersion = "1.0"
	}
	if params.LegalDriftState == "" {
		params.LegalDriftState = "CURRENT"
	}

	const query = `
		INSERT INTO jurisdiction_rules (
			jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name,
			effective_from, effective_to, rule_payload, source_reference, rule_status,
			external_feed_reference, legal_drift_state, created_by_principal_id, schema_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (jurisdiction_id, rule_code, effective_from)
		DO NOTHING
		RETURNING ` + ruleColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionRuleID, params.JurisdictionID, params.RuleDomain, params.RuleCode, params.RuleName,
		params.EffectiveFrom, params.EffectiveTo, params.RulePayload, params.SourceReference, params.RuleStatus,
		params.ExternalFeedReference, params.LegalDriftState, params.CreatedByPrincipalID, params.SchemaVersion,
	)

	r, err := scanJurisdictionRule(row)
	if err == nil {
		return r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateRule failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on (jurisdiction_id, rule_code, effective_from). Lookup existing record.
	const lookupQuery = `
		SELECT ` + ruleColumns + `
		FROM jurisdiction_rules
		WHERE jurisdiction_id = $1
		  AND rule_code = $2
		  AND effective_from = $3;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.JurisdictionID, params.RuleCode, params.EffectiveFrom)
	r, err = scanJurisdictionRule(row)
	if err != nil {
		s.log.Error("pg CreateRule lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if !bytes.Equal(r.RulePayload, params.RulePayload) || r.RuleName != params.RuleName {
		s.log.Warn("rule dedup match but payload/name mismatch (409 conflict)",
			zap.String("existing_id", r.JurisdictionRuleID),
			zap.String("req_id", params.JurisdictionRuleID),
		)
		return nil, false, domain.ErrConflict
	}

	return r, false, nil
}

// TransitionRuleStatus updates rule_status atomically with a state machine check and pre-read retry no-op check.
func (s *PgStore) TransitionRuleStatus(ctx context.Context, ruleID, newStatus string, allowedPriors []string, actorID string) (*domain.JurisdictionRule, error) {
	current, err := s.FindRuleByID(ctx, ruleID)
	if err != nil {
		return nil, err
	}
	if current.RuleStatus == newStatus {
		s.log.Debug("rule status transition idempotent no-op", zap.String("rule_id", ruleID), zap.String("status", newStatus))
		return current, nil
	}

	const query = `
		UPDATE jurisdiction_rules
		SET rule_status = $1, updated_at = NOW(), updated_by_principal_id = $2
		WHERE jurisdiction_rule_id = $3 AND rule_status = ANY($4::text[])
		RETURNING ` + ruleColumns + `;`

	row := s.pool.QueryRow(ctx, query, newStatus, actorID, ruleID, allowedPriors)
	r, err := scanJurisdictionRule(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrInvalidTransition
		}
		s.log.Error("pg TransitionRuleStatus failed", zap.String("id", ruleID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// FindRules returns rules for a jurisdiction active at a point in time.
func (s *PgStore) FindRules(ctx context.Context, params FindRulesParams) ([]*domain.JurisdictionRule, error) {
	_, err := s.FindByID(ctx, params.JurisdictionID)
	if err != nil {
		return nil, err
	}

	at := params.EffectiveAt
	if at.IsZero() {
		at = time.Now().UTC()
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	const query = `
		SELECT ` + ruleColumns + `
		FROM   jurisdiction_rules
		WHERE  jurisdiction_id = $1
		  AND  ($2 = '' OR rule_domain = $2)
		  AND  rule_status    != 'DRAFT'
		  AND  effective_from  <= $3
		  AND  (effective_to IS NULL OR effective_to > $3)
		ORDER BY rule_domain ASC, effective_from ASC
		LIMIT  $4 OFFSET $5`

	rows, err := s.pool.Query(ctx, query,
		params.JurisdictionID,
		params.Domain,
		at,
		limit,
		params.Offset,
	)
	if err != nil {
		s.log.Error("pg FindRules failed",
			zap.String("jurisdiction_id", params.JurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.JurisdictionRule
	for rows.Next() {
		rule, scanErr := scanJurisdictionRule(rows)
		if scanErr != nil {
			s.log.Error("pg FindRules scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, rule)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg FindRules rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}
