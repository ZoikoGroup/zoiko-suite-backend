// Package store implements privacy-consent-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02) — see
// privacy-purpose-registry-svc's identical helper for the full
// rationale: found by live-stack testing, where a malformed caller-
// supplied ID was surfacing as ErrStoreUnavailable/503 (indistinguishable
// from a real outage) instead of the cheap, correct "not found" answer.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// Store is the interface the handler depends on.
type Store interface {
	CreateNotice(ctx context.Context, tenantID string, req domain.CreateNoticeRequest, principalID string) (*domain.Notice, *domain.NoticeVersion, error)
	CreateNoticeVersion(ctx context.Context, noticeID string, req domain.CreateNoticeVersionRequest, principalID string) (*domain.NoticeVersion, error)
	FindNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error)
	FindLatestNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error)
	ApproveNoticeVersion(ctx context.Context, noticeID, versionID, principalID string) (*domain.NoticeVersion, error)
	PublishNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error)
	WithdrawNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error)
	ResolveNoticeAsOf(ctx context.Context, noticeID string, asOf time.Time) (*domain.NoticeVersion, error)

	RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error)

	RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error)
	FindConsentReceipt(ctx context.Context, receiptID string) (*domain.ConsentReceipt, error)
	WithdrawConsent(ctx context.Context, receiptID, channel, principalID string) (*domain.WithdrawalReceipt, error)
	ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error)

	SetPreference(ctx context.Context, tenantID string, req domain.SetPreferenceRequest) (*domain.PreferenceAssertion, error)
	ResolvePreference(ctx context.Context, subjectRef, channelOrPurpose string) (*domain.PreferenceAssertion, error)

	GetIdempotency(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error)
	SaveIdempotency(ctx context.Context, rec domain.IdempotencyRecord) error
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

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

// ── notices ──────────────────────────────────────────────────────────────────

const noticeVersionColumns = `
	notice_version_id, notice_id, locale, audience, content_hash, version_status,
	effective_from, supersedes_version_id, approved_by_principal_id, created_at, created_by_principal_id`

const noticeVersionColumnsJoined = `
	nv.notice_version_id, nv.notice_id, nv.locale, nv.audience, nv.content_hash, nv.version_status,
	nv.effective_from, nv.supersedes_version_id, nv.approved_by_principal_id, nv.created_at, nv.created_by_principal_id`

