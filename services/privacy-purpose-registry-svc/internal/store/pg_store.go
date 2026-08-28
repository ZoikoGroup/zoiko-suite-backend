// Package store implements privacy-purpose-registry-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

// Store is the interface the handler depends on — narrow enough to be
// stubbed in tests without a real database.
type Store interface {
	CreatePurpose(ctx context.Context, tenantID string, req domain.CreatePurposeRequest, principalID string) (*domain.Purpose, *domain.PurposeVersion, error)
	CreatePurposeVersion(ctx context.Context, purposeID string, req domain.CreatePurposeVersionRequest, principalID string) (*domain.PurposeVersion, error)
	PublishPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error)
	FindPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error)
	ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf time.Time) (*domain.PurposeVersion, error)
	ListPurposes(ctx context.Context) ([]domain.PurposeVersion, error)
	// IsPurposePublished reports whether purposeID resolves (in the
	// caller's tenant scope, platform-wide included) to a currently
	// PUBLISHED version — used by Validate to enforce PRV-001
	// (PURPOSE_NOT_REGISTERED) without exposing full purpose content.
	IsPurposePublished(ctx context.Context, purposeID string) (bool, error)

	CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error)
	CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error)
	FindActivityVersion(ctx context.Context, activityID, versionID string) (*domain.ProcessingActivityVersion, error)
	ResolveActivityAsOf(ctx context.Context, activityID string, asOf time.Time) (*domain.ProcessingActivityVersion, error)
	ListActiveActivities(ctx context.Context, role, jurisdiction string) ([]domain.ProcessingActivityVersion, error)

	SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding) (*domain.ProcessingActivityVersion, error)
	TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus) (*domain.ProcessingActivityVersion, error)
	RejectActivity(ctx context.Context, activityID, versionID, reason string) (*domain.ProcessingActivityVersion, error)
	ActivateActivity(ctx context.Context, activityID, versionID string, effectiveFrom time.Time) (*domain.ProcessingActivityVersion, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context — same doctrine as every RLS-backed store in this
// platform: transaction-local, never session-wide on a pooled connection.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func marshalSlice(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	raw, _ := json.Marshal(s)
	return raw
}

func unmarshalSlice(raw []byte, dest *[]string) {
	if len(raw) == 0 {
		*dest = []string{}
		return
	}
	_ = json.Unmarshal(raw, dest)
}

// ── purposes ─────────────────────────────────────────────────────────────────

const purposeVersionColumns = `
	purpose_version_id, purpose_id, statement, compatibility_class, lawful_basis_refs,
	version_status, effective_from, supersedes_version_id, created_at, created_by_principal_id`

const purposeVersionColumnsJoined = `
	pv.purpose_version_id, pv.purpose_id, pv.statement, pv.compatibility_class, pv.lawful_basis_refs,
	pv.version_status, pv.effective_from, pv.supersedes_version_id, pv.created_at, pv.created_by_principal_id`

