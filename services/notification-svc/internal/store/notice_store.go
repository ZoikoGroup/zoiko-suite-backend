package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// ZS-SVC-Y-001 NCD-05 persistence: regulated notices (migration 000025). Every transition
// writes its history row and its outbox event in the transaction that moves the status, so a
// notice can never have moved without the record of why, and a consumer is never told of a
// move that did not commit.

const noticeColumns = `notice_id::text, tenant_id, legal_entity_id, lineage_id::text, version_number,
	supersedes_notice_id::text, correction_reason, intent_version_id::text, recipient_principal_id,
	recipient_capacity, channel, locale, subject, body, content_hash, policy_ref,
	to_char(effective_date, 'YYYY-MM-DD'), ack_requirement, deadline_at, status,
	notification_id::text, superseded_by_notice_id::text, created_by_principal_id, created_at`

func scanNotice(s scannable, n *domain.Notice) error {
	return s.Scan(&n.NoticeID, &n.TenantID, &n.LegalEntityID, &n.LineageID, &n.VersionNumber,
		&n.SupersedesNoticeID, &n.CorrectionReason, &n.IntentVersionID, &n.RecipientPrincipalID,
		&n.RecipientCapacity, &n.Channel, &n.Locale, &n.Subject, &n.Body, &n.ContentHash, &n.PolicyRef,
		&n.EffectiveDate, &n.AckRequirement, &n.DeadlineAt, &n.Status,
		&n.NotificationID, &n.SupersededByNoticeID, &n.CreatedByPrincipalID, &n.CreatedAt)
}

func getNoticeTx(ctx context.Context, tx pgx.Tx, tenantID, noticeID string, forUpdate bool) (*domain.Notice, error) {
	q := `SELECT ` + noticeColumns + ` FROM regulated_notices WHERE notice_id::text = $1 AND tenant_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var n domain.Notice
	if err := scanNotice(tx.QueryRow(ctx, q, noticeID, tenantID), &n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNoticeNotFound
		}
		return nil, err
	}
	return &n, nil
}

func noticeHistoryTx(ctx context.Context, tx pgx.Tx, n *domain.Notice, from *string, to, actor, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO regulated_notice_events (tenant_id, notice_id, from_status, to_status, actor_principal_id, reason)
		VALUES ($1, $2::uuid, $3, $4, $5, $6)`, n.TenantID, n.NoticeID, from, to, actor, reason)
	return err
}

// transitionNoticeTx moves a notice to a new status, recording why and announcing it.
// link, when set, is the delivery the notice is being tied to (READY to DELIVERY_IN_PROGRESS).
func transitionNoticeTx(ctx context.Context, tx pgx.Tx, n *domain.Notice, to, actor, reason string, link *string) error {
	from := n.Status
	var err error
	if link != nil {
		_, err = tx.Exec(ctx, `UPDATE regulated_notices SET status = $1, notification_id = $2::uuid WHERE notice_id::text = $3 AND tenant_id = $4`,
			to, *link, n.NoticeID, n.TenantID)
		n.NotificationID = link
	} else {
		_, err = tx.Exec(ctx, `UPDATE regulated_notices SET status = $1 WHERE notice_id::text = $2 AND tenant_id = $3`, to, n.NoticeID, n.TenantID)
	}
	if err != nil {
		return err
	}
	n.Status = to
	if err := noticeHistoryTx(ctx, tx, n, &from, to, actor, reason); err != nil {
		return err
	}
	if et, ok := events.NoticeEventFor(to); ok {
		ev, err := events.NoticeEvent(et, *n, actor, reason)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, n.TenantID, ev)
	}
	return nil
}

