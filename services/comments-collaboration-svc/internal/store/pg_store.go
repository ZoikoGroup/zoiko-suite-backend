// Package store provides the PostgreSQL implementation of
// comments-collaboration-svc's persistence layer.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	svcmiddleware "zoiko.io/comments-collaboration-svc/internal/middleware"
)

// Store is Wave 1's own persistence contract — AddComment/EditComment and
// the three named queries this wave implements. Wave 2 adds
// Mention/React/Moderate/ResolveThread/AttachReference and their own
// queries to this same interface.
type Store interface {
	AddComment(ctx context.Context, p domain.AddCommentParams) (*domain.Comment, *domain.CommentThread, error)
	EditComment(ctx context.Context, p domain.EditCommentParams) (*domain.Comment, error)
	// GetComment is the read-only fetch every mutating comment command
	// uses to authorize before it acts — returns the comment's parent
	// thread too, since authorization is scoped by the thread's
	// legal_entity_id, not the comment row itself.
	GetComment(ctx context.Context, tenantID, commentID string) (*domain.Comment, *domain.CommentThread, error)
	GetThread(ctx context.Context, threadID string) (*domain.CommentThread, error)
	ListComments(ctx context.Context, tenantID, threadID string) ([]domain.Comment, error)
	GetCommentHistory(ctx context.Context, tenantID, commentID string) ([]domain.CommentVersion, error)

	DeleteComment(ctx context.Context, p domain.DeleteCommentParams) (*domain.Comment, error)
	Mention(ctx context.Context, p domain.MentionParams) (*domain.Mention, error)
	React(ctx context.Context, p domain.ReactParams) (added bool, err error)
	Moderate(ctx context.Context, p domain.ModerateParams) (*domain.Comment, error)
	ResolveThread(ctx context.Context, p domain.ResolveThreadParams) (*domain.CommentThread, error)
	AttachReference(ctx context.Context, p domain.AttachReferenceParams) (*domain.CommentAttachment, error)
	GetMentions(ctx context.Context, tenantID, commentID string) ([]domain.Mention, error)
	GetModerationTrail(ctx context.Context, tenantID, commentID string) ([]domain.ModerationEntry, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

const threadColumns = `thread_id, tenant_id, legal_entity_id, linked_object_type, linked_object_id,
	restricted, status, created_by, created_at, resolved_by, resolved_at, resolution_note`

func scanThread(row pgx.Row) (*domain.CommentThread, error) {
	t := &domain.CommentThread{}
	err := row.Scan(&t.ThreadID, &t.TenantID, &t.LegalEntityID, &t.LinkedObjectType, &t.LinkedObjectID,
		&t.Restricted, &t.Status, &t.CreatedBy, &t.CreatedAt, &t.ResolvedBy, &t.ResolvedAt, &t.ResolutionNote)
	return t, err
}

const commentColumns = `comment_id, thread_id, tenant_id, parent_comment_id, current_version_id, status,
	created_by, created_at, updated_at, moderated_at, moderated_by_principal_id, moderation_reason,
	deleted_at, deleted_by_principal_id, deletion_reason`

func scanComment(row pgx.Row) (*domain.Comment, error) {
	c := &domain.Comment{}
	var parentCommentID *string
	err := row.Scan(&c.CommentID, &c.ThreadID, &c.TenantID, &parentCommentID, &c.CurrentVersionID, &c.Status,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.ModeratedAt, &c.ModeratedByPrincipalID, &c.ModerationReason,
		&c.DeletedAt, &c.DeletedByPrincipalID, &c.DeletionReason)
	if err != nil {
		return nil, err
	}
	if parentCommentID != nil {
		c.ParentCommentID = *parentCommentID
	}
	return c, nil
}

const versionColumns = `version_id, comment_id, tenant_id, version_number, body, edited_by_principal_id, edited_at`

func scanVersion(row pgx.Row) (*domain.CommentVersion, error) {
	v := &domain.CommentVersion{}
	err := row.Scan(&v.VersionID, &v.CommentID, &v.TenantID, &v.VersionNumber, &v.Body, &v.EditedByPrincipalID, &v.EditedAt)
	return v, err
}

// AddComment — BIZ-09's own AddComment command. Finds the most recent
// thread for (tenant, legal_entity, linked_object); creates one if none
// exists; reopens a RESOLVED thread to ACTIVE if new discussion lands on
// it. See domain.AddCommentParams's own doc comment.
func (s *PgStore) AddComment(ctx context.Context, p domain.AddCommentParams) (*domain.Comment, *domain.CommentThread, error) {
	if p.Body == "" {
		return nil, nil, domain.ErrEmptyBody
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, nil, err
	}

	thread, err := scanThread(tx.QueryRow(ctx, `
		SELECT `+threadColumns+` FROM comment_threads
		WHERE tenant_id=$1 AND legal_entity_id=$2 AND linked_object_type=$3 AND linked_object_id=$4
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE
	`, p.TenantID, p.LegalEntityID, p.LinkedObjectType, p.LinkedObjectID))
	threadExists := true
	if errors.Is(err, pgx.ErrNoRows) {
		threadExists = false
	} else if err != nil {
		return nil, nil, err
	}

	if !threadExists {
		threadID := "thread-" + uuid.New().String()
		thread, err = scanThread(tx.QueryRow(ctx, `
			INSERT INTO comment_threads (thread_id, tenant_id, legal_entity_id, linked_object_type, linked_object_id, restricted, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+threadColumns,
			threadID, p.TenantID, p.LegalEntityID, p.LinkedObjectType, p.LinkedObjectID, p.Restricted, p.CreatedByPrincipalID))
		if err != nil {
			return nil, nil, fmt.Errorf("insert comment_thread: %w", err)
		}
	} else if thread.Status == domain.ThreadStatusResolved {
		thread, err = scanThread(tx.QueryRow(ctx, `
			UPDATE comment_threads SET status='ACTIVE', resolved_by='', resolved_at=NULL, resolution_note=''
			WHERE thread_id=$1 AND tenant_id=$2 RETURNING `+threadColumns,
			thread.ThreadID, p.TenantID))
		if err != nil {
			return nil, nil, fmt.Errorf("reopen comment_thread: %w", err)
		}
	}

	if p.ParentCommentID != "" {
		var parentThreadID string
		err := tx.QueryRow(ctx, `SELECT thread_id FROM comments WHERE comment_id=$1 AND tenant_id=$2`, p.ParentCommentID, p.TenantID).Scan(&parentThreadID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, domain.ErrCommentNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		if parentThreadID != thread.ThreadID {
			return nil, nil, domain.ErrParentCommentNotInThread
		}
	}

	commentID := "comment-" + uuid.New().String()
	versionID := "commentver-" + uuid.New().String()

	var parentCommentID any
	if p.ParentCommentID != "" {
		parentCommentID = p.ParentCommentID
	}
	// The comment row is inserted BEFORE its first version — comment_versions
	// has an FK back to comments, so the comment must exist first.
	// current_version_id is backfilled immediately after, in the same
	// transaction, so no caller ever observes it unset.
	if _, err := tx.Exec(ctx, `
		INSERT INTO comments (comment_id, thread_id, tenant_id, parent_comment_id, created_by)
		VALUES ($1,$2,$3,$4,$5)
	`, commentID, thread.ThreadID, p.TenantID, parentCommentID, p.CreatedByPrincipalID); err != nil {
		return nil, nil, fmt.Errorf("insert comment: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO comment_versions (version_id, comment_id, tenant_id, version_number, body, edited_by_principal_id)
		VALUES ($1,$2,$3,1,$4,$5)
	`, versionID, commentID, p.TenantID, p.Body, p.CreatedByPrincipalID); err != nil {
		return nil, nil, fmt.Errorf("insert comment_version: %w", err)
	}

	comment, err := scanComment(tx.QueryRow(ctx, `
		UPDATE comments SET current_version_id=$3 WHERE comment_id=$1 AND tenant_id=$2 RETURNING `+commentColumns,
		commentID, p.TenantID, versionID))
	if err != nil {
		return nil, nil, fmt.Errorf("backfill current_version_id: %w", err)
	}
	comment.CurrentBody = p.Body

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return comment, thread, nil
}

// EditComment — BIZ-09's own EditComment command. Never mutates an
// existing CommentVersion; inserts a new one and repoints
// comments.current_version_id.
func (s *PgStore) EditComment(ctx context.Context, p domain.EditCommentParams) (*domain.Comment, error) {
	if p.Body == "" {
		return nil, domain.ErrEmptyBody
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanComment(tx.QueryRow(ctx, `SELECT `+commentColumns+` FROM comments WHERE comment_id=$1 AND tenant_id=$2 FOR UPDATE`, p.CommentID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommentNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.CommentStatusActive && current.Status != domain.CommentStatusEdited {
		return nil, domain.ErrCommentInvalidState
	}

	var maxVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number),0) FROM comment_versions WHERE tenant_id=$1 AND comment_id=$2`, p.TenantID, p.CommentID).Scan(&maxVersion); err != nil {
		return nil, err
	}
	versionID := "commentver-" + uuid.New().String()
	if _, err := tx.Exec(ctx, `
		INSERT INTO comment_versions (version_id, comment_id, tenant_id, version_number, body, edited_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, versionID, p.CommentID, p.TenantID, maxVersion+1, p.Body, p.ActorPrincipalID); err != nil {
		return nil, fmt.Errorf("insert comment_version: %w", err)
	}

	comment, err := scanComment(tx.QueryRow(ctx, `
		UPDATE comments SET status='EDITED', current_version_id=$3, updated_at=now()
		WHERE comment_id=$1 AND tenant_id=$2 RETURNING `+commentColumns,
		p.CommentID, p.TenantID, versionID))
	if err != nil {
		return nil, err
	}
	comment.CurrentBody = p.Body
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return comment, nil
}

// GetComment — the read-only fetch backing fetch-then-authorize-then-
// mutate for every command that targets an existing comment.
func (s *PgStore) GetComment(ctx context.Context, tenantID, commentID string) (*domain.Comment, *domain.CommentThread, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, nil, err
	}
	comment, err := scanComment(tx.QueryRow(ctx, `SELECT `+commentColumns+` FROM comments WHERE comment_id=$1 AND tenant_id=$2`, commentID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, domain.ErrCommentNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	thread, err := scanThread(tx.QueryRow(ctx, `SELECT `+threadColumns+` FROM comment_threads WHERE thread_id=$1 AND tenant_id=$2`, comment.ThreadID, tenantID))
	if err != nil {
		return nil, nil, err
	}
	return comment, thread, nil
}

func (s *PgStore) GetThread(ctx context.Context, threadID string) (*domain.CommentThread, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	t, err := scanThread(tx.QueryRow(ctx, `SELECT `+threadColumns+` FROM comment_threads WHERE thread_id=$1 AND tenant_id=$2`,
		threadID, svcmiddleware.TenantFromContext(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrThreadNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListComments — BIZ-09's own ListComments query. Each comment's
// CurrentBody is populated via a join against comment_versions.
func (s *PgStore) ListComments(ctx context.Context, tenantID, threadID string) ([]domain.Comment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT c.comment_id, c.thread_id, c.tenant_id, c.parent_comment_id, c.current_version_id, c.status,
			c.created_by, c.created_at, c.updated_at, c.moderated_at, c.moderated_by_principal_id, c.moderation_reason,
			c.deleted_at, c.deleted_by_principal_id, c.deletion_reason, v.body
		FROM comments c
		JOIN comment_versions v ON v.version_id = c.current_version_id AND v.tenant_id = c.tenant_id
		WHERE c.tenant_id=$1 AND c.thread_id=$2
		ORDER BY c.created_at ASC
	`, tenantID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Comment
	for rows.Next() {
		c := domain.Comment{}
		var parentCommentID *string
		if err := rows.Scan(&c.CommentID, &c.ThreadID, &c.TenantID, &parentCommentID, &c.CurrentVersionID, &c.Status,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.ModeratedAt, &c.ModeratedByPrincipalID, &c.ModerationReason,
			&c.DeletedAt, &c.DeletedByPrincipalID, &c.DeletionReason, &c.CurrentBody); err != nil {
			return nil, err
		}
		if parentCommentID != nil {
			c.ParentCommentID = *parentCommentID
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCommentHistory — BIZ-09's own GetCommentHistory query. Returns every
// version ever written for a comment, oldest first — the doc's own
// "original history preserved" guarantee, queryable.
func (s *PgStore) GetCommentHistory(ctx context.Context, tenantID, commentID string) ([]domain.CommentVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+versionColumns+` FROM comment_versions WHERE tenant_id=$1 AND comment_id=$2 ORDER BY version_number ASC`, tenantID, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CommentVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// DeleteComment — BIZ-09's own DeleteComment command. Never a hard
// delete — moves the comment to DELETED_REDACTED. Retention/legal-hold
// checking happens in the handler (this store call is only reached once
// that check has already passed) — same fetch-then-authorize-then-mutate
// split as everywhere else, with the retention check as an extra gate
// alongside authorization.
func (s *PgStore) DeleteComment(ctx context.Context, p domain.DeleteCommentParams) (*domain.Comment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanComment(tx.QueryRow(ctx, `SELECT `+commentColumns+` FROM comments WHERE comment_id=$1 AND tenant_id=$2 FOR UPDATE`, p.CommentID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommentNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status == domain.CommentStatusDeletedRedacted {
		return nil, domain.ErrCommentInvalidState
	}
	comment, err := scanComment(tx.QueryRow(ctx, `
		UPDATE comments SET status='DELETED_REDACTED', deleted_at=now(), deleted_by_principal_id=$3, deletion_reason=$4, updated_at=now()
		WHERE comment_id=$1 AND tenant_id=$2 RETURNING `+commentColumns,
		p.CommentID, p.TenantID, p.ActorPrincipalID, p.Reason))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return comment, nil
}

// Mention — BIZ-09's own Mention command. VisibilityGranted is decided
// by the caller before this is invoked; this method only records the
// outcome — see domain.MentionParams's own doc comment.
func (s *PgStore) Mention(ctx context.Context, p domain.MentionParams) (*domain.Mention, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	var exists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM comments WHERE comment_id=$1 AND tenant_id=$2`, p.CommentID, p.TenantID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, domain.ErrCommentNotFound
	}
	m := &domain.Mention{}
	mentionID := "mention-" + uuid.New().String()
	err = tx.QueryRow(ctx, `
		INSERT INTO mentions (mention_id, comment_id, tenant_id, mentioned_principal_id, visibility_granted)
		VALUES ($1,$2,$3,$4,$5) RETURNING mention_id, comment_id, tenant_id, mentioned_principal_id, visibility_granted, created_at
	`, mentionID, p.CommentID, p.TenantID, p.MentionedPrincipalID, p.VisibilityGranted).
		Scan(&m.MentionID, &m.CommentID, &m.TenantID, &m.MentionedPrincipalID, &m.VisibilityGranted, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert mention: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// React — BIZ-09's own React command. A toggle: reacting again with the
// same (comment, principal, reaction_type) un-reacts. added reports
// which way this call went, so the handler can decide whether to publish
// anything (Wave 1/2 name no ReactionAdded/Removed event, so this is
// purely for the caller's own response shaping today).
func (s *PgStore) React(ctx context.Context, p domain.ReactParams) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return false, err
	}
	var exists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM comments WHERE comment_id=$1 AND tenant_id=$2`, p.CommentID, p.TenantID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, domain.ErrCommentNotFound
	}
	tag, err := tx.Exec(ctx, `DELETE FROM reactions WHERE tenant_id=$1 AND comment_id=$2 AND principal_id=$3 AND reaction_type=$4`,
		p.TenantID, p.CommentID, p.PrincipalID, p.ReactionType)
	if err != nil {
		return false, err
	}
	added := false
	if tag.RowsAffected() == 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO reactions (reaction_id, comment_id, tenant_id, principal_id, reaction_type) VALUES ($1,$2,$3,$4,$5)`,
			"reaction-"+uuid.New().String(), p.CommentID, p.TenantID, p.PrincipalID, p.ReactionType); err != nil {
			return false, fmt.Errorf("insert reaction: %w", err)
		}
		added = true
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return added, nil
}

// Moderate — BIZ-09's own Moderate command. Valid from ACTIVE or EDITED
// only. Reason is mandatory (checked by the caller, mirrored here) —
// doc's own T46.
func (s *PgStore) Moderate(ctx context.Context, p domain.ModerateParams) (*domain.Comment, error) {
	if p.Reason == "" {
		return nil, domain.ErrReasonRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanComment(tx.QueryRow(ctx, `SELECT `+commentColumns+` FROM comments WHERE comment_id=$1 AND tenant_id=$2 FOR UPDATE`, p.CommentID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommentNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.CommentStatusActive && current.Status != domain.CommentStatusEdited {
		return nil, domain.ErrCommentInvalidState
	}
	comment, err := scanComment(tx.QueryRow(ctx, `
		UPDATE comments SET status='MODERATED', moderated_at=now(), moderated_by_principal_id=$3, moderation_reason=$4, updated_at=now()
		WHERE comment_id=$1 AND tenant_id=$2 RETURNING `+commentColumns,
		p.CommentID, p.TenantID, p.ModeratorPrincipalID, p.Reason))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO moderation_trail (moderation_id, comment_id, tenant_id, moderator_principal_id, action, reason)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, "modtrail-"+uuid.New().String(), p.CommentID, p.TenantID, p.ModeratorPrincipalID, p.Action, p.Reason); err != nil {
		return nil, fmt.Errorf("insert moderation_trail: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return comment, nil
}

// ResolveThread — BIZ-09's own ResolveThread command. Valid from ACTIVE
// only.
func (s *PgStore) ResolveThread(ctx context.Context, p domain.ResolveThreadParams) (*domain.CommentThread, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanThread(tx.QueryRow(ctx, `SELECT `+threadColumns+` FROM comment_threads WHERE thread_id=$1 AND tenant_id=$2 FOR UPDATE`, p.ThreadID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrThreadNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.ThreadStatusActive {
		return nil, domain.ErrThreadInvalidState
	}
	thread, err := scanThread(tx.QueryRow(ctx, `
		UPDATE comment_threads SET status='RESOLVED', resolved_by=$3, resolved_at=now(), resolution_note=$4
		WHERE thread_id=$1 AND tenant_id=$2 RETURNING `+threadColumns,
		p.ThreadID, p.TenantID, p.ActorPrincipalID, p.ResolutionNote))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return thread, nil
}

// AttachReference — BIZ-09's own AttachReference command. Refused on a
// DELETED_REDACTED comment — nothing legitimate attaches to evidence
// that has already been redacted.
func (s *PgStore) AttachReference(ctx context.Context, p domain.AttachReferenceParams) (*domain.CommentAttachment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanComment(tx.QueryRow(ctx, `SELECT `+commentColumns+` FROM comments WHERE comment_id=$1 AND tenant_id=$2 FOR UPDATE`, p.CommentID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommentNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status == domain.CommentStatusDeletedRedacted {
		return nil, domain.ErrCommentInvalidState
	}
	linkedObjectType := p.LinkedObjectType
	if linkedObjectType == "" {
		linkedObjectType = "DOCUMENT"
	}
	a := &domain.CommentAttachment{}
	err = tx.QueryRow(ctx, `
		INSERT INTO comment_attachments (attachment_id, comment_id, tenant_id, linked_object_type, linked_object_id, attached_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING attachment_id, comment_id, tenant_id, linked_object_type, linked_object_id, attached_by_principal_id, attached_at
	`, "attachment-"+uuid.New().String(), p.CommentID, p.TenantID, linkedObjectType, p.LinkedObjectID, p.AttachedByPrincipalID).
		Scan(&a.AttachmentID, &a.CommentID, &a.TenantID, &a.LinkedObjectType, &a.LinkedObjectID, &a.AttachedByPrincipalID, &a.AttachedAt)
	if err != nil {
		return nil, fmt.Errorf("insert comment_attachment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// GetMentions — BIZ-09's own GetMentions query.
func (s *PgStore) GetMentions(ctx context.Context, tenantID, commentID string) ([]domain.Mention, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT mention_id, comment_id, tenant_id, mentioned_principal_id, visibility_granted, created_at
		FROM mentions WHERE tenant_id=$1 AND comment_id=$2 ORDER BY created_at ASC`, tenantID, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Mention
	for rows.Next() {
		var m domain.Mention
		if err := rows.Scan(&m.MentionID, &m.CommentID, &m.TenantID, &m.MentionedPrincipalID, &m.VisibilityGranted, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModerationTrail — BIZ-09's own GetModerationTrail query.
func (s *PgStore) GetModerationTrail(ctx context.Context, tenantID, commentID string) ([]domain.ModerationEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT moderation_id, comment_id, tenant_id, moderator_principal_id, action, reason, occurred_at
		FROM moderation_trail WHERE tenant_id=$1 AND comment_id=$2 ORDER BY occurred_at ASC`, tenantID, commentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModerationEntry
	for rows.Next() {
		var m domain.ModerationEntry
		if err := rows.Scan(&m.ModerationID, &m.CommentID, &m.TenantID, &m.ModeratorPrincipalID, &m.Action, &m.Reason, &m.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
