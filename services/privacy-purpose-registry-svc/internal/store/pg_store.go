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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/events"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02) — a caller-supplied ID
// that is not even syntactically a UUID. Without this check, that error
// was indistinguishable from a real outage: it surfaced as the same
// ErrStoreUnavailable/503 as a genuine connection failure, when the
// correct, cheap answer is "not found" — the same answer a syntactically
// valid but nonexistent UUID already gets.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isUniqueViolation reports SQLSTATE 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Store is the interface the handler depends on — narrow enough to be
// stubbed in tests without a real database.
type Store interface {
	CreatePurpose(ctx context.Context, tenantID string, req domain.CreatePurposeRequest, principalID string) (*domain.Purpose, *domain.PurposeVersion, error)
	CreatePurposeVersion(ctx context.Context, purposeID string, req domain.CreatePurposeVersionRequest, principalID string) (*domain.PurposeVersion, error)
	PublishPurposeVersion(ctx context.Context, purposeID, versionID, principalID, correlationID string) (*domain.PurposeVersion, error)
	FindPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error)
	// ResolvePurposeAsOf resolves the PUBLISHED version in force at asOf
	// (valid time) that the platform had already published by knownAsOf
	// (knowledge time) — §20.3.
	ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf, knownAsOf time.Time) (*domain.PurposeVersion, error)
	ListPurposes(ctx context.Context) ([]domain.PurposeVersion, error)
	// IsPurposePublished reports whether purposeID resolves (in the
	// caller's tenant scope, platform-wide included) to a currently
	// PUBLISHED version — used by Validate to enforce PRV-001
	// (PURPOSE_NOT_REGISTERED) without exposing full purpose content.
	IsPurposePublished(ctx context.Context, purposeID string) (bool, error)

	CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error)
	CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error)
	FindActivityVersion(ctx context.Context, activityID, versionID string) (*domain.ProcessingActivityVersion, error)
	FindLatestActivityVersion(ctx context.Context, activityID string) (*domain.ProcessingActivityVersion, error)
	// ResolveActivityAsOf resolves the version that governed the activity at
	// asOf (valid time: effective_from <= asOf < effective_to) and that the
	// platform had already approved by knownAsOf (knowledge time) — §9.1/§20.3.
	ResolveActivityAsOf(ctx context.Context, activityID string, asOf, knownAsOf time.Time) (*domain.ProcessingActivityVersion, error)
	ListActiveActivities(ctx context.Context, role, jurisdiction string) ([]domain.ProcessingActivityVersion, error)

	SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding, principalID, correlationID string) (*domain.ProcessingActivityVersion, error)
	TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus, principalID, correlationID string) (*domain.ProcessingActivityVersion, error)
	RejectActivity(ctx context.Context, activityID, versionID, reason, principalID, correlationID string) (*domain.ProcessingActivityVersion, error)
	// ActivateActivity activates an APPROVED version at the server's
	// activation instant, atomically retiring every previously ACTIVE or
	// SUSPENDED version of the same activity (with its own retired event and
	// audit row). The caller cannot choose the effective time.
	ActivateActivity(ctx context.Context, activityID, versionID, principalID, correlationID string) (*domain.ProcessingActivityVersion, error)

	GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error)
	// RunIdempotent claims the idempotency key, runs fn and stores the
	// replayable response in ONE transaction: every store call fn makes joins
	// that transaction (see withTenant), so the change, its audit row, its
	// outbox event and the idempotency record commit together or not at all,
	// and concurrent identical requests execute exactly once. When the key
	// was already claimed it runs nothing and returns the stored record.
	RunIdempotent(ctx context.Context, claim domain.IdempotencyRecord, fn func(ctx context.Context) (status int, payload any, err error)) (existing *domain.IdempotencyRecord, status int, payload any, err error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withTenant runs fn inside a transaction with the RLS session variables set
// from the request context — transaction-local, never session-wide on a
// pooled connection:
//
//	app.tenant_id       the verified X-Tenant-Id ("" when none was sent)
//	app.platform_scope  'true' ONLY when the request explicitly carried the
//	                    platform-scope sentinel (middleware.IsPlatformScope)
//
// The write policies of migration 000006 require a matching tenant OR the
// platform capability, so a request with no tenant context can write
// nothing, and a platform context can write no tenant's row.
// ambientTxKey carries the transaction opened by RunIdempotent so the store
// calls made inside it join it instead of opening their own. The RLS session
// variables are identical (same request context), so they are already set.
type ambientTxKey struct{}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	if tx, ok := ctx.Value(ambientTxKey{}).(pgx.Tx); ok {
		return fn(tx) // commit/rollback belongs to RunIdempotent
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	platform := "false"
	if middleware.IsPlatformScope(ctx) {
		platform = "true"
	}
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true), set_config('app.platform_scope', $2, true)",
		middleware.TenantFromContext(ctx), platform); err != nil {
		return fmt.Errorf("set rls session variables: %w", err)
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

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	if *dest == nil {
		*dest = []string{}
	}
}

