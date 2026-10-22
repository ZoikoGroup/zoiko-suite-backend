// Package store implements privacy-consent-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/middleware"
	"zoiko.io/privacy-consent-svc/internal/outbox"
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
	// FindPublishedNoticeVersion returns the notice's PUBLISHED version, or
	// ErrNoticeVersionInvalid when it has none.
	FindPublishedNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error)
	ApproveNoticeVersion(ctx context.Context, noticeID, versionID, principalID string) (*domain.NoticeVersion, error)
	// PublishNoticeVersion now takes the publishing principal — §10.1's
	// "publisher" evidence. It used to record no actor at all for the act
	// of publishing.
	PublishNoticeVersion(ctx context.Context, noticeID, versionID, principalID, correlationID string) (*domain.NoticeVersion, error)
	WithdrawNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error)
	ResolveNoticeAsOf(ctx context.Context, noticeID string, asOf time.Time) (*domain.NoticeVersion, error)

	RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error)

	RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error)
	FindConsentReceipt(ctx context.Context, receiptID string) (*domain.ConsentReceipt, error)
	WithdrawConsent(ctx context.Context, receiptID, channel, principalID, correlationID string) (*domain.WithdrawalReceipt, error)
	// InvalidateConsent is the platform-initiated counterpart to
	// WithdrawConsent — see domain.ConsentInvalidation's own doc comment for
	// why the two are not the same fact.
	InvalidateConsent(ctx context.Context, receiptID, reason, channel, principalID, correlationID string) (*domain.ConsentInvalidation, error)
	ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error)

	SetPreference(ctx context.Context, tenantID string, req domain.SetPreferenceRequest) (*domain.PreferenceAssertion, error)
	ResolvePreference(ctx context.Context, subjectRef, channelOrPurpose string) (*domain.PreferenceAssertion, error)

	// GetIdempotency looks up a stored response by (tenant, principal, key).
	GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error)
	// RunIdempotent claims the idempotency key, runs fn, and stores the
	// replayable response in ONE transaction: every store call fn makes joins
	// that transaction (see withTenant), so the evidence rows, the outbox
	// event and the idempotency record commit together or not at all, and
	// concurrent identical requests execute exactly once. When the key was
	// already claimed it runs nothing and returns the stored record.
	RunIdempotent(ctx context.Context, claim domain.IdempotencyRecord, fn func(ctx context.Context) (status int, payload any, err error)) (existing *domain.IdempotencyRecord, status int, payload any, err error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// ambientTxKey carries the transaction opened by RunIdempotent so the store
// calls made inside it join it instead of opening their own.
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

// enqueueEvent writes one outbox event inside tx — see package outbox's doc
// comment for why this replaces the old post-commit, fire-and-forget
// publisher call. entityID is the aggregate the event is about (a subject,
// for consent/preference facts; a notice, for notice facts), matching what
// the old events.PublishParams.EntityID already carried.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, eventType, tenantID, entityID, actorID, correlationID string, payload interface{}) error {
	data, err := outbox.BuildPayload(eventType, entityID, tenantID, actorID, correlationID, payload)
	if err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, outbox.Record{
		EventType: eventType, TenantID: tenantID, EntityID: entityID,
		ActorID: actorID, CorrelationID: correlationID, Payload: data,
	})
}

// ── notices ──────────────────────────────────────────────────────────────────

const noticeVersionColumns = `
	notice_version_id, notice_id, locale, audience, content_hash, version_status,
	effective_from, effective_to, supersedes_version_id, approved_by_principal_id,
	published_by_principal_id, published_at, created_at, created_by_principal_id`

const noticeVersionColumnsJoined = `
	nv.notice_version_id, nv.notice_id, nv.locale, nv.audience, nv.content_hash, nv.version_status,
	nv.effective_from, nv.effective_to, nv.supersedes_version_id, nv.approved_by_principal_id,
	nv.published_by_principal_id, nv.published_at, nv.created_at, nv.created_by_principal_id`