func scanNoticeVersion(row pgx.Row) (*domain.NoticeVersion, error) {
	v := &domain.NoticeVersion{}
	err := row.Scan(&v.NoticeVersionID, &v.NoticeID, &v.Locale, &v.Audience, &v.ContentHash, &v.VersionStatus,
		&v.EffectiveFrom, &v.SupersedesVersionID, &v.ApprovedByPrincipalID, &v.CreatedAt, &v.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (s *PgStore) CreateNotice(ctx context.Context, tenantID string, req domain.CreateNoticeRequest, principalID string) (*domain.Notice, *domain.NoticeVersion, error) {
	noticeID := uuid.New().String()
	versionID := uuid.New().String()

	var notice domain.Notice
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO notices (notice_id, tenant_id, created_by_principal_id)
			VALUES ($1, $2, $3)
			RETURNING notice_id, tenant_id, created_at, created_by_principal_id`,
			noticeID, strPtrOrNil(tenantID), principalID,
		).Scan(&notice.NoticeID, &notice.TenantID, &notice.CreatedAt, &notice.CreatedByPrincipalID); err != nil {
			return err
		}

		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			INSERT INTO notice_versions (`+noticeVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', NULL, NULL, NULL, NOW(), $6)
			RETURNING `+noticeVersionColumns,
			versionID, noticeID, req.Locale, req.Audience, req.ContentHash, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateNotice failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &notice, version, nil
}

func (s *PgStore) CreateNoticeVersion(ctx context.Context, noticeID string, req domain.CreateNoticeVersionRequest, principalID string) (*domain.NoticeVersion, error) {
	versionID := uuid.New().String()
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM notice_versions nv
				JOIN notices n ON n.notice_id = nv.notice_id
				WHERE nv.notice_version_id = $1 AND nv.notice_id = $2
			)`, req.ParentVersionID, noticeID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrNoticeVersionNotFound
		}

		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			INSERT INTO notice_versions (`+noticeVersionColumns+`)
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', NULL, $6, NULL, NOW(), $7)
			RETURNING `+noticeVersionColumns,
			versionID, noticeID, req.Locale, req.Audience, req.ContentHash, req.ParentVersionID, principalID,
		))
		return err
	})
	if errors.Is(err, domain.ErrNoticeVersionNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg CreateNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) FindNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			SELECT `+noticeVersionColumnsJoined+`
			FROM notice_versions nv
			JOIN notices n ON n.notice_id = nv.notice_id
			WHERE nv.notice_version_id = $1 AND nv.notice_id = $2`,
			versionID, noticeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) FindLatestNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			SELECT `+noticeVersionColumnsJoined+`
			FROM notice_versions nv
			JOIN notices n ON n.notice_id = nv.notice_id
			WHERE nv.notice_id = $1
			ORDER BY nv.created_at DESC, nv.sequence_no DESC
			LIMIT 1`,
			noticeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg FindLatestNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ApproveNoticeVersion(ctx context.Context, noticeID, versionID, principalID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			UPDATE notice_versions
			SET version_status = 'APPROVED', approved_by_principal_id = $3
			WHERE notice_version_id = $1 AND notice_id = $2 AND version_status = 'DRAFT'
			RETURNING `+noticeVersionColumns,
			versionID, noticeID, principalID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidNoticeTransition
	}
	if err != nil {
		s.log.Error("pg ApproveNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// PublishNoticeVersion transitions APPROVED -> PUBLISHED, and — inside
// the SAME transaction — demotes whatever version was previously
// PUBLISHED for this notice_id to SUPERSEDED. This is the one place a
// "direct" transition in domain.noticeTransitions isn't the whole story:
// SUPERSEDED is a side effect of publishing a successor, never an action
// a caller takes directly.
func (s *PgStore) PublishNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE notice_versions
			SET version_status = 'SUPERSEDED'
			WHERE notice_id = $1 AND version_status = 'PUBLISHED'`,
			noticeID,
		); err != nil {
			return err
		}

		now := time.Now().UTC()
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			UPDATE notice_versions
			SET version_status = 'PUBLISHED', effective_from = $3
			WHERE notice_version_id = $1 AND notice_id = $2 AND version_status = 'APPROVED'
			RETURNING `+noticeVersionColumns,
			versionID, noticeID, now,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidNoticeTransition
	}
	if err != nil {
		s.log.Error("pg PublishNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) WithdrawNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			UPDATE notice_versions
			SET version_status = 'WITHDRAWN'
			WHERE notice_version_id = $1 AND notice_id = $2 AND version_status = 'PUBLISHED'
			RETURNING `+noticeVersionColumns,
			versionID, noticeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidNoticeTransition
	}
	if err != nil {
		s.log.Error("pg WithdrawNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

func (s *PgStore) ResolveNoticeAsOf(ctx context.Context, noticeID string, asOf time.Time) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			SELECT `+noticeVersionColumnsJoined+`
			FROM notice_versions nv
			JOIN notices n ON n.notice_id = nv.notice_id
			WHERE nv.notice_id = $1
			  AND nv.version_status IN ('PUBLISHED', 'SUPERSEDED', 'WITHDRAWN')
			  AND nv.effective_from <= $2
			ORDER BY nv.effective_from DESC, nv.sequence_no DESC
			LIMIT 1`,
			noticeID, asOf,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeNotFound
	}
	if err != nil {
		s.log.Error("pg ResolveNoticeAsOf failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// ── presentation receipts ────────────────────────────────────────────────────

const presentationReceiptColumns = `
	presentation_receipt_id, tenant_id, notice_version_id, subject_ref, channel, locale,
	created_at, session_ref, template_version, delivery_evidence`

func scanPresentationReceipt(row pgx.Row) (*domain.PresentationReceipt, error) {
	r := &domain.PresentationReceipt{}
	var sessionRef, templateVer, deliveryEv *string
	err := row.Scan(&r.PresentationReceiptID, &r.TenantID, &r.NoticeVersionID, &r.SubjectRef,
		&r.Channel, &r.Locale, &r.CreatedAt, &sessionRef, &templateVer, &deliveryEv)
	if err != nil {
		return nil, err
	}
	r.SessionRef = sessionRef
	r.TemplateVersion = templateVer
	r.DeliveryEvidence = deliveryEv
	return r, nil
}

func (s *PgStore) RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error) {
	id := uuid.New().String()
	var receipt *domain.PresentationReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM notice_versions WHERE notice_version_id = $1 AND notice_id = $2)`,
			versionID, noticeID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrNoticeVersionNotFound
		}
		var err error
		receipt, err = scanPresentationReceipt(tx.QueryRow(ctx, `
			INSERT INTO presentation_receipts (
				presentation_receipt_id, tenant_id, notice_version_id, subject_ref, channel, locale,
				session_ref, template_version, delivery_evidence
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+presentationReceiptColumns,
			id, strPtrOrNil(tenantID), versionID, req.SubjectRef, req.Channel, req.Locale,
			strPtrOrNil(req.SessionRef), strPtrOrNil(req.TemplateVersion), strPtrOrNil(req.DeliveryEvidence),
		))
		return err
	})
	if errors.Is(err, domain.ErrNoticeVersionNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg RecordPresentation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return receipt, nil
}

