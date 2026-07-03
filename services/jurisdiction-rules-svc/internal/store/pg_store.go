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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Store is the interface consumed by the handler for validation and admin mutations.
type Store interface {
	// FindByID returns the Jurisdiction with the given jurisdiction_id.
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)

	// CreateJurisdiction inserts a new jurisdiction idempotently.
	// Returns (record, created=true, nil) if new; (record, created=false, nil) if identical dedup match;
	// (nil, false, ErrConflict) if dedup match has differing attributes.
	CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error)

	// DeactivateJurisdiction sets active_flag=false and updates audit columns.
	DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error)

	// FindRuleByID looks up a rule by ID.
	FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error)

	// CreateRule inserts a new rule idempotently.
	// Returns (record, created=true, nil) if new; (record, created=false, nil) if identical dedup match;
	// (nil, false, ErrConflict) if dedup match has differing payload or attributes.
	CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error)

	// TransitionRuleStatus atomically updates rule_status if current status is in allowedPriors.
	// Performs a pre-read no-op check: if current == newStatus, returns (record, nil) without updating DB.
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

// FindByID looks up a jurisdiction by its UUID primary key.
func (s *PgStore) FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error) {
	const query = `
		SELECT
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
			updated_by_principal_id
		FROM jurisdictions
		WHERE jurisdiction_id    = $1
		  AND active_flag        = TRUE
		  AND (effective_to IS NULL OR effective_to > NOW())`

	row := s.pool.QueryRow(ctx, query, jurisdictionID)

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
		DO UPDATE SET jurisdiction_code = EXCLUDED.jurisdiction_code
		RETURNING jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type,
		          parent_jurisdiction_id, authority_type, effective_from, effective_to,
		          active_flag, created_at, created_by_principal_id, schema_version,
		          updated_at, updated_by_principal_id;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionID, params.JurisdictionCode, params.JurisdictionName, params.JurisdictionType,
		params.ParentJurisdictionID, params.AuthorityType, params.EffectiveFrom, params.EffectiveTo,
		params.ActiveFlag, params.CreatedByPrincipalID, params.SchemaVersion,
	)

	j := &domain.Jurisdiction{}
	err := row.Scan(
		&j.JurisdictionID, &j.JurisdictionCode, &j.JurisdictionName, &j.JurisdictionType,
		&j.ParentJurisdictionID, &j.AuthorityType, &j.EffectiveFrom, &j.EffectiveTo,
		&j.ActiveFlag, &j.CreatedAt, &j.CreatedByPrincipalID, &j.SchemaVersion,
		&j.UpdatedAt, &j.UpdatedByPrincipalID,
	)
	if err != nil {
		s.log.Error("pg CreateJurisdiction failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Check if newly created or matched existing
	if j.JurisdictionID == params.JurisdictionID {
		return j, true, nil
	}

	// Existing matched on dedup key. Check for attribute mismatch (409 Conflict).
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
		RETURNING jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type,
		          parent_jurisdiction_id, authority_type, effective_from, effective_to,
		          active_flag, created_at, created_by_principal_id, schema_version,
		          updated_at, updated_by_principal_id;`

	row := s.pool.QueryRow(ctx, query, jurisdictionID, actorID)

	j := &domain.Jurisdiction{}
	err := row.Scan(
		&j.JurisdictionID, &j.JurisdictionCode, &j.JurisdictionName, &j.JurisdictionType,
		&j.ParentJurisdictionID, &j.AuthorityType, &j.EffectiveFrom, &j.EffectiveTo,
		&j.ActiveFlag, &j.CreatedAt, &j.CreatedByPrincipalID, &j.SchemaVersion,
		&j.UpdatedAt, &j.UpdatedByPrincipalID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg DeactivateJurisdiction failed", zap.String("id", jurisdictionID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return j, nil
}

// FindRuleByID looks up a rule by ID without active status checks (admin can view any state).
func (s *PgStore) FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error) {
	const query = `
		SELECT jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name,
		       effective_from, effective_to, rule_payload, source_reference, rule_status,
		       external_feed_reference, legal_drift_state, created_at, created_by_principal_id,
		       schema_version, updated_at, updated_by_principal_id
		FROM jurisdiction_rules
		WHERE jurisdiction_rule_id = $1;`

	row := s.pool.QueryRow(ctx, query, ruleID)
	r := &domain.JurisdictionRule{}
	err := row.Scan(
		&r.JurisdictionRuleID, &r.JurisdictionID, &r.RuleDomain, &r.RuleCode, &r.RuleName,
		&r.EffectiveFrom, &r.EffectiveTo, &r.RulePayload, &r.SourceReference, &r.RuleStatus,
		&r.ExternalFeedReference, &r.LegalDriftState, &r.CreatedAt, &r.CreatedByPrincipalID,
		&r.SchemaVersion, &r.UpdatedAt, &r.UpdatedByPrincipalID,
	)
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
		DO UPDATE SET rule_code = EXCLUDED.rule_code
		RETURNING jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name,
		          effective_from, effective_to, rule_payload, source_reference, rule_status,
		          external_feed_reference, legal_drift_state, created_at, created_by_principal_id,
		          schema_version, updated_at, updated_by_principal_id;`

	row := s.pool.QueryRow(ctx, query,
		params.JurisdictionRuleID, params.JurisdictionID, params.RuleDomain, params.RuleCode, params.RuleName,
		params.EffectiveFrom, params.EffectiveTo, params.RulePayload, params.SourceReference, params.RuleStatus,
		params.ExternalFeedReference, params.LegalDriftState, params.CreatedByPrincipalID, params.SchemaVersion,
	)

	r := &domain.JurisdictionRule{}
	err := row.Scan(
		&r.JurisdictionRuleID, &r.JurisdictionID, &r.RuleDomain, &r.RuleCode, &r.RuleName,
		&r.EffectiveFrom, &r.EffectiveTo, &r.RulePayload, &r.SourceReference, &r.RuleStatus,
		&r.ExternalFeedReference, &r.LegalDriftState, &r.CreatedAt, &r.CreatedByPrincipalID,
		&r.SchemaVersion, &r.UpdatedAt, &r.UpdatedByPrincipalID,
	)
	if err != nil {
		s.log.Error("pg CreateRule failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if r.JurisdictionRuleID == params.JurisdictionRuleID {
		return r, true, nil
	}

	// Existing matched on dedup key. Check for payload mismatch (409 Conflict).
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
	// Pre-read check for idempotent retry
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
		RETURNING jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name,
		          effective_from, effective_to, rule_payload, source_reference, rule_status,
		          external_feed_reference, legal_drift_state, created_at, created_by_principal_id,
		          schema_version, updated_at, updated_by_principal_id;`

	row := s.pool.QueryRow(ctx, query, newStatus, actorID, ruleID, allowedPriors)
	r := &domain.JurisdictionRule{}
	err = row.Scan(
		&r.JurisdictionRuleID, &r.JurisdictionID, &r.RuleDomain, &r.RuleCode, &r.RuleName,
		&r.EffectiveFrom, &r.EffectiveTo, &r.RulePayload, &r.SourceReference, &r.RuleStatus,
		&r.ExternalFeedReference, &r.LegalDriftState, &r.CreatedAt, &r.CreatedByPrincipalID,
		&r.SchemaVersion, &r.UpdatedAt, &r.UpdatedByPrincipalID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Since we pre-read and know the rule exists, 0 rows affected means state not in allowedPriors.
			return nil, domain.ErrInvalidTransition
		}
		s.log.Error("pg TransitionRuleStatus failed", zap.String("id", ruleID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}