func scanPurposeVersion(row pgx.Row) (*domain.PurposeVersion, error) {
	v := &domain.PurposeVersion{}
	var basisRaw []byte
	err := row.Scan(&v.PurposeVersionID, &v.PurposeID, &v.Statement, &v.CompatibilityClass, &basisRaw,
		&v.VersionStatus, &v.EffectiveFrom, &v.SupersedesVersionID, &v.CreatedAt, &v.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	unmarshalSlice(basisRaw, &v.LawfulBasisRefs)
	return v, nil
}

func (s *PgStore) CreatePurpose(ctx context.Context, tenantID string, req domain.CreatePurposeRequest, principalID string) (*domain.Purpose, *domain.PurposeVersion, error) {
	purposeID := uuid.New().String()
	versionID := uuid.New().String()
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	basisRaw := marshalSlice(req.LawfulBasisRefs)

	var purpose domain.Purpose
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO purposes (purpose_id, tenant_id, created_by_principal_id)
			VALUES ($1, $2, $3)
			RETURNING purpose_id, tenant_id, created_at, created_by_principal_id`,
			purposeID, strPtrOrNil(tenantID), principalID,
		).Scan(&purpose.PurposeID, &purpose.TenantID, &purpose.CreatedAt, &purpose.CreatedByPrincipalID); err != nil {
			return err
		}

		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			INSERT INTO purpose_versions (`+purposeVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', $6, NULL, NOW(), $7)
			RETURNING `+purposeVersionColumns,
			versionID, purposeID, req.Statement, req.CompatibilityClass, basisRaw, effectiveFrom, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreatePurpose failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &purpose, version, nil
}

func (s *PgStore) CreatePurposeVersion(ctx context.Context, purposeID string, req domain.CreatePurposeVersionRequest, principalID string) (*domain.PurposeVersion, error) {
	versionID := uuid.New().String()
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	basisRaw := marshalSlice(req.LawfulBasisRefs)

	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		// The parent must belong to the same purpose_id AND be visible in
		// this tenant scope (RLS on purposes enforces the latter via the
		// join) — otherwise a caller could graft a new version onto
		// another tenant's purpose by guessing its parent_version_id.
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM purpose_versions pv
				JOIN purposes p ON p.purpose_id = pv.purpose_id
				WHERE pv.purpose_version_id = $1 AND pv.purpose_id = $2
			)`, req.ParentVersionID, purposeID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrPurposeVersionNotFound
		}

		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			INSERT INTO purpose_versions (`+purposeVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', $6, $7, NOW(), $8)
			RETURNING `+purposeVersionColumns,
			versionID, purposeID, req.Statement, req.CompatibilityClass, basisRaw, effectiveFrom, req.ParentVersionID, principalID,
		))
		return err
	})
	if errors.Is(err, domain.ErrPurposeVersionNotFound) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg CreatePurposeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) PublishPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error) {
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		// Atomic WHERE-guarded transition: only affects a row that is
		// still DRAFT, same doctrine as retention-registry-svc's
		// ReleaseLegalHold — a concurrent double-publish affects at most
		// one caller's UPDATE.
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			UPDATE purpose_versions
			SET version_status = 'PUBLISHED'
			WHERE purpose_version_id = $1 AND purpose_id = $2 AND version_status = 'DRAFT'
			RETURNING `+purposeVersionColumns,
			versionID, purposeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "doesn't exist" from "exists but already published"
		// so the handler can return the right status code.
		existing, findErr := s.FindPurposeVersion(ctx, purposeID, versionID)
		if findErr == nil && existing != nil {
			return nil, domain.ErrPurposeAlreadyPublished
		}
		return nil, domain.ErrPurposeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg PublishPurposeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) FindPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error) {
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			SELECT `+purposeVersionColumnsJoined+`
			FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.purpose_version_id = $1 AND pv.purpose_id = $2`,
			versionID, purposeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindPurposeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf time.Time) (*domain.PurposeVersion, error) {
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			SELECT `+purposeVersionColumnsJoined+`
			FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.purpose_id = $1 AND pv.version_status = 'PUBLISHED' AND pv.effective_from <= $2
			ORDER BY pv.effective_from DESC, pv.sequence_no DESC
			LIMIT 1`,
			purposeID, asOf,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPurposeNotFound
	}
	if err != nil {
		s.log.Error("pg ResolvePurposeAsOf failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ListPurposes(ctx context.Context) ([]domain.PurposeVersion, error) {
	var out []domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+purposeVersionColumnsJoined+`
			FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.version_status = 'PUBLISHED'
			ORDER BY pv.created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanPurposeVersion(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListPurposes failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) IsPurposePublished(ctx context.Context, purposeID string) (bool, error) {
	var exists bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM purpose_versions pv
				JOIN purposes p ON p.purpose_id = pv.purpose_id
				WHERE pv.purpose_id = $1 AND pv.version_status = 'PUBLISHED'
			)`, purposeID,
		).Scan(&exists)
	})
	if err != nil {
		s.log.Error("pg IsPurposePublished failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return exists, nil
}

// ── processing activities ────────────────────────────────────────────────────

const activityVersionColumns = `
	activity_version_id, activity_id, privacy_role, owner, purpose_ids, subject_classes,
	data_categories, sources, recipients, jurisdictions, retention_rule_refs, transfer_refs,
	version_status, validation_findings, rejection_reason, effective_from, supersedes_version_id,
	created_at, created_by_principal_id`

const activityVersionColumnsJoined = `
	av.activity_version_id, av.activity_id, av.privacy_role, av.owner, av.purpose_ids, av.subject_classes,
	av.data_categories, av.sources, av.recipients, av.jurisdictions, av.retention_rule_refs, av.transfer_refs,
	av.version_status, av.validation_findings, av.rejection_reason, av.effective_from, av.supersedes_version_id,
	av.created_at, av.created_by_principal_id`

func scanActivityVersion(row pgx.Row) (*domain.ProcessingActivityVersion, error) {
	v := &domain.ProcessingActivityVersion{}
	var purposeIDs, subjectClasses, dataCategories, sources, recipients, jurisdictions, retentionRuleRefs, transferRefs, findingsRaw []byte
	err := row.Scan(&v.ActivityVersionID, &v.ActivityID, &v.PrivacyRole, &v.Owner,
		&purposeIDs, &subjectClasses, &dataCategories, &sources, &recipients, &jurisdictions,
		&retentionRuleRefs, &transferRefs,
		&v.VersionStatus, &findingsRaw, &v.RejectionReason, &v.EffectiveFrom, &v.SupersedesVersionID,
		&v.CreatedAt, &v.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	unmarshalSlice(purposeIDs, &v.PurposeIDs)
	unmarshalSlice(subjectClasses, &v.SubjectClasses)
	unmarshalSlice(dataCategories, &v.DataCategories)
	unmarshalSlice(sources, &v.Sources)
	unmarshalSlice(recipients, &v.Recipients)
	unmarshalSlice(jurisdictions, &v.Jurisdictions)
	unmarshalSlice(retentionRuleRefs, &v.RetentionRuleRefs)
	unmarshalSlice(transferRefs, &v.TransferRefs)
	if len(findingsRaw) > 0 {
		_ = json.Unmarshal(findingsRaw, &v.ValidationFindings)
	}
	return v, nil
}

type activityContentArgs struct {
	purposeIDs, subjectClasses, dataCategories, sources, recipients, jurisdictions, retentionRuleRefs, transferRefs []byte
}

func marshalActivityContent(purposeIDs, subjectClasses, dataCategories, sources, recipients, jurisdictions, retentionRuleRefs, transferRefs []string) activityContentArgs {
	return activityContentArgs{
		purposeIDs:        marshalSlice(purposeIDs),
		subjectClasses:    marshalSlice(subjectClasses),
		dataCategories:    marshalSlice(dataCategories),
		sources:           marshalSlice(sources),
		recipients:        marshalSlice(recipients),
		jurisdictions:     marshalSlice(jurisdictions),
		retentionRuleRefs: marshalSlice(retentionRuleRefs),
		transferRefs:      marshalSlice(transferRefs),
	}
}

func (s *PgStore) CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error) {
	activityID := uuid.New().String()
	versionID := uuid.New().String()
	cols := marshalActivityContent(req.PurposeIDs, req.SubjectClasses, req.DataCategories, req.Sources, req.Recipients, req.Jurisdictions, req.RetentionRuleRefs, req.TransferRefs)

	var activity domain.ProcessingActivity
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO processing_activities (activity_id, tenant_id, created_by_principal_id)
			VALUES ($1, $2, $3)
			RETURNING activity_id, tenant_id, created_at, created_by_principal_id`,
			activityID, strPtrOrNil(tenantID), principalID,
		).Scan(&activity.ActivityID, &activity.TenantID, &activity.CreatedAt, &activity.CreatedByPrincipalID); err != nil {
			return err
		}

		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			INSERT INTO processing_activity_versions (`+activityVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'DRAFT', NULL, NULL, NULL, NULL, NOW(), $13)
			RETURNING `+activityVersionColumns,
			versionID, activityID, req.PrivacyRole, req.Owner,
			cols.purposeIDs, cols.subjectClasses, cols.dataCategories, cols.sources, cols.recipients,
			cols.jurisdictions, cols.retentionRuleRefs, cols.transferRefs, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateActivity failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &activity, version, nil
}