// ── purposes ─────────────────────────────────────────────────────────────────

const purposeVersionColumns = `
	purpose_version_id, purpose_id, statement, compatibility_class, lawful_basis_refs,
	necessity_rationale, special_condition_refs,
	version_status, effective_from, supersedes_version_id, published_by_principal_id, published_at,
	created_at, created_by_principal_id`

const purposeVersionColumnsJoined = `
	pv.purpose_version_id, pv.purpose_id, p.tenant_id, pv.statement, pv.compatibility_class, pv.lawful_basis_refs,
	pv.necessity_rationale, pv.special_condition_refs,
	pv.version_status, pv.effective_from, pv.supersedes_version_id, pv.published_by_principal_id, pv.published_at,
	pv.created_at, pv.created_by_principal_id`

func scanPurposeVersion(row pgx.Row) (*domain.PurposeVersion, error) {
	v := &domain.PurposeVersion{}
	var basisRaw, specialRaw []byte
	var rationale *string
	err := row.Scan(
		&v.PurposeVersionID, &v.PurposeID, &v.Statement, &v.CompatibilityClass, &basisRaw,
		&rationale, &specialRaw,
		&v.VersionStatus, &v.EffectiveFrom, &v.SupersedesVersionID,
		&v.PublishedByPrincipalID, &v.PublishedAt,
		&v.CreatedAt, &v.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	v.NecessityRationale = derefStr(rationale)
	unmarshalSlice(basisRaw, &v.LawfulBasisRefs)
	unmarshalSlice(specialRaw, &v.SpecialConditionRefs)
	return v, nil
}

func scanPurposeVersionJoined(row pgx.Row) (*domain.PurposeVersion, error) {
	v := &domain.PurposeVersion{}
	var basisRaw, specialRaw []byte
	var rationale, tID *string
	err := row.Scan(
		&v.PurposeVersionID, &v.PurposeID, &tID, &v.Statement, &v.CompatibilityClass, &basisRaw,
		&rationale, &specialRaw,
		&v.VersionStatus, &v.EffectiveFrom, &v.SupersedesVersionID,
		&v.PublishedByPrincipalID, &v.PublishedAt,
		&v.CreatedAt, &v.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	v.TenantID = tID
	v.NecessityRationale = derefStr(rationale)
	unmarshalSlice(basisRaw, &v.LawfulBasisRefs)
	unmarshalSlice(specialRaw, &v.SpecialConditionRefs)
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
	specialRaw := marshalSlice(req.SpecialConditionRefs)

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
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8, NULL, NULL, NULL, NOW(), $9)
			RETURNING `+purposeVersionColumns,
			versionID, purposeID, req.Statement, req.CompatibilityClass, basisRaw,
			strPtrOrNil(req.NecessityRationale), specialRaw, effectiveFrom, principalID,
		))
		if err != nil {
			return err
		}
		version.TenantID = purpose.TenantID
		return nil
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
	specialRaw := marshalSlice(req.SpecialConditionRefs)

	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `
			SELECT p.tenant_id FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.purpose_version_id = $1 AND pv.purpose_id = $2`,
			req.ParentVersionID, purposeID,
		).Scan(&parentTenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPurposeVersionNotFound
			}
			return err
		}

		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			INSERT INTO purpose_versions (`+purposeVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8, $9, NULL, NULL, NOW(), $10)
			RETURNING `+purposeVersionColumns,
			versionID, purposeID, req.Statement, req.CompatibilityClass, basisRaw,
			strPtrOrNil(req.NecessityRationale), specialRaw, effectiveFrom, req.ParentVersionID, principalID,
		))
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID
		return nil
	})
	if errors.Is(err, domain.ErrPurposeVersionNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg CreatePurposeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// PublishPurposeVersion publishes a DRAFT purpose version. effective_from is
// clamped to the publish instant when it would otherwise precede it: a
// draft created an hour ago must not become effective "an hour ago" — that
// would rewrite what the platform knew when (§20.3). A deliberately
// future-dated effective_from is preserved.
func (s *PgStore) PublishPurposeVersion(ctx context.Context, purposeID, versionID, principalID, correlationID string) (*domain.PurposeVersion, error) {
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM purposes WHERE purpose_id = $1 FOR UPDATE`, purposeID).Scan(&parentTenantID); err != nil {
			return err
		}

		var err error
		version, err = scanPurposeVersion(tx.QueryRow(ctx, `
			UPDATE purpose_versions
			SET version_status = 'PUBLISHED',
			    effective_from = GREATEST(effective_from, NOW()),
			    published_by_principal_id = $3,
			    published_at = NOW()
			WHERE purpose_version_id = $1 AND purpose_id = $2 AND version_status = 'DRAFT'
			RETURNING `+purposeVersionColumns,
			versionID, purposeID, principalID,
		))
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID

		// Enqueue to transactional outbox (§19.1) — the ONLY event path.
		return events.EnqueueTx(ctx, tx, events.PublishParams{
			EventType:     "privacy.purpose.published",
			EntityID:      version.PurposeID,
			TenantID:      derefStr(parentTenantID),
			ActorID:       principalID,
			CorrelationID: correlationID,
			Payload:       version,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
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
		version, err = scanPurposeVersionJoined(tx.QueryRow(ctx, `
			SELECT `+purposeVersionColumnsJoined+`
			FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.purpose_version_id = $1 AND pv.purpose_id = $2`,
			versionID, purposeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindPurposeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf, knownAsOf time.Time) (*domain.PurposeVersion, error) {
	var version *domain.PurposeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanPurposeVersionJoined(tx.QueryRow(ctx, `
			SELECT `+purposeVersionColumnsJoined+`
			FROM purpose_versions pv
			JOIN purposes p ON p.purpose_id = pv.purpose_id
			WHERE pv.purpose_id = $1 AND pv.version_status = 'PUBLISHED'
			  AND pv.effective_from <= $2
			  AND COALESCE(pv.published_at, pv.effective_from) <= $3
			ORDER BY pv.effective_from DESC, pv.sequence_no DESC
			LIMIT 1`,
			purposeID, asOf, knownAsOf,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
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
			v, err := scanPurposeVersionJoined(rows)
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
	if isInvalidUUID(err) {
		return false, nil
	}
	if err != nil {
		s.log.Error("pg IsPurposePublished failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return exists, nil
}

// ── processing activities ────────────────────────────────────────────────────

const activityVersionColumns = `
	activity_version_id, activity_id, privacy_role, owner, privacy_owner, purpose_ids, subject_classes,
	data_categories, sources, recipients, jurisdictions, retention_rule_refs, transfer_refs, processor_refs,
	notice_consent_dependency, dpia_tia_status,
	version_status, validation_findings, rejection_reason, effective_from, effective_to, supersedes_version_id,
	approved_by_principal_id, approved_at, rejected_by_principal_id, rejected_at,
	activated_by_principal_id, activated_at,
	created_at, created_by_principal_id`

const activityVersionColumnsJoined = `
	av.activity_version_id, av.activity_id, a.tenant_id, av.privacy_role, av.owner, av.privacy_owner, av.purpose_ids, av.subject_classes,
	av.data_categories, av.sources, av.recipients, av.jurisdictions, av.retention_rule_refs, av.transfer_refs, av.processor_refs,
	av.notice_consent_dependency, av.dpia_tia_status,
	av.version_status, av.validation_findings, av.rejection_reason, av.effective_from, av.effective_to, av.supersedes_version_id,
	av.approved_by_principal_id, av.approved_at, av.rejected_by_principal_id, av.rejected_at,
	av.activated_by_principal_id, av.activated_at,
	av.created_at, av.created_by_principal_id`

// activityScan collects the raw columns shared by the plain and joined scans.
type activityScan struct {
	purposeIDs, subjectClasses, dataCategories, sources, recipients, jurisdictions []byte
	retentionRuleRefs, transferRefs, processorRefs, findingsRaw                    []byte
	noticeConsent, dpiaTia, privacyOwner                                           *string
}

func (a *activityScan) finish(v *domain.ProcessingActivityVersion) {
	v.PrivacyOwner = derefStr(a.privacyOwner)
	if a.noticeConsent != nil {
		v.NoticeConsentDependency = domain.NoticeConsentDependency(*a.noticeConsent)
	}
	if a.dpiaTia != nil {
		v.DPIATIAStatus = domain.DPIATIAStatus(*a.dpiaTia)
	}
	unmarshalSlice(a.purposeIDs, &v.PurposeIDs)
	unmarshalSlice(a.subjectClasses, &v.SubjectClasses)
	unmarshalSlice(a.dataCategories, &v.DataCategories)
	unmarshalSlice(a.sources, &v.Sources)
	unmarshalSlice(a.recipients, &v.Recipients)
	unmarshalSlice(a.jurisdictions, &v.Jurisdictions)
	unmarshalSlice(a.retentionRuleRefs, &v.RetentionRuleRefs)
	unmarshalSlice(a.transferRefs, &v.TransferRefs)
	unmarshalSlice(a.processorRefs, &v.ProcessorRefs)
	if len(a.findingsRaw) > 0 {
		_ = json.Unmarshal(a.findingsRaw, &v.ValidationFindings)
	}
}

func scanActivityVersion(row pgx.Row) (*domain.ProcessingActivityVersion, error) {
	v := &domain.ProcessingActivityVersion{}
	var a activityScan
	err := row.Scan(
		&v.ActivityVersionID, &v.ActivityID, &v.PrivacyRole, &v.Owner, &a.privacyOwner,
		&a.purposeIDs, &a.subjectClasses, &a.dataCategories, &a.sources, &a.recipients, &a.jurisdictions,
		&a.retentionRuleRefs, &a.transferRefs, &a.processorRefs,
		&a.noticeConsent, &a.dpiaTia,
		&v.VersionStatus, &a.findingsRaw, &v.RejectionReason, &v.EffectiveFrom, &v.EffectiveTo, &v.SupersedesVersionID,
		&v.ApprovedByPrincipalID, &v.ApprovedAt,
		&v.RejectedByPrincipalID, &v.RejectedAt,
		&v.ActivatedByPrincipalID, &v.ActivatedAt,
		&v.CreatedAt, &v.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	a.finish(v)
	return v, nil
}

func scanActivityVersionJoined(row pgx.Row) (*domain.ProcessingActivityVersion, error) {
	v := &domain.ProcessingActivityVersion{}
	var a activityScan
	var tID *string
	err := row.Scan(
		&v.ActivityVersionID, &v.ActivityID, &tID, &v.PrivacyRole, &v.Owner, &a.privacyOwner,
		&a.purposeIDs, &a.subjectClasses, &a.dataCategories, &a.sources, &a.recipients, &a.jurisdictions,
		&a.retentionRuleRefs, &a.transferRefs, &a.processorRefs,
		&a.noticeConsent, &a.dpiaTia,
		&v.VersionStatus, &a.findingsRaw, &v.RejectionReason, &v.EffectiveFrom, &v.EffectiveTo, &v.SupersedesVersionID,
		&v.ApprovedByPrincipalID, &v.ApprovedAt,
		&v.RejectedByPrincipalID, &v.RejectedAt,
		&v.ActivatedByPrincipalID, &v.ActivatedAt,
		&v.CreatedAt, &v.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	v.TenantID = tID
	a.finish(v)
	return v, nil
}

// activityContent is the content half of an activity version, shared by the
// create-activity and create-successor-version paths.
type activityContent struct {
	privacyRole, owner, privacyOwner                                               string
	purposeIDs, subjectClasses, dataCategories, sources, recipients, jurisdictions []string
	retentionRuleRefs, transferRefs, processorRefs                                 []string
	noticeConsentDependency, dpiaTIAStatus                                         string
}

// insertActivityVersion inserts a DRAFT version and returns it. parentID is
// the explicit parent for a successor ("" for an activity's first version).
func insertActivityVersion(ctx context.Context, tx pgx.Tx, versionID, activityID, parentID, principalID string, c activityContent) (*domain.ProcessingActivityVersion, error) {
	return scanActivityVersion(tx.QueryRow(ctx, `
		INSERT INTO processing_activity_versions (`+activityVersionColumns+`)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
			'DRAFT', NULL, NULL, NULL, NULL, $17,
			NULL, NULL, NULL, NULL, NULL, NULL,
			NOW(), $18
		)
		RETURNING `+activityVersionColumns,
		versionID, activityID, c.privacyRole, c.owner, strPtrOrNil(c.privacyOwner),
		marshalSlice(c.purposeIDs), marshalSlice(c.subjectClasses), marshalSlice(c.dataCategories),
		marshalSlice(c.sources), marshalSlice(c.recipients), marshalSlice(c.jurisdictions),
		marshalSlice(c.retentionRuleRefs), marshalSlice(c.transferRefs), marshalSlice(c.processorRefs),
		strPtrOrNil(c.noticeConsentDependency), strPtrOrNil(c.dpiaTIAStatus),
		strPtrOrNil(parentID), principalID,
	))
}

func (s *PgStore) CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error) {
	activityID := uuid.New().String()
	versionID := uuid.New().String()
	c := activityContent{
		privacyRole: req.PrivacyRole, owner: req.Owner, privacyOwner: req.PrivacyOwner,
		purposeIDs: req.PurposeIDs, subjectClasses: req.SubjectClasses, dataCategories: req.DataCategories,
		sources: req.Sources, recipients: req.Recipients, jurisdictions: req.Jurisdictions,
		retentionRuleRefs: req.RetentionRuleRefs, transferRefs: req.TransferRefs, processorRefs: req.ProcessorRefs,
		noticeConsentDependency: req.NoticeConsentDependency, dpiaTIAStatus: req.DPIATIAStatus,
	}

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
		version, err = insertActivityVersion(ctx, tx, versionID, activityID, "", principalID, c)
		if err != nil {
			return err
		}
		version.TenantID = activity.TenantID
		return nil
	})
	if err != nil {
		s.log.Error("pg CreateActivity failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &activity, version, nil
}

func (s *PgStore) CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error) {
	versionID := uuid.New().String()
	c := activityContent{
		privacyRole: req.PrivacyRole, owner: req.Owner, privacyOwner: req.PrivacyOwner,
		purposeIDs: req.PurposeIDs, subjectClasses: req.SubjectClasses, dataCategories: req.DataCategories,
		sources: req.Sources, recipients: req.Recipients, jurisdictions: req.Jurisdictions,
		retentionRuleRefs: req.RetentionRuleRefs, transferRefs: req.TransferRefs, processorRefs: req.ProcessorRefs,
		noticeConsentDependency: req.NoticeConsentDependency, dpiaTIAStatus: req.DPIATIAStatus,
	}

	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `
			SELECT a.tenant_id FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_version_id = $1 AND av.activity_id = $2`,
			req.ParentVersionID, activityID,
		).Scan(&parentTenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrActivityVersionNotFound
			}
			return err
		}

		var err error
		version, err = insertActivityVersion(ctx, tx, versionID, activityID, req.ParentVersionID, principalID, c)
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID
		return nil
	})
	if errors.Is(err, domain.ErrActivityVersionNotFound) || isInvalidUUID(err) {
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
		version, err = scanActivityVersionJoined(tx.QueryRow(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_version_id = $1 AND av.activity_id = $2`,
			versionID, activityID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindActivityVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// statusAt is the version's status at the requested instants, so a version
// that is SUSPENDED today is not reported as suspended for an instant before the
// suspension (or after a resume). Suspend and resume are recorded as SUSPEND /
// RESUME rows in the append-only activity_lifecycle_audit; the service stamps
// recorded_at itself, so it is both the valid-time and the knowledge-time of
// the change and a suspension counts only when recorded_at <= as_of AND
// recorded_at <= known_as_of. Intervals are half-open: a version is SUSPENDED
// from its suspend instant up to (excluding) its resume instant.
//
//   - Both instants at or after the database clock ("what is true now"): the
//     stored status, so current reads are unchanged.
//   - Otherwise the last SUSPEND/RESUME at or before both instants decides;
//     with none, the version was ACTIVE then (it governs only inside
//     [effective_from, effective_to)).
//   - A SUSPENDED version with no lifecycle row at all (suspended before the
//     audit table existed) cannot be time-sliced and stays SUSPENDED.
func (s *PgStore) statusAt(ctx context.Context, tx pgx.Tx, v *domain.ProcessingActivityVersion, asOf, knownAsOf time.Time) (domain.ActivityVersionStatus, error) {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT CASE
		  WHEN $2 >= NOW() AND $3 >= NOW() AND $4 <> 'RETIRED' THEN $4
		  ELSE COALESCE(
		    (SELECT CASE action WHEN 'SUSPEND' THEN 'SUSPENDED' ELSE 'ACTIVE' END
		       FROM activity_lifecycle_audit
		      WHERE activity_version_id = $1
		        AND action IN ('SUSPEND', 'RESUME')
		        AND recorded_at <= $2 AND recorded_at <= $3
		      ORDER BY recorded_at DESC
		      LIMIT 1),
		    CASE WHEN $4 = 'SUSPENDED'
		          AND NOT EXISTS (SELECT 1 FROM activity_lifecycle_audit
		                           WHERE activity_version_id = $1 AND action IN ('SUSPEND', 'RESUME'))
		         THEN 'SUSPENDED' ELSE 'ACTIVE' END)
		END`,
		v.ActivityVersionID, asOf, knownAsOf, string(v.VersionStatus),
	).Scan(&status)
	if err != nil {
		return "", err
	}
	return domain.ActivityVersionStatus(status), nil
}

func (s *PgStore) ResolveActivityAsOf(ctx context.Context, activityID string, asOf, knownAsOf time.Time) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersionJoined(tx.QueryRow(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_id = $1
			  AND av.version_status IN ('ACTIVE', 'SUSPENDED', 'RETIRED')
			  AND av.effective_from <= $2
			  AND (av.effective_to IS NULL OR av.effective_to > $2)
			  AND COALESCE(av.approved_at, av.effective_from) <= $3
			ORDER BY av.effective_from DESC, av.sequence_no DESC
			LIMIT 1`,
			activityID, asOf, knownAsOf,
		))
		if err != nil {
			return err
		}
		version.VersionStatus, err = s.statusAt(ctx, tx, version, asOf, knownAsOf)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrActivityNotFound
	}
	if err != nil {
		s.log.Error("pg ResolveActivityAsOf failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ListActiveActivities(ctx context.Context, role, jurisdiction string) ([]domain.ProcessingActivityVersion, error) {
	var out []domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.version_status = 'ACTIVE'
			  AND av.effective_from <= NOW()
			  AND ($1 = '' OR av.privacy_role = $1)
			  AND ($2 = '' OR av.jurisdictions @> to_jsonb($2::text))
			ORDER BY av.effective_from DESC`,
			role, jurisdiction)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanActivityVersionJoined(rows)
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

// ── lifecycle: audit evidence + the single event path ────────────────────────

// lifecycle describes one committed state change for recordLifecycle.
type lifecycle struct {
	eventType    string // "" = audit evidence only, no event
	action       string // VALIDATE | SUBMIT | APPROVE | REJECT | ACTIVATE | SUSPEND | RESUME | RETIRE | SUPERSEDE
	from         domain.ActivityVersionStatus
	actor        string
	correlation  string
	reason       string
	supersededBy *string
}

// recordLifecycle writes, inside the caller's transaction, BOTH the durable
// audit row (activity_lifecycle_audit — required actor evidence that does not
// depend on a deliverable event) AND, when lc.eventType is set, the outbox
// event. These are the only two side effects of a lifecycle change: nothing
// publishes after commit, so a committed change yields exactly one event and
// a rolled-back one yields none. v.TenantID must already be set.
func (s *PgStore) recordLifecycle(ctx context.Context, tx pgx.Tx, v *domain.ProcessingActivityVersion, lc lifecycle) error {
	tenant := derefStr(v.TenantID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO activity_lifecycle_audit (
			tenant_id, activity_id, activity_version_id, action, from_status, to_status,
			actor_principal_id, correlation_id, reason, superseded_by_version_id,
			effective_from, effective_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		strPtrOrNil(tenant), v.ActivityID, v.ActivityVersionID, lc.action, strPtrOrNil(string(lc.from)), string(v.VersionStatus),
		lc.actor, lc.correlation, strPtrOrNil(lc.reason), lc.supersededBy,
		v.EffectiveFrom, v.EffectiveTo,
	); err != nil {
		return fmt.Errorf("record lifecycle audit: %w", err)
	}
	if lc.eventType == "" {
		return nil
	}
	refs, err := s.policyRefs(ctx, tx, v)
	if err != nil {
		return err
	}
	return events.EnqueueTx(ctx, tx, events.PublishParams{
		EventType:     lc.eventType,
		EntityID:      v.ActivityID,
		TenantID:      tenant,
		ActorID:       lc.actor,
		CorrelationID: lc.correlation,
		Payload: domain.ActivityEventPayload{
			ProcessingActivityVersion: *v,
			PreviousStatus:            lc.from,
			PolicyRefs:                refs,
			SupersededByVersionID:     lc.supersededBy,
		},
	})
}

// policyRefs collects the §19 "policy refs" for an activity version: the
// lawful-basis and special-condition references of its linked published
// purposes, plus the version's own retention, transfer and processor refs.
func (s *PgStore) policyRefs(ctx context.Context, tx pgx.Tx, v *domain.ProcessingActivityVersion) ([]string, error) {
	refs := []string{}
	seen := map[string]bool{}
	add := func(xs []string) {
		for _, x := range xs {
			if x != "" && !seen[x] {
				seen[x] = true
				refs = append(refs, x)
			}
		}
	}

	// Only syntactically valid UUIDs may reach the query: a malformed id would
	// abort the whole transaction (SQLSTATE 22P02).
	ids := make([]string, 0, len(v.PurposeIDs))
	for _, id := range v.PurposeIDs {
		if _, err := uuid.Parse(id); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (pv.purpose_id) pv.lawful_basis_refs, pv.special_condition_refs
			FROM purpose_versions pv
			WHERE pv.purpose_id::text = ANY($1::text[])
			  AND pv.version_status = 'PUBLISHED' AND pv.effective_from <= NOW()
			ORDER BY pv.purpose_id, pv.effective_from DESC, pv.sequence_no DESC`, ids)
		if err != nil {
			return nil, fmt.Errorf("resolve policy refs: %w", err)
		}
		for rows.Next() {
			var lawfulRaw, specialRaw []byte
			if err := rows.Scan(&lawfulRaw, &specialRaw); err != nil {
				rows.Close()
				return nil, err
			}
			var lawful, special []string
			unmarshalSlice(lawfulRaw, &lawful)
			unmarshalSlice(specialRaw, &special)
			add(lawful)
			add(special)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	add(v.RetentionRuleRefs)
	add(v.TransferRefs)
	add(v.ProcessorRefs)
	return refs, nil
}

func (s *PgStore) SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	if findings == nil {
		findings = []domain.ValidationFinding{}
	}
	findingsRaw, err := json.Marshal(findings)
	if err != nil {
		return nil, err
	}
	newStatus := domain.ActivityStatusValidated
	if len(findings) > 0 {
		newStatus = domain.ActivityStatusDraft
	}

	var version *domain.ProcessingActivityVersion
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processing_activities WHERE activity_id = $1`, activityID).Scan(&parentTenantID); err != nil {
			return err
		}

		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET validation_findings = $3, version_status = $4
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'DRAFT'
			RETURNING `+activityVersionColumns,
			versionID, activityID, findingsRaw, newStatus,
		))
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID

		// A passing validation is a lifecycle fact (DRAFT -> VALIDATED) and is
		// audited; a failing one leaves the version DRAFT and changes nothing
		// but the findings.
		if newStatus == domain.ActivityStatusValidated {
			return s.recordLifecycle(ctx, tx, version, lifecycle{
				action: "VALIDATE", from: domain.ActivityStatusDraft, actor: principalID, correlation: correlationID,
			})
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrActivityVersionNotFound
	}
	if err != nil {
		s.log.Error("pg SetValidationOutcome failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// transitionMeta maps a target status to its audit action and event type.
func transitionMeta(to domain.ActivityVersionStatus) (action, eventType string) {
	switch to {
	case domain.ActivityStatusSubmitted:
		return "SUBMIT", "privacy.processing_activity.submitted"
	case domain.ActivityStatusApproved:
		return "APPROVE", "privacy.processing_activity.approved"
	case domain.ActivityStatusSuspended:
		return "SUSPEND", "privacy.processing_activity.suspended"
	case domain.ActivityStatusActive:
		return "RESUME", "privacy.processing_activity.resumed"
	case domain.ActivityStatusRetired:
		return "RETIRE", "privacy.processing_activity.retired"
	}
	return "", ""
}

func (s *PgStore) TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processing_activities WHERE activity_id = $1 FOR UPDATE`, activityID).Scan(&parentTenantID); err != nil {
			return err
		}

		var err error
		switch to {
		case domain.ActivityStatusApproved:
			version, err = scanActivityVersion(tx.QueryRow(ctx, `
				UPDATE processing_activity_versions
				SET version_status = $4, approved_by_principal_id = $5, approved_at = NOW()
				WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = $3
				RETURNING `+activityVersionColumns,
				versionID, activityID, from, to, principalID,
			))
		case domain.ActivityStatusRetired:
			// Retiring closes the valid-time window at the same instant.
			version, err = scanActivityVersion(tx.QueryRow(ctx, `
				UPDATE processing_activity_versions
				SET version_status = $4, effective_to = NOW()
				WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = $3
				RETURNING `+activityVersionColumns,
				versionID, activityID, from, to,
			))
		default:
			version, err = scanActivityVersion(tx.QueryRow(ctx, `
				UPDATE processing_activity_versions
				SET version_status = $4
				WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = $3
				RETURNING `+activityVersionColumns,
				versionID, activityID, from, to,
			))
		}
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID

		action, eventType := transitionMeta(to)
		if action == "" {
			return nil
		}
		return s.recordLifecycle(ctx, tx, version, lifecycle{
			eventType: eventType, action: action, from: from, actor: principalID, correlation: correlationID,
		})
	})
	if isUniqueViolation(err) {
		// Resuming a SUSPENDED version while another version of the activity
		// is ACTIVE: the one-ACTIVE index refused it. A state conflict, not
		// an outage.
		return nil, domain.ErrInvalidTransition
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
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

func (s *PgStore) RejectActivity(ctx context.Context, activityID, versionID, reason, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processing_activities WHERE activity_id = $1 FOR UPDATE`, activityID).Scan(&parentTenantID); err != nil {
			return err
		}

		var err error
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET version_status = 'REJECTED',
			    rejection_reason = $3,
			    rejected_by_principal_id = $4,
			    rejected_at = NOW()
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'SUBMITTED'
			RETURNING `+activityVersionColumns,
			versionID, activityID, reason, principalID,
		))
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID

		return s.recordLifecycle(ctx, tx, version, lifecycle{
			eventType: "privacy.processing_activity.rejected", action: "REJECT", from: domain.ActivityStatusSubmitted,
			actor: principalID, correlation: correlationID, reason: reason,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
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

func (s *PgStore) ActivateActivity(ctx context.Context, activityID, versionID, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		// 1. Serialise every activation of this activity on its identity row.
		var parentTenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processing_activities WHERE activity_id = $1 FOR UPDATE`, activityID).Scan(&parentTenantID); err != nil {
			return err
		}

		// 2. The target must be APPROVED. Checked BEFORE retiring anything, so
		// a request for a version that is not activatable can never retire the
		// version that is currently governing.
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT version_status FROM processing_activity_versions
			WHERE activity_version_id = $1 AND activity_id = $2 FOR UPDATE`,
			versionID, activityID).Scan(&status); err != nil {
			return err
		}
		if domain.ActivityVersionStatus(status) != domain.ActivityStatusApproved {
			return domain.ErrInvalidTransition
		}

		// 3. Retire every version that currently governs or is suspended. A
		// SUSPENDED sibling must go too: it could otherwise be resumed later and
		// collide with the new ACTIVE version. Each one is closed at the same
		// transaction instant the new version opens (no gap, no overlap) and
		// announces its own retired event + audit row.
		type prior struct {
			id     string
			status domain.ActivityVersionStatus
		}
		var priors []prior
		rows, err := tx.Query(ctx, `
			SELECT activity_version_id, version_status FROM processing_activity_versions
			WHERE activity_id = $1 AND version_status IN ('ACTIVE', 'SUSPENDED') AND activity_version_id <> $2
			ORDER BY sequence_no FOR UPDATE`, activityID, versionID)
		if err != nil {
			return fmt.Errorf("select superseded versions: %w", err)
		}
		for rows.Next() {
			var p prior
			var st string
			if err := rows.Scan(&p.id, &st); err != nil {
				rows.Close()
				return err
			}
			p.status = domain.ActivityVersionStatus(st)
			priors = append(priors, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range priors {
			retired, err := scanActivityVersion(tx.QueryRow(ctx, `
				UPDATE processing_activity_versions
				SET version_status = 'RETIRED', effective_to = NOW()
				WHERE activity_version_id = $1 AND activity_id = $2
				RETURNING `+activityVersionColumns, p.id, activityID))
			if err != nil {
				return fmt.Errorf("retire superseded version: %w", err)
			}
			retired.TenantID = parentTenantID
			by := versionID
			if err := s.recordLifecycle(ctx, tx, retired, lifecycle{
				eventType: "privacy.processing_activity.retired", action: "SUPERSEDE", from: p.status,
				actor: principalID, correlation: correlationID, supersededBy: &by,
			}); err != nil {
				return err
			}
		}

		// 4. Activate the target at the server's instant, with actor evidence.
		version, err = scanActivityVersion(tx.QueryRow(ctx, `
			UPDATE processing_activity_versions
			SET version_status = 'ACTIVE',
			    effective_from = NOW(),
			    effective_to = NULL,
			    activated_by_principal_id = $3,
			    activated_at = NOW()
			WHERE activity_version_id = $1 AND activity_id = $2 AND version_status = 'APPROVED'
			RETURNING `+activityVersionColumns,
			versionID, activityID, principalID,
		))
		if err != nil {
			return err
		}
		version.TenantID = parentTenantID

		return s.recordLifecycle(ctx, tx, version, lifecycle{
			eventType: "privacy.processing_activity.activated", action: "ACTIVATE", from: domain.ActivityStatusApproved,
			actor: principalID, correlation: correlationID,
		})
	})
	if errors.Is(err, domain.ErrInvalidTransition) {
		return nil, domain.ErrInvalidTransition
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
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

func (s *PgStore) FindLatestActivityVersion(ctx context.Context, activityID string) (*domain.ProcessingActivityVersion, error) {
	var version *domain.ProcessingActivityVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanActivityVersionJoined(tx.QueryRow(ctx, `
			SELECT `+activityVersionColumnsJoined+`
			FROM processing_activity_versions av
			JOIN processing_activities a ON a.activity_id = av.activity_id
			WHERE av.activity_id = $1
			ORDER BY av.created_at DESC, av.sequence_no DESC
			LIMIT 1`,
			activityID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrActivityNotFound
	}
	if err != nil {
		s.log.Error("pg FindLatestActivityVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tID *string
		return tx.QueryRow(ctx, `
			SELECT idempotency_key, tenant_id, principal_id, operation, request_hash, response_status, response_body, created_at
			FROM purpose_registry_idempotency_keys
			WHERE COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE($1::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
			  AND principal_id = $2 AND idempotency_key = $3`,
			strPtrOrNil(tenantID), principalID, key,
		).Scan(&rec.IdempotencyKey, &tID, &rec.PrincipalID, &rec.Operation, &rec.RequestHash, &rec.ResponseStatus, &rec.ResponseBody, &rec.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &rec, nil
}

var errReplay = errors.New("idempotent replay")

func (s *PgStore) RunIdempotent(ctx context.Context, claim domain.IdempotencyRecord, fn func(ctx context.Context) (int, any, error)) (*domain.IdempotencyRecord, int, any, error) {
	var existing *domain.IdempotencyRecord
	var status int
	var payload any
	var opErr error
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO purpose_registry_idempotency_keys
				(idempotency_key, tenant_id, principal_id, operation, request_hash, response_status, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, 'null'::jsonb, NOW())
			ON CONFLICT (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), principal_id, idempotency_key)
			DO NOTHING`,
			claim.IdempotencyKey, claim.TenantID, claim.PrincipalID, claim.Operation, claim.RequestHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var rec domain.IdempotencyRecord
			var tID *string
			if err := tx.QueryRow(ctx, `
				SELECT idempotency_key, tenant_id, principal_id, operation, request_hash, response_status, response_body, created_at
				FROM purpose_registry_idempotency_keys
				WHERE COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE($1::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
				  AND principal_id = $2 AND idempotency_key = $3`,
				claim.TenantID, claim.PrincipalID, claim.IdempotencyKey,
			).Scan(&rec.IdempotencyKey, &tID, &rec.PrincipalID, &rec.Operation, &rec.RequestHash, &rec.ResponseStatus, &rec.ResponseBody, &rec.CreatedAt); err != nil {
				return err
			}
			existing = &rec
			return errReplay
		}
		st, v, err := fn(context.WithValue(ctx, ambientTxKey{}, tx))
		if err != nil {
			opErr = err
			return err // roll back the claim and every change made by fn
		}
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE purpose_registry_idempotency_keys SET response_status = $1, response_body = $2
			WHERE COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) = COALESCE($3::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
			  AND principal_id = $4 AND idempotency_key = $5`,
			st, body, claim.TenantID, claim.PrincipalID, claim.IdempotencyKey); err != nil {
			return err
		}
		status, payload = st, v
		return nil
	})
	switch {
	case errors.Is(err, errReplay):
		return existing, 0, nil, nil
	case err == nil:
		return nil, status, payload, nil
	case opErr != nil:
		return nil, 0, nil, opErr // a domain error the handler maps (404/409/…)
	default:
		s.log.Error("pg RunIdempotent failed", zap.Error(err))
		return nil, 0, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
}