// CreateNotice prepares a notice, validates it into READY, and (for a correction) marks the
// version it supersedes. The caller has already validated the content against the intent.
func (s *PgStore) CreateNotice(ctx context.Context, p domain.CreateNoticeParams) (*domain.Notice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out domain.Notice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		lineage, version := uuid.NewString(), 1
		var prior *domain.Notice
		if p.SupersedesNoticeID != "" {
			var err error
			if prior, err = getNoticeTx(ctx, tx, tenantID, p.SupersedesNoticeID, true); err != nil {
				return err
			}
			if prior.SupersededByNoticeID != nil {
				return fmt.Errorf("%w: this version was already corrected by a newer one", domain.ErrNoticeState)
			}
			if prior.RecipientPrincipalID != p.RecipientPrincipalID || prior.LegalEntityID != p.LegalEntityID {
				return domain.NoticeProblem{Msg: "a correction goes to the same recipient and legal entity; a different recipient is a new notice (and a mistaken one is a privacy incident to assess, not a correction)"}
			}
			lineage, version = prior.LineageID, prior.VersionNumber+1
		}
		var supersedes, reason *string
		if prior != nil {
			supersedes, reason = &prior.NoticeID, &p.CorrectionReason
		}
		if err := scanNotice(tx.QueryRow(ctx, `
			INSERT INTO regulated_notices (tenant_id, legal_entity_id, lineage_id, version_number, supersedes_notice_id, correction_reason,
				intent_version_id, recipient_principal_id, locale, subject, body, content_hash, policy_ref, effective_date,
				ack_requirement, deadline_at, created_by_principal_id)
			VALUES ($1,$2,$3::uuid,$4,$5::uuid,$6,$7::uuid,$8,$9,$10,$11,$12,$13,$14::date,$15,$16,$17)
			RETURNING `+noticeColumns,
			tenantID, p.LegalEntityID, lineage, version, supersedes, reason,
			p.IntentVersionID, p.RecipientPrincipalID, p.Locale, p.Subject, p.Body,
			domain.NoticeContentHash(p.Subject, p.Body, p.Locale), p.PolicyRef, p.EffectiveDate,
			p.AckRequirement, p.DeadlineAt, p.CreatedByPrincipalID), &out); err != nil {
			return err
		}
		if err := noticeHistoryTx(ctx, tx, &out, nil, domain.NoticePrepared, p.CreatedByPrincipalID, "notice prepared"); err != nil {
			return err
		}
		// Validation happened before the insert, so a notice that exists is ready.
		if err := transitionNoticeTx(ctx, tx, &out, domain.NoticeReady, p.CreatedByPrincipalID, "content, recipient, intent and policy basis validated", nil); err != nil {
			return err
		}
		if prior != nil {
			if _, err := tx.Exec(ctx, `UPDATE regulated_notices SET superseded_by_notice_id = $1::uuid WHERE notice_id::text = $2 AND tenant_id = $3`,
				out.NoticeID, prior.NoticeID, tenantID); err != nil {
				return err
			}
			ev, err := events.NoticeEvent(events.TypeNoticeCorrected, out, p.CreatedByPrincipalID, p.CorrectionReason)
			if err != nil {
				return err
			}
			return enqueue(ctx, tx, tenantID, ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetNotice reads one notice version.
func (s *PgStore) GetNotice(ctx context.Context, noticeID string) (*domain.Notice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.Notice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = getNoticeTx(ctx, tx, tenantID, noticeID, false)
		return err
	})
	return out, err
}

// BeginNoticeDispatch ties a READY notice to the delivery that carries it and moves it to
// DELIVERY_IN_PROGRESS. started is false for a replay of the same dispatch.
func (s *PgStore) BeginNoticeDispatch(ctx context.Context, noticeID, notificationID, actor string) (*domain.Notice, bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, false, domain.ErrIdentityMissing
	}
	var out *domain.Notice
	started := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := getNoticeTx(ctx, tx, tenantID, noticeID, true)
		if err != nil {
			return err
		}
		out = n
		switch {
		case n.Status == domain.NoticeReady:
			if n.SupersededByNoticeID != nil {
				return fmt.Errorf("%w: a newer version of this notice exists and supersedes it", domain.ErrNoticeState)
			}
			started = true
			return transitionNoticeTx(ctx, tx, n, domain.NoticeDeliveryInProgess, actor, "submitted for delivery", &notificationID)
		case n.NotificationID != nil && *n.NotificationID == notificationID:
			return nil // the same dispatch again
		default:
			return fmt.Errorf("%w: status is %s", domain.ErrNoticeState, n.Status)
		}
	})
	if err != nil {
		return nil, false, err
	}
	return out, started, nil
}