func (s *PgStore) CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error) {
	versionID := uuid.New().String()
	cols := marshalActivityContent(req.PurposeIDs, req.SubjectClasses, req.DataCategories, req.Sources, req.Recipients, req.Jurisdictions, req.RetentionRuleRefs, req.TransferRefs)

	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM processing_activity_versions av
				JOIN processing_activities a ON a.activity_id = av.activity_id
				WHERE av.activity_version_id = $1 AND av.activity_id = $2
			)`, req.ParentVersionID, activityID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrActivityVersionNotFound
		}

		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			INSERT INTO processing_activity_versions (`+activityVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'DRAFT', NULL, NULL, NULL, $13, NOW(), $14)
			RETURNING `+activityVersionColumns,
			versionID, activityID, req.PrivacyRole, req.Owner,
			cols.purposeIDs, cols.subjectClasses, cols.dataCategories, cols.sources, cols.recipients,
			cols.jurisdictions, cols.retentionRuleRefs, cols.transferRefs, req.ParentVersionID, principalID,
		))
		return err
	})
	if errors.Is(err, domain.ErrActivityVersionNotFound) {
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg CreateActivityVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) FindActivityVersion(ctx context.Context, activityID, versionID string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_version_id = $1 AND av.activity_id = $2`,
			versionID, activityID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindActivityVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// ResolveActivityAsOf implements the doc's "resolve exact valid/known