func scanNoticeVersion(row pgx.Row) (*domain.NoticeVersion, error) {
	v := &domain.NoticeVersion{}
	err := row.Scan(&v.NoticeVersionID, &v.NoticeID, &v.Locale, &v.Audience, &v.ContentHash, &v.VersionStatus,
		&v.EffectiveFrom, &v.EffectiveTo, &v.SupersedesVersionID, &v.ApprovedByPrincipalID,
		&v.PublishedByPrincipalID, &v.PublishedAt, &v.CreatedAt, &v.CreatedByPrincipalID)
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
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', NULL, NULL, NULL, NULL, NULL, NULL, NOW(), $6)
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
			VALUES ($1, $2, $3, $4, $5, 'DRAFT', NULL, NULL, $6, NULL, NULL, NULL, NOW(), $7)
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
// PUBLISHED for this notice_id to SUPERSEDED, closing its §10.1 effective
// window (effective_to = this version's effective_from). This is the one
// place a "direct" transition in domain.noticeTransitions isn't the whole
// story: SUPERSEDED is a side effect of publishing a successor, never an
// action a caller takes directly.
//
// principalID is recorded as published_by_principal_id — the §10.1
// "publisher" evidence that used to not exist at all, which left the SoD
// check that refuses the maker or approver from publishing with no row
// recording who actually did. A privacy.notice.published event is enqueued
// in the same transaction (ZS-EVENT-001 §6), replacing the old post-commit
// publish call that could silently drop the event on a crash or broker
// outage.
func (s *PgStore) PublishNoticeVersion(ctx context.Context, noticeID, versionID, principalID, correlationID string) (*domain.NoticeVersion, error) {
	tenantID := middleware.TenantFromContext(ctx)
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE notice_versions
			SET version_status = 'SUPERSEDED', effective_to = $2
			WHERE notice_id = $1 AND version_status = 'PUBLISHED'`,
			noticeID, now,
		); err != nil {
			return err
		}

		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			UPDATE notice_versions
			SET version_status = 'PUBLISHED', effective_from = $3,
			    published_by_principal_id = $4, published_at = $3
			WHERE notice_version_id = $1 AND notice_id = $2 AND version_status = 'APPROVED'
			RETURNING `+noticeVersionColumns,
			versionID, noticeID, now, principalID,
		))
		if err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, "privacy.notice.published", tenantID, version.NoticeID, principalID, correlationID, version)
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
		if err := requirePublishedNoticeVersion(ctx, tx, noticeID, versionID); err != nil {
			return err
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
	if errors.Is(err, domain.ErrNoticeVersionInvalid) {
		return nil, domain.ErrNoticeVersionInvalid
	}
	if err != nil {
		s.log.Error("pg RecordPresentation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return receipt, nil
}

// requirePublishedNoticeVersion is §6's "Receipt always binds exact
// published notice version" (§32 PRV-008), enforced for both presentation
// and consent receipts. Only a version whose status is PUBLISHED passes:
// DRAFT, APPROVED, WITHDRAWN, SUPERSEDED, a malformed id and a nonexistent
// id all answer ErrNoticeVersionInvalid — one answer for all of them, so a
// caller cannot probe which ids exist.
//
// noticeID may be "" (a consent receipt names only the version); when given
// it must also be the version's own notice, so a version id cannot be
// presented under a different notice.
//
// The row is read FOR SHARE inside the caller's transaction. A plain SELECT
// would let a concurrent WithdrawNoticeVersion (or a publish that supersedes
// this version) commit between the check and the receipt INSERT, minting a
// receipt bound to a version that is no longer PUBLISHED. FOR SHARE makes
// that writer wait for this transaction to finish.
func requirePublishedNoticeVersion(ctx context.Context, tx pgx.Tx, noticeID, versionID string) error {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT version_status FROM notice_versions
		WHERE notice_version_id = $1 AND ($2 = '' OR notice_id::text = $2)
		FOR SHARE`,
		versionID, noticeID,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return domain.ErrNoticeVersionInvalid
	}
	if err != nil {
		return err
	}
	if domain.NoticeVersionStatus(status) != domain.NoticeStatusPublished {
		return domain.ErrNoticeVersionInvalid
	}
	return nil
}

// FindPublishedNoticeVersion returns the single PUBLISHED version of a
// notice — what the canonical presentation route (no version segment)
// binds to. It cannot use FindLatestNoticeVersion: the latest-created
// version is often a DRAFT successor being prepared while the previous
// version is still the one subjects are being shown.
func (s *PgStore) FindPublishedNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error) {
	var version *domain.NoticeVersion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = scanNoticeVersion(tx.QueryRow(ctx, `
			SELECT `+noticeVersionColumnsJoined+`
			FROM notice_versions nv
			JOIN notices n ON n.notice_id = nv.notice_id
			WHERE nv.notice_id = $1 AND nv.version_status = 'PUBLISHED'
			ORDER BY nv.effective_from DESC, nv.sequence_no DESC
			LIMIT 1`,
			noticeID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrNoticeVersionInvalid
	}
	if err != nil {
		s.log.Error("pg FindPublishedNoticeVersion failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return version, nil
}

// ── consent ──────────────────────────────────────────────────────────────────

const consentReceiptColumns = `
	consent_receipt_id, tenant_id, subject_ref, purpose_id, notice_version_id,
	action, capture_channel, actor_principal_id, correlation_id, created_at,
	is_proxy, representative_subject_ref, representative_authority_ref, representative_evidence,
	affirmative_action_type, affirmative_evidence, receipt_integrity_hash,
	age_band, is_offline_import, import_source, import_capture_method, import_evidence_ref,
	imported_at, expires_at`

func scanConsentReceipt(row pgx.Row) (*domain.ConsentReceipt, error) {
	c := &domain.ConsentReceipt{}
	var correlationID, repSubject, repAuthority, repEvidence, affEvidence *string
	var importSource, importCaptureMethod, importEvidenceRef *string
	err := row.Scan(&c.ConsentReceiptID, &c.TenantID, &c.SubjectRef, &c.PurposeID, &c.NoticeVersionID,
		&c.Action, &c.CaptureChannel, &c.ActorPrincipalID, &correlationID, &c.CreatedAt,
		&c.IsProxy, &repSubject, &repAuthority, &repEvidence,
		&c.AffirmativeActionType, &affEvidence, &c.ReceiptIntegrityHash,
		&c.AgeBand, &c.IsOfflineImport, &importSource, &importCaptureMethod, &importEvidenceRef,
		&c.ImportedAt, &c.ExpiresAt)
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
	c.ImportSource = importSource
	c.ImportCaptureMethod = importCaptureMethod
	c.ImportEvidenceRef = importEvidenceRef
	return c, nil
}

// normalizeInstant puts a caller-supplied instant in UTC at PostgreSQL's
// microsecond resolution, so what is hashed at insert equals what is stored.
func normalizeInstant(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := t.UTC().Truncate(time.Microsecond)
	return &n
}

func sameNoticeVersion(stored *string, requested string) bool {
	if stored == nil {
		return requested == ""
	}
	return *stored == requested
}

// RecordConsent writes the ConsentReceipt and — when the caller supplied
// one — its ConsentRequest evidence, in one transaction, then enqueues
// privacy.consent.changed in the same transaction (ZS-EVENT-001 §6) rather
// than publishing after commit.
//
// PRV-N04 ("same consent event replayed -> return original receipt; no
// duplicate evidence") applies ONLY while the original receipt is still the
// live outcome of this exact request: it must be the LATEST receipt for the
// (subject, purpose) pair, carry the same action and notice version, and
// still derive to GRANTED/DENIED — not WITHDRAWN, INVALIDATED or EXPIRED. The
// previous check only excluded withdrawn receipts, so after an invalidation
// or an expiry (or after a later DENY) the dead receipt was returned as the
// answer and the subject could never consent again. Now a fresh receipt is
// written; every earlier receipt stays untouched (append-only).
func (s *PgStore) RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error) {
	affAction := req.AffirmativeActionType
	if affAction == "" {
		affAction = "EXPLICIT_CHECKBOX"
	}
	req.ImportedAt = normalizeInstant(req.ImportedAt)
	req.ExpiresAt = normalizeInstant(req.ExpiresAt)

	var receipt *domain.ConsentReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		// §6 / PRV-008: every consent receipt must bind an existing, currently
		// PUBLISHED notice version. Checked BEFORE the PRV-N04 replay below, so
		// an existing receipt can never be returned as the answer to a request
		// that cites an unpublished, omitted, or invalid version.
		if strings.TrimSpace(req.NoticeVersionID) == "" {
			return domain.ErrNoticeVersionInvalid
		}
		if err := requirePublishedNoticeVersion(ctx, tx, "", req.NoticeVersionID); err != nil {
			return err
		}

		before, err := s.resolveTx(ctx, tx, req.SubjectRef, req.PurposeID)
		if err != nil {
			return err
		}
		if l := before.LatestReceipt; l != nil &&
			string(l.Action) == req.Action &&
			sameNoticeVersion(l.NoticeVersionID, req.NoticeVersionID) &&
			before.Status == domain.ConsentStatus(l.Action) {
			receipt = l // PRV-N04: a genuine replay — no new evidence, no event
			return nil
		}

		id := uuid.New().String()
		now := time.Now().UTC().Truncate(time.Microsecond)
		candidate := &domain.ConsentReceipt{
			SubjectRef: req.SubjectRef, PurposeID: req.PurposeID, NoticeVersionID: strPtrOrNil(req.NoticeVersionID),
			Action: domain.ConsentAction(req.Action), CaptureChannel: req.CaptureChannel, ActorPrincipalID: principalID,
			CreatedAt: now, IsProxy: req.IsProxy,
			RepresentativeSubjectRef:   strPtrOrNil(req.RepresentativeSubjectRef),
			RepresentativeAuthorityRef: strPtrOrNil(req.RepresentativeAuthorityRef),
			RepresentativeEvidence:     strPtrOrNil(req.RepresentativeEvidence),
			AffirmativeActionType:      affAction, AffirmativeEvidence: strPtrOrNil(req.AffirmativeEvidence),
			AgeBand: strPtrOrNil(req.AgeBand), IsOfflineImport: req.IsOfflineImport,
			ImportSource: strPtrOrNil(req.ImportSource), ImportCaptureMethod: strPtrOrNil(req.ImportCaptureMethod),
			ImportEvidenceRef: strPtrOrNil(req.ImportEvidenceRef), ImportedAt: req.ImportedAt, ExpiresAt: req.ExpiresAt,
			ConsentRequest: req.ConsentRequest,
		}
		hash := candidate.ComputeIntegrityHash()

		receipt, err = scanConsentReceipt(tx.QueryRow(ctx, `
			INSERT INTO consent_receipts (`+consentReceiptColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			        $11, $12, $13, $14, $15, $16, $17,
			        $18, $19, $20, $21, $22, $23, $24)
			RETURNING `+consentReceiptColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.PurposeID, strPtrOrNil(req.NoticeVersionID),
			req.Action, req.CaptureChannel, principalID, strPtrOrNil(correlationID), now,
			req.IsProxy, strPtrOrNil(req.RepresentativeSubjectRef), strPtrOrNil(req.RepresentativeAuthorityRef), strPtrOrNil(req.RepresentativeEvidence),
			affAction, strPtrOrNil(req.AffirmativeEvidence), hash,
			strPtrOrNil(req.AgeBand), req.IsOfflineImport, strPtrOrNil(req.ImportSource), strPtrOrNil(req.ImportCaptureMethod), strPtrOrNil(req.ImportEvidenceRef),
			req.ImportedAt, req.ExpiresAt,
		))
		if err != nil {
			return err
		}

		scope := ""
		if req.ConsentRequest != nil {
			cr := req.ConsentRequest
			if _, err := tx.Exec(ctx, `
				INSERT INTO consent_requests (
					consent_request_id, tenant_id, consent_receipt_id, scope, data_context,
					recipient_context, technology_context, vendor, storage_lifetime, jurisdiction_rule_ref,
					required_status, notice_version_id, policy_package
				) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				strPtrOrNil(tenantID), receipt.ConsentReceiptID, strPtrOrNil(cr.Scope), strPtrOrNil(cr.DataContext),
				strPtrOrNil(cr.RecipientContext), strPtrOrNil(cr.TechnologyContext), strPtrOrNil(cr.Vendor),
				strPtrOrNil(cr.StorageLifetime), strPtrOrNil(cr.JurisdictionRuleRef),
				strPtrOrNil(cr.RequiredStatus), strPtrOrNil(cr.NoticeVersionID), strPtrOrNil(cr.PolicyPackage),
			); err != nil {
				return fmt.Errorf("persist consent_request evidence: %w", err)
			}
			cr.PurposeID = receipt.PurposeID
			receipt.ConsentRequest = cr
			scope = cr.Scope
		}

		after, err := s.resolveTx(ctx, tx, req.SubjectRef, req.PurposeID)
		if err != nil {
			return err
		}
		effective := receipt.CreatedAt
		if receipt.IsOfflineImport && receipt.ImportedAt != nil {
			effective = *receipt.ImportedAt // the consent's own time, never a fabricated current one
		}
		return s.enqueueEvent(ctx, tx, "privacy.consent.changed", tenantID, receipt.SubjectRef, principalID, correlationID,
			domain.ConsentChangedPayload{
				SubjectRef: receipt.SubjectRef, PurposeID: receipt.PurposeID, Scope: scope,
				ChangeType: string(receipt.Action), OldState: before.Status, NewState: after.Status,
				ReceiptID: receipt.ConsentReceiptID, NoticeVersionID: receipt.NoticeVersionID,
				EffectiveTime: effective, Receipt: receipt,
			})
	})
	if errors.Is(err, domain.ErrNoticeVersionInvalid) {
		return nil, domain.ErrNoticeVersionInvalid
	}
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

// loadConsentRequest returns the ConsentRequest evidence row of a receipt, or
// nil when the receipt was captured without one.
func loadConsentRequest(ctx context.Context, tx pgx.Tx, receipt *domain.ConsentReceipt) (*domain.ConsentRequest, error) {
	cr := &domain.ConsentRequest{PurposeID: receipt.PurposeID}
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(scope,''), COALESCE(data_context,''), COALESCE(recipient_context,''),
		       COALESCE(technology_context,''), COALESCE(vendor,''), COALESCE(storage_lifetime,''),
		       COALESCE(jurisdiction_rule_ref,''), COALESCE(required_status,''),
		       COALESCE(notice_version_id::text,''), COALESCE(policy_package,'')
		FROM consent_requests WHERE consent_receipt_id = $1`,
		receipt.ConsentReceiptID,
	).Scan(&cr.Scope, &cr.DataContext, &cr.RecipientContext, &cr.TechnologyContext, &cr.Vendor,
		&cr.StorageLifetime, &cr.JurisdictionRuleRef, &cr.RequiredStatus, &cr.NoticeVersionID, &cr.PolicyPackage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cr, nil
}

// receiptContext reads what a consent.changed event needs about the receipt a
// withdrawal/invalidation targets, inside the caller's transaction. A receipt
// the session's tenant cannot see (another tenant's, or none) is not found.
func receiptContext(ctx context.Context, tx pgx.Tx, receiptID string) (tenantID *string, subjectRef, purposeID string, noticeVersionID *string, scope string, err error) {
	if err = tx.QueryRow(ctx, `SELECT tenant_id, subject_ref, purpose_id, notice_version_id FROM consent_receipts WHERE consent_receipt_id = $1`,
		receiptID).Scan(&tenantID, &subjectRef, &purposeID, &noticeVersionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			err = domain.ErrConsentReceiptNotFound
		}
		return
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT scope FROM consent_requests WHERE consent_receipt_id = $1), '')`, receiptID).Scan(&scope)
	return
}

// WithdrawConsent is the subject-initiated terminal act. propagation_correlation_id
// is minted once, at the first (non-replay) withdrawal, and is what a
// downstream consumer fanning the withdrawal out to other systems can use
// to trace every resulting action back to this one row (§10.1).
//
// The privacy.consent.changed event it enqueues carries the derived consent
// status of the (subject, purpose) pair before and after, both read inside
// this transaction (§19).
func (s *PgStore) WithdrawConsent(ctx context.Context, receiptID, channel, principalID, correlationID string) (*domain.WithdrawalReceipt, error) {
	tenantCtx := middleware.TenantFromContext(ctx)
	id := uuid.New().String()
	var withdrawal domain.WithdrawalReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID, subjectRef, purposeID, noticeVersionID, scope, err := receiptContext(ctx, tx, receiptID)
		if err != nil {
			return err
		}

		// PRV-N05: Replayed withdrawal returns the existing effective withdrawal receipt idempotently
		var existingID, existingPrincipal, existingChannel, existingPropagation string
		var existingTenant *string
		var existingCreated time.Time
		err = tx.QueryRow(ctx, `
			SELECT withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at, propagation_correlation_id
			FROM withdrawal_receipts
			WHERE consent_receipt_id = $1`,
			receiptID,
		).Scan(&existingID, &existingTenant, &withdrawal.ConsentReceiptID, &existingPrincipal, &existingChannel, &existingCreated, &existingPropagation)
		if err == nil {
			withdrawal.WithdrawalReceiptID = existingID
			withdrawal.TenantID = existingTenant
			withdrawal.WithdrawnByPrincipalID = existingPrincipal
			withdrawal.Channel = existingChannel
			withdrawal.CreatedAt = existingCreated
			withdrawal.PropagationCorrelationID = existingPropagation
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		before, err := s.resolveTx(ctx, tx, subjectRef, purposeID)
		if err != nil {
			return err
		}
		propagationID := "prop-" + uuid.New().String()
		if err := tx.QueryRow(ctx, `
			INSERT INTO withdrawal_receipts (withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, propagation_correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at, propagation_correlation_id`,
			id, tenantID, receiptID, principalID, channel, propagationID,
		).Scan(&withdrawal.WithdrawalReceiptID, &withdrawal.TenantID, &withdrawal.ConsentReceiptID,
			&withdrawal.WithdrawnByPrincipalID, &withdrawal.Channel, &withdrawal.CreatedAt, &withdrawal.PropagationCorrelationID); err != nil {
			return err
		}
		after, err := s.resolveTx(ctx, tx, subjectRef, purposeID)
		if err != nil {
			return err
		}
		w := withdrawal
		return s.enqueueEvent(ctx, tx, "privacy.consent.changed", tenantCtx, subjectRef, principalID, correlationID,
			domain.ConsentChangedPayload{
				SubjectRef: subjectRef, PurposeID: purposeID, Scope: scope,
				ChangeType: string(domain.ConsentStatusWithdrawn), OldState: before.Status, NewState: after.Status,
				ReceiptID: receiptID, WithdrawalID: w.WithdrawalReceiptID, PropagationCorrelation: w.PropagationCorrelationID,
				NoticeVersionID: noticeVersionID, EffectiveTime: w.CreatedAt, Withdrawal: &w,
			})
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

// InvalidateConsent is the platform-initiated terminal act — see
// domain.ConsentInvalidation's own doc comment for why it is never
// recorded as a withdrawal. Idempotent the same way WithdrawConsent is: a
// replay returns the original invalidation rather than erroring or
// creating a second one.
func (s *PgStore) InvalidateConsent(ctx context.Context, receiptID, reason, channel, principalID, correlationID string) (*domain.ConsentInvalidation, error) {
	tenantCtx := middleware.TenantFromContext(ctx)
	id := uuid.New().String()
	var inv domain.ConsentInvalidation
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tenantID, subjectRef, purposeID, noticeVersionID, scope, err := receiptContext(ctx, tx, receiptID)
		if err != nil {
			return err
		}

		var existingID, existingPrincipal, existingReason, existingChannel string
		var existingTenant *string
		var existingCreated time.Time
		err = tx.QueryRow(ctx, `
			SELECT consent_invalidation_id, tenant_id, consent_receipt_id, invalidated_by_principal_id, reason, channel, created_at
			FROM consent_invalidations WHERE consent_receipt_id = $1`,
			receiptID,
		).Scan(&existingID, &existingTenant, &inv.ConsentReceiptID, &existingPrincipal, &existingReason, &existingChannel, &existingCreated)
		if err == nil {
			inv.ConsentInvalidationID = existingID
			inv.TenantID = existingTenant
			inv.InvalidatedByPrincipalID = existingPrincipal
			inv.Reason = existingReason
			inv.Channel = existingChannel
			inv.CreatedAt = existingCreated
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		before, err := s.resolveTx(ctx, tx, subjectRef, purposeID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO consent_invalidations (consent_invalidation_id, tenant_id, consent_receipt_id, invalidated_by_principal_id, reason, channel)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING consent_invalidation_id, tenant_id, consent_receipt_id, invalidated_by_principal_id, reason, channel, created_at`,
			id, tenantID, receiptID, principalID, reason, channel,
		).Scan(&inv.ConsentInvalidationID, &inv.TenantID, &inv.ConsentReceiptID,
			&inv.InvalidatedByPrincipalID, &inv.Reason, &inv.Channel, &inv.CreatedAt); err != nil {
			return err
		}
		after, err := s.resolveTx(ctx, tx, subjectRef, purposeID)
		if err != nil {
			return err
		}
		i := inv
		return s.enqueueEvent(ctx, tx, "privacy.consent.changed", tenantCtx, subjectRef, principalID, correlationID,
			domain.ConsentChangedPayload{
				SubjectRef: subjectRef, PurposeID: purposeID, Scope: scope,
				ChangeType: string(domain.ConsentStatusInvalidated), OldState: before.Status, NewState: after.Status,
				ReceiptID: receiptID, InvalidationID: i.ConsentInvalidationID,
				NoticeVersionID: noticeVersionID, EffectiveTime: i.CreatedAt, Invalidation: &i,
			})
	})
	if errors.Is(err, domain.ErrConsentReceiptNotFound) {
		return nil, err
	}
	if err != nil {
		s.log.Error("pg InvalidateConsent failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &inv, nil
}

// resolveTx derives the current consent status of (subject, purpose) inside
// the caller's transaction: the latest ConsentReceipt for the pair,
// downgraded to INVALIDATED or WITHDRAWN if one of those records references
// it, or to EXPIRED if its ExpiresAt has passed. Precedence is INVALIDATED >
// WITHDRAWN > EXPIRED > the receipt's own action — an invalidated or
// withdrawn grant is not "merely expired" even if its expiry has also
// passed, because the earlier of the two facts is the one a reader needs to
// see. Nothing here is a stored "status" column.
//
// Used by the read path AND, before and after each change, by the write
// paths: RecordConsent's replay decision, and the old/new state in the
// privacy.consent.changed payload.
func (s *PgStore) resolveTx(ctx context.Context, tx pgx.Tx, subjectRef, purposeID string) (*domain.ConsentResolution, error) {
	res := &domain.ConsentResolution{SubjectRef: subjectRef, PurposeID: purposeID, Status: domain.ConsentStatusNotRequested}
	receipt, err := scanConsentReceipt(tx.QueryRow(ctx, `
		SELECT `+consentReceiptColumns+`
		FROM consent_receipts
		WHERE subject_ref = $1 AND purpose_id = $2
		ORDER BY created_at DESC
		LIMIT 1`,
		subjectRef, purposeID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	if receipt.ConsentRequest, err = loadConsentRequest(ctx, tx, receipt); err != nil {
		return nil, err
	}
	res.LatestReceipt = receipt

	var inv domain.ConsentInvalidation
	iErr := tx.QueryRow(ctx, `
		SELECT consent_invalidation_id, tenant_id, consent_receipt_id, invalidated_by_principal_id, reason, channel, created_at
		FROM consent_invalidations WHERE consent_receipt_id = $1`,
		receipt.ConsentReceiptID,
	).Scan(&inv.ConsentInvalidationID, &inv.TenantID, &inv.ConsentReceiptID, &inv.InvalidatedByPrincipalID, &inv.Reason, &inv.Channel, &inv.CreatedAt)
	if iErr == nil {
		res.ConsentInvalidation = &inv
		res.Status = domain.ConsentStatusInvalidated
		return res, nil
	} else if !errors.Is(iErr, pgx.ErrNoRows) {
		return nil, iErr
	}

	var w domain.WithdrawalReceipt
	werr := tx.QueryRow(ctx, `
		SELECT withdrawal_receipt_id, tenant_id, consent_receipt_id, withdrawn_by_principal_id, channel, created_at, propagation_correlation_id
		FROM withdrawal_receipts WHERE consent_receipt_id = $1`,
		receipt.ConsentReceiptID,
	).Scan(&w.WithdrawalReceiptID, &w.TenantID, &w.ConsentReceiptID, &w.WithdrawnByPrincipalID, &w.Channel, &w.CreatedAt, &w.PropagationCorrelationID)
	switch {
	case errors.Is(werr, pgx.ErrNoRows):
		if receipt.ExpiresAt != nil && !receipt.ExpiresAt.After(time.Now().UTC()) {
			res.Status = domain.ConsentStatusExpired
		} else {
			res.Status = domain.ConsentStatus(receipt.Action)
		}
	case werr != nil:
		return nil, werr
	default:
		res.WithdrawalReceipt = &w
		res.Status = domain.ConsentStatusWithdrawn
	}
	return res, nil
}

// ResolveConsentStatus is the derived read: see resolveTx.
func (s *PgStore) ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error) {
	var res *domain.ConsentResolution
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = s.resolveTx(ctx, tx, subjectRef, purposeID)
		return err
	})
	if err != nil {
		s.log.Error("pg ResolveConsentStatus failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return res, nil
}

// ── preferences ──────────────────────────────────────────────────────────────

// preferenceColumns reads the effective time; rows written before migration
// 000008 have none stored and read as their recording time (the evidence table
// is append-only, so they are not rewritten).
const preferenceColumns = `preference_assertion_id, tenant_id, subject_ref, channel_or_purpose, value, source,
	COALESCE(effective_at, created_at), created_at`

func (s *PgStore) SetPreference(ctx context.Context, tenantID string, req domain.SetPreferenceRequest) (*domain.PreferenceAssertion, error) {
	id := uuid.New().String()
	var p domain.PreferenceAssertion
	// Deterministic instant handling: UTC, microsecond precision (what
	// timestamptz stores), so what is returned is exactly what is stored.
	var effective any // nil -> the database's recording instant
	if req.EffectiveAt != nil {
		effective = req.EffectiveAt.UTC().Truncate(time.Microsecond)
	}
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO preference_assertions (preference_assertion_id, tenant_id, subject_ref, channel_or_purpose, value, source, effective_at)
			VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, NOW()))
			RETURNING `+preferenceColumns,
			id, strPtrOrNil(tenantID), req.SubjectRef, req.ChannelOrPurpose, req.Value, req.Source, effective,
		).Scan(&p.PreferenceAssertionID, &p.TenantID, &p.SubjectRef, &p.ChannelOrPurpose, &p.Value, &p.Source, &p.EffectiveAt, &p.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg SetPreference failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	p.EffectiveAt = p.EffectiveAt.UTC() // the driver returns the host's zone; the contract is UTC
	return &p, nil
}

// ResolvePreference returns the assertion in force NOW for (subject, channel or
// purpose): among assertions whose effective time has been reached, the one
// with the latest effective time (ties: the one recorded last). An assertion
// dated in the future is not yet in force and does not govern; a later-recorded
// assertion with an EARLIER effective time does not override one that took
// effect after it. With none in force the answer is NOT_APPLICABLE.
func (s *PgStore) ResolvePreference(ctx context.Context, subjectRef, channelOrPurpose string) (*domain.PreferenceAssertion, error) {
	var p domain.PreferenceAssertion
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT `+preferenceColumns+`
			FROM preference_assertions
			WHERE subject_ref = $1 AND channel_or_purpose = $2
			  AND COALESCE(effective_at, created_at) <= NOW()
			ORDER BY COALESCE(effective_at, created_at) DESC, created_at DESC
			LIMIT 1`,
			subjectRef, channelOrPurpose,
		).Scan(&p.PreferenceAssertionID, &p.TenantID, &p.SubjectRef, &p.ChannelOrPurpose, &p.Value, &p.Source, &p.EffectiveAt, &p.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		p = domain.PreferenceAssertion{SubjectRef: subjectRef, ChannelOrPurpose: channelOrPurpose, Value: domain.PreferenceNotApplicable}
		return &p, nil
	}
	if err != nil {
		s.log.Error("pg ResolvePreference failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	p.EffectiveAt = p.EffectiveAt.UTC()
	return &p, nil
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

var errReplay = errors.New("idempotent replay")

func getIdempotencyTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := tx.QueryRow(ctx, `
		SELECT idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at
		FROM consent_idempotency_keys
		WHERE tenant_id = $1 AND principal_id = $2 AND idempotency_key = $3`,
		tenantID, principalID, key,
	).Scan(&rec.Key, &rec.TenantID, &rec.PrincipalID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec *domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		r, err := getIdempotencyTx(ctx, tx, tenantID, principalID, key)
		rec = r
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return rec, nil
}

func (s *PgStore) RunIdempotent(ctx context.Context, claim domain.IdempotencyRecord, fn func(ctx context.Context) (int, any, error)) (*domain.IdempotencyRecord, int, any, error) {
	if strings.TrimSpace(claim.TenantID) == "" || strings.TrimSpace(middleware.TenantFromContext(ctx)) != strings.TrimSpace(claim.TenantID) {
		return nil, 0, nil, fmt.Errorf("%w: idempotency claim does not match the verified tenant", domain.ErrStoreUnavailable)
	}
	var existing *domain.IdempotencyRecord
	var status int
	var payload any
	var opErr error
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO consent_idempotency_keys
				(idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, ''::bytea, NOW())
			ON CONFLICT (tenant_id, principal_id, idempotency_key) DO NOTHING`,
			claim.Key, claim.TenantID, claim.PrincipalID, claim.Endpoint, claim.RequestHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			rec, err := getIdempotencyTx(ctx, tx, claim.TenantID, claim.PrincipalID, claim.Key)
			if err != nil {
				return err
			}
			existing = rec
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
			UPDATE consent_idempotency_keys SET response_code = $1, response_body = $2
			WHERE tenant_id = $3 AND principal_id = $4 AND idempotency_key = $5`,
			st, body, claim.TenantID, claim.PrincipalID, claim.Key); err != nil {
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
		return nil, 0, nil, opErr // a domain error the handler maps (404/409/422…)
	default:
		s.log.Error("pg RunIdempotent failed", zap.Error(err))
		return nil, 0, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
}