// applyNoticeProgressTx moves a notice as far as the delivery facts allow, one rule at a time.
func applyNoticeProgressTx(ctx context.Context, tx pgx.Tx, n *domain.Notice, now time.Time) error {
	for i := 0; i < 4; i++ {
		var notifStatus, notifFailure string
		var facts []domain.DeliveryEvidence
		if n.Status == domain.NoticeDeliveryInProgess && n.NotificationID != nil {
			if err := tx.QueryRow(ctx, `SELECT status, COALESCE(failure_reason, '') FROM notifications
				WHERE notification_id::text = $1 AND tenant_id = $2`, *n.NotificationID, n.TenantID).Scan(&notifStatus, &notifFailure); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT fact FROM notification_delivery_evidence
				WHERE notification_id::text = $1 AND tenant_id = $2`, *n.NotificationID, n.TenantID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var f domain.DeliveryEvidence
				if err := rows.Scan(&f.Fact); err != nil {
					rows.Close()
					return err
				}
				facts = append(facts, f)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		prog, ok := domain.DecideNoticeProgress(n, notifStatus, notifFailure, facts, now)
		if !ok {
			return nil
		}
		if err := transitionNoticeTx(ctx, tx, n, prog.To, "system", prog.Reason, nil); err != nil {
			return err
		}
	}
	return nil
}

// RefreshNotice advances a notice from the delivery facts that now exist. It is safe to call
// at any time and as often as wanted: it moves a notice only when a rule says it must.
func (s *PgStore) RefreshNotice(ctx context.Context, noticeID string, now time.Time) (*domain.Notice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.Notice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := getNoticeTx(ctx, tx, tenantID, noticeID, true)
		if err != nil {
			return err
		}
		out = n
		return applyNoticeProgressTx(ctx, tx, n, now)
	})
	return out, err
}

// RecordNoticeAck records the recipient's response to one exact notice version and moves the
// notice to the status it produces. Only the recipient, acting as themselves, may respond:
// nobody can respond on their behalf, and an unanswered deadline is never turned into a
// response.
func (s *PgStore) RecordNoticeAck(ctx context.Context, noticeID, actor, action, comment string, now time.Time) (*domain.Notice, *domain.NoticeAck, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil, domain.ErrIdentityMissing
	}
	var out *domain.Notice
	var ack domain.NoticeAck
	// refuse is returned AFTER commit: a response that arrives too late is refused, but the
	// expiry or evidence that made it too late must still be recorded.
	var refuse error
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := getNoticeTx(ctx, tx, tenantID, noticeID, true)
		if err != nil {
			return err
		}
		out = n
		if actor != n.RecipientPrincipalID {
			return domain.ErrNotNoticeRecipient
		}
		// Bring the notice up to date first: delivery evidence may have just arrived, or the
		// deadline may have just passed.
		if err := applyNoticeProgressTx(ctx, tx, n, now); err != nil {
			return err
		}
		switch n.Status {
		case domain.NoticeAckPending:
		case domain.NoticeAcknowledged, domain.NoticeDeclined, domain.NoticeDisputed:
			refuse = domain.ErrNoticeAlreadyAnswered
			return nil
		case domain.NoticeSatisfiedByPolicy:
			refuse = domain.ErrAckNotRequired
			return nil
		default:
			refuse = fmt.Errorf("%w: status is %s", domain.ErrNoticeState, n.Status)
			return nil
		}
		to, err := domain.AckOutcome(n.AckRequirement, action)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO notice_acknowledgements (tenant_id, notice_id, action, actor_principal_id, method, comment, occurred_at)
			VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)
			RETURNING ack_id::text, notice_id::text, action, actor_principal_id, method, COALESCE(comment, ''), occurred_at`,
			tenantID, n.NoticeID, action, actor, domain.AckMethodAuthAction, nullIfEmpty(comment), now).
			Scan(&ack.AckID, &ack.NoticeID, &ack.Action, &ack.ActorPrincipalID, &ack.Method, &ack.Comment, &ack.OccurredAt); err != nil {
			if pgCode(err) == "23505" {
				return domain.ErrNoticeAlreadyAnswered
			}
			return err
		}
		return transitionNoticeTx(ctx, tx, n, to, actor, "the recipient responded: "+action, nil)
	})
	if err != nil {
		return nil, nil, err
	}
	if refuse != nil {
		return out, nil, refuse
	}
	return out, &ack, nil
}

// ListNoticeTransitions returns the notice's full history, oldest first.
func (s *PgStore) ListNoticeTransitions(ctx context.Context, noticeID string) ([]domain.NoticeTransition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := []domain.NoticeTransition{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT event_id::text, notice_id::text, COALESCE(from_status, ''), to_status, actor_principal_id, reason, occurred_at
			FROM regulated_notice_events WHERE tenant_id = $1 AND notice_id::text = $2 ORDER BY seq`, tenantID, noticeID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.NoticeTransition
			if err := rows.Scan(&t.EventID, &t.NoticeID, &t.FromStatus, &t.ToStatus, &t.ActorPrincipalID, &t.Reason, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// GetNoticeAck returns the recipient's response, or nil when there is none.
func (s *PgStore) GetNoticeAck(ctx context.Context, noticeID string) (*domain.NoticeAck, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var a domain.NoticeAck
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ack_id::text, notice_id::text, action, actor_principal_id, method, COALESCE(comment, ''), occurred_at
			FROM notice_acknowledgements WHERE tenant_id = $1 AND notice_id::text = $2`, tenantID, noticeID).
			Scan(&a.AckID, &a.NoticeID, &a.Action, &a.ActorPrincipalID, &a.Method, &a.Comment, &a.OccurredAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// OpenNotice names a notice that still has a clock or a delivery running.
type OpenNotice struct{ NoticeID, TenantID string }

// FindOpenNotices lists notices awaiting delivery evidence or a response, across tenants.
// Read-only under the platform scope; the caller then acts under each tenant.
func (s *PgStore) FindOpenNotices(ctx context.Context, limit int) ([]OpenNotice, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT notice_id::text, tenant_id FROM regulated_notices
		WHERE status IN ('DELIVERY_IN_PROGRESS', 'DELIVERY_EVIDENCED', 'ACK_PENDING') ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var out []OpenNotice
	for rows.Next() {
		var o OpenNotice
		if err := rows.Scan(&o.NoticeID, &o.TenantID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}