// ── consent ──────────────────────────────────────────────────────────────────

const consentReceiptColumns = `
	consent_receipt_id, tenant_id, subject_ref, purpose_id, notice_version_id,
	action, capture_channel, actor_principal_id, correlation_id, created_at,
	is_proxy, representative_subject_ref, representative_authority_ref, representative_evidence,
	affirmative_action_type, affirmative_evidence`

func scanConsentReceipt(row pgx.Row) (*domain.ConsentReceipt, error) {
	c := &domain.ConsentReceipt{}
	var correlationID, repSubject, repAuthority, repEvidence, affEvidence *string
	err := row.Scan(&c.ConsentReceiptID, &c.TenantID, &c.SubjectRef, &c.PurposeID, &c.NoticeVersionID,
		&c.Action, &c.CaptureChannel, &c.ActorPrincipalID, &correlationID, &c.CreatedAt,
		&c.IsProxy, &repSubject, &repAuthority, &repEvidence,
		&c.AffirmativeActionType, &affEvidence)
	if err != nil {
		return nil, err
	}
	if correlationID != nil {
		c.CorrelationID = *correlationID
	}
	c.RepresentativeSubjectRef = repSubject
	c.RepresentativeAuthorityRef = repAuthority
	c.RepresentativeEvidence = repEvidence
	c.AffirmativeEvidence = affEvidence
	return c, nil
}