// version for historical evidence" (GET .../{id}?as_of=). Valid-time only
// (effective_from) — not full bitemporal with retroactive correction
// tracking, which is a real, documented v1 simplification (see domain
// package doc comment). ACTIVE/SUSPENDED/RETIRED all qualify because each
// reached ACTIVE at some point with a real effective_from; RETIRED is
// included because "RETIRED preserves history" (Figure 4) — a query for a
// past as_of can and should land on a version that has since been
// retired.
func (s *PgStore) ResolveActivityAsOf(ctx context.Context, activityID string, asOf time.Time) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_id = $1
			  AND av.version_status IN ('ACTIVE', 'SUSPENDED', 'RETIRED')
			  AND av.effective_from <= $2
			ORDER BY av.effective_from DESC, av.sequence_no DESC
			LIMIT 1`,
			activityID, asOf,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrActivityNotFound
	}
	if err != nil {
		s.log.Error("pg ResolveActivityAsOf failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// ListActiveActivities backs GET /privacy/ropa — the role/jurisdiction-
// filtered processing inventory projection (§9.1). Only ACTIVE versions:
// this is "what is happening now," not a historical archive.
func (s *PgStore) ListActiveActivities(ctx context.Context, role, jurisdiction string) ([]domain.ProcessingActivityVersion, error) {
	var out []domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.version_status = 'ACTIVE'
			  AND ($1 = '' OR av.privacy_role = $1)
			  AND ($2 = '' OR av.jurisdictions @> to_jsonb($2::text))
			ORDER BY av.effective_from DESC`,
			role, jurisdiction)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanActivityVersion(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListActiveActivities failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding) (*domain.ProcessingActivityVersion, error) {
	if findings == nil {
		findings = []domain.ValidationFinding{}
	}
	findingsRaw, err := json.Marshal(findings)
	if err != nil {
		return nil, err
	}
	newStatus := domain.ActivityStatusValidated
	if len(findings) > 0 {
		newStatus = domain.ActivityStatusDraft // stays DRAFT — PRV-I13, never silently PERMIT
	}

	var version *domain.ProcessingActivityVersion
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET validation_findings = $3, version_status = $4
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'DRAFT'
			RETURNING `+activityVersionColumns,
			versionID, activityID, findingsRaw, newStatus,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg SetValidationOutcome failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// TransitionActivity performs one atomic, WHERE-guarded status
// transition. The caller (handler) has already checked
// domain.ValidActivityTransition, but the WHERE clause is the real
// concurrency guard — two simultaneous callers racing the same
// transition can only ever have one succeed.
func (s *PgStore) TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET version_status = $4
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = $3
			RETURNING `+activityVersionColumns,
			versionID, activityID, from, to,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, findErr := s.FindActivityVersion(ctx, activityID, versionID)
		if findErr == nil && existing != nil {
			return nil, domain.ErrInvalidTransition
		}
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg TransitionActivity failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) RejectActivity(ctx context.Context, activityID, versionID, reason string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET version_status = 'REJECTED', rejection_reason = $3
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'SUBMITTED'
			RETURNING `+activityVersionColumns,
			versionID, activityID, reason,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, findErr := s.FindActivityVersion(ctx, activityID, versionID)
		if findErr == nil && existing != nil {
			return nil, domain.ErrInvalidTransition
		}
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg RejectActivity failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ActivateActivity(ctx context.Context, activityID, versionID string, effectiveFrom time.Time) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET version_status = 'ACTIVE', effective_from = $3
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'APPROVED'
			RETURNING `+activityVersionColumns,
			versionID, activityID, effectiveFrom,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, findErr := s.FindActivityVersion(ctx, activityID, versionID)
		if findErr == nil && existing != nil {
			return nil, domain.ErrInvalidTransition
		}
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg ActivateActivity failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}
