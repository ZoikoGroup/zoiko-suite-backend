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

func (s *PgStore) RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error) {
	id := uuid.New().String()
	var receipt domain.PresentationReceipt
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
		return tx.QueryRow(ctx, `
			INSERT INTO presentation_receipts (presentation_receipt_id, tenant_id, notice_version_id, subject_ref, channel, locale)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING presentation_receipt_id, tenant_id, notice_version_id, subject_ref, channel, locale, created_at`,
			id, strPtrOrNil(tenantID), versionID, req.SubjectRef, req.Channel, req.Locale,
		).Scan(&receipt.PresentationReceiptID, &receipt.TenantID, &receipt.NoticeVersionID,
			&receipt.SubjectRef, &receipt.Channel, &receipt.Locale, &receipt.CreatedAt)
	})
	if errors.Is(err, domain.ErrNoticeVersionNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionNotFound
	}
	if err != nil {
		s.log.Error("pg RecordPresentation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &receipt, nil
}

// ── consent ──────────────────────────────────────────────────────────────────

func scanConsentReceipt(row pgx.Row) (*domain.ConsentReceipt, error) {
	c := &domain.ConsentReceipt{}
	var correlationID *string
	err := row.Scan(&c.ConsentReceiptID, &c.TenantID, &c.SubjectRef, &c.PurposeID, &c.NoticeVersionID,
		&c.Action, &c.CaptureChannel, &c.ActorPrincipalID, &correlationID, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	if correlationID != nil {
		c.CorrelationID = *correlationID
	}
	return c, nil
}

const consentReceiptColumns = `
	consent_receipt_id, tenant_id, subject_ref, purpose_id, notice_version_id,
	action, capture_channel, actor_principal_id, correlation_id, created_at`

func (s *PgStore) RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error) {
	id := uuid.New().String()
	var receipt *domain.ConsentReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		receipt, err = scanConsentReceipt(tx.QueryRow(ctx, `
			INSERT INTO consent_receipts (`+consentReceiptColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
			RETURNING `+consentReceiptColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.PurposeID, strPtrOrNil(req.NoticeVersionID),
			req.Action, req.CaptureChannel, principalID, strPtrOrNil(correlationID),
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

		var alreadyWithdrawn bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM withdrawal_receipts WHERE consent_receipt_id = $1)`, receiptID).Scan(&alreadyWithdrawn); err != nil {
			return err
		}
		if alreadyWithdrawn {
			return domain.ErrAlreadyWithdrawn
		}

		return tx.QueryRow(ctx, `
			INSERT INTO withdrawal_receipts (withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at`,
			id, tenantID, receiptID, principalID, channel,
		).Scan(&withdrawal.WithdrawalReceiptID, &withdrawal.TenantID, &withdrawal.ConsentReceiptID,
			&withdrawal.WithdrawnByPrincipalID, &withdrawal.Channel, &withdrawal.CreatedAt)
	})
	if errors.Is(err, domain.ErrConsentReceiptNotFound) || errors.Is(err, domain.ErrAlreadyWithdrawn) {
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