func (s *PgStore) RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error) {
	affAction := req.AffirmativeActionType
	if affAction == "" {
		affAction = "EXPLICIT_CHECKBOX"
	}

	var receipt *domain.ConsentReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		// PRV-N04: Check if an identical unwithdrawn consent receipt already exists for this subject and purpose
		existing, err := scanConsentReceipt(tx.QueryRow(ctx, `
			SELECT `+consentReceiptColumns+`
			FROM consent_receipts c
			WHERE (c.tenant_id IS NULL OR c.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
			  AND c.subject_ref = $1
			  AND c.purpose_id = $2
			  AND c.action = $3
			  AND (c.notice_version_id IS NOT DISTINCT FROM $4::uuid)
			  AND NOT EXISTS (
				  SELECT 1 FROM withdrawal_receipts w WHERE w.consent_receipt_id = c.consent_receipt_id
			  )
			ORDER BY c.created_at DESC
			LIMIT 1`,
			req.SubjectRef, req.PurposeID, req.Action, strPtrOrNil(req.NoticeVersionID),
		))
		if err == nil && existing != nil {
			receipt = existing
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) && !isInvalidUUID(err) {
			return err
		}

		id := uuid.New().String()
		receipt, err = scanConsentReceipt(tx.QueryRow(ctx, `
			INSERT INTO consent_receipts (`+consentReceiptColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), $10, $11, $12, $13, $14, $15)
			RETURNING `+consentReceiptColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.PurposeID, strPtrOrNil(req.NoticeVersionID),
			req.Action, req.CaptureChannel, principalID, strPtrOrNil(correlationID),
			req.IsProxy, strPtrOrNil(req.RepresentativeSubjectRef), strPtrOrNil(req.RepresentativeAuthorityRef), strPtrOrNil(req.RepresentativeEvidence),
			affAction, strPtrOrNil(req.AffirmativeEvidence),
		))
		return err
	})
	if err != nil {
		s.log.Error("pg RecordConsent failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return receipt, nil
}

func (s *PgStore) FindConsentReceipt(ctx context.Context, receiptID string) (*domain.ConsentReceipt, error) {
	var receipt *domain.ConsentReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		receipt, err = scanConsentReceipt(tx.QueryRow(ctx, `
			SELECT `+consentReceiptColumns+` FROM consent_receipts WHERE consent_receipt_id = $1`,
			receiptID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrConsentReceiptNotFound
	}
	if err != nil {
		s.log.Error("pg FindConsentReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return receipt, nil
}

func (s *PgStore) WithdrawConsent(ctx context.Context, receiptID, channel, principalID string) (*domain.WithdrawalReceipt, error) {
	id := uuid.New().String()
	var withdrawal domain.WithdrawalReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM consent_receipts WHERE consent_receipt_id = $1`, receiptID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrConsentReceiptNotFound
			}
			return err
		}

		// PRV-N05: Replayed withdrawal returns the existing effective withdrawal receipt idempotently
		var existingID, existingPrincipal, existingChannel string
		var existingTenant *string
		var existingCreated time.Time
		err := tx.QueryRow(ctx, `
			SELECT withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at
			FROM withdrawal_receipts
			WHERE consent_receipt_id = $1`,
			receiptID,
		).Scan(&existingID, &existingTenant, &withdrawal.ConsentReceiptID, &existingPrincipal, &existingChannel, &existingCreated)
		if err == nil {
			withdrawal.WithdrawalReceiptID = existingID
			withdrawal.TenantID = existingTenant
			withdrawal.WithdrawnByPrincipalID = existingPrincipal
			withdrawal.Channel = existingChannel
			withdrawal.CreatedAt = existingCreated
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		return tx.QueryRow(ctx, `
			INSERT INTO withdrawal_receipts (withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at`,
			id, tenantID, receiptID, principalID, channel,
		).Scan(&withdrawal.WithdrawalReceiptID, &withdrawal.TenantID, &withdrawal.ConsentReceiptID,
			&withdrawal.WithdrawnByPrincipalID, &withdrawal.Channel, &withdrawal.CreatedAt)
	})
	if errors.Is(err, domain.ErrConsentReceiptNotFound) {
		return nil, err
	}
	if err != nil {
		s.log.Error("pg WithdrawConsent failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &withdrawal, nil
}

// ResolveConsentStatus is the derived read PRV-02's schema doc comment
// describes: the latest ConsentReceipt for (subject_ref, purpose_id),
// downgraded to WITHDRAWN if a WithdrawalReceipt references it. Nothing
// here is a stored "status" column.
func (s *PgStore) ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error) {
	res := &domain.ConsentResolution{SubjectRef: subjectRef, PurposeID: purposeID, Status: domain.ConsentStatusNotRequested}
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		receipt, err := scanConsentReceipt(tx.QueryRow(ctx, `
			SELECT `+consentReceiptColumns+`
			FROM consent_receipts
			WHERE subject_ref = $1 AND purpose_id = $2
			ORDER BY created_at DESC
			LIMIT 1`,
			subjectRef, purposeID,
		))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return nil
		}
		if err != nil {
			return err
		}
		res.LatestReceipt = receipt

		var w domain.WithdrawalReceipt
		werr := tx.QueryRow(ctx, `
			SELECT withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at
			FROM withdrawal_receipts WHERE consent_receipt_id = $1`,
			receipt.ConsentReceiptID,
		).Scan(&w.WithdrawalReceiptID, &w.TenantID, &w.ConsentReceiptID, &w.WithdrawnByPrincipalID, &w.Channel, &w.CreatedAt)
		switch {
		case errors.Is(werr, pgx.ErrNoRows):
			res.Status = domain.ConsentStatus(receipt.Action)
		case werr != nil:
			return werr
		default:
			res.WithdrawalReceipt = &w
			res.Status = domain.ConsentStatusWithdrawn
		}
		return nil
	})
	if err != nil {
		s.log.Error("pg ResolveConsentStatus failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return res, nil
}

// ── preferences ──────────────────────────────────────────────────────────────

func (s *PgStore) SetPreference(ctx context.Context, tenantID string, req domain.SetPreferenceRequest) (*domain.PreferenceAssertion, error) {
	id := uuid.New().String()
	var p domain.PreferenceAssertion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO preference_assertions (preference_assertion_id, tenant_id, subject_ref, channel_or_purpose, value, source)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING preference_assertion_id, tenant_id, subject_ref, channel_or_purpose, value, source, created_at`,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.ChannelOrPurpose, req.Value, req.Source,
		).Scan(&p.PreferenceAssertionID, &p.TenantID, &p.SubjectRef, &p.ChannelOrPurpose, &p.Value, &p.Source, &p.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg SetPreference failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &p, nil
}

func (s *PgStore) ResolvePreference(ctx context.Context, subjectRef, channelOrPurpose string) (*domain.PreferenceAssertion, error) {
	var p domain.PreferenceAssertion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT preference_assertion_id, tenant_id, subject_ref, channel_or_purpose, value, source, created_at
			FROM preference_assertions
			WHERE subject_ref = $1 AND channel_or_purpose = $2
			ORDER BY created_at DESC
			LIMIT 1`,
			subjectRef, channelOrPurpose,
		).Scan(&p.PreferenceAssertionID, &p.TenantID, &p.SubjectRef, &p.ChannelOrPurpose, &p.Value, &p.Source, &p.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		p = domain.PreferenceAssertion{SubjectRef: subjectRef, ChannelOrPurpose: channelOrPurpose, Value: domain.PreferenceNotApplicable}
		return &p, nil
	}
	if err != nil {
		s.log.Error("pg ResolvePreference failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &p, nil
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT idempotency_key, tenant_id, endpoint, request_hash, response_code, response_body, created_at
			FROM consent_idempotency_keys
			WHERE idempotency_key = $1`,
			key,
		).Scan(&rec.Key, &rec.TenantID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &rec, nil
}

func (s *PgStore) SaveIdempotency(ctx context.Context, rec domain.IdempotencyRecord) error {
	return s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO consent_idempotency_keys (idempotency_key, tenant_id, endpoint, request_hash, response_code, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
			rec.Key, rec.TenantID, rec.Endpoint, rec.RequestHash, rec.ResponseCode, rec.ResponseBody, rec.CreatedAt,
		)
		return err
	})
}

