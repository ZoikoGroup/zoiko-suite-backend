// Package domain defines the authoritative domain types for
// comments-collaboration-svc — BIZ-09 (Comments & Collaboration) from
// ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0.docx.
//
// See migration 000001's own doc comment for the full placement rationale
// (why this is a new service, not a bolt-on) and the design decisions this
// package encodes (thread- vs comment-level lifecycle, no CreateThread
// command, append-only CommentVersion).
package domain

import (
	"errors"
	"time"
)

type ThreadStatus string

const (
	ThreadStatusActive   ThreadStatus = "ACTIVE"
	ThreadStatusResolved ThreadStatus = "RESOLVED"
)

type CommentStatus string

const (
	CommentStatusActive          CommentStatus = "ACTIVE"
	CommentStatusEdited          CommentStatus = "EDITED"
	CommentStatusModerated       CommentStatus = "MODERATED"
	CommentStatusDeletedRedacted CommentStatus = "DELETED_REDACTED"
)

// CommentThread is BIZ-09's own "CommentThread" entity (doc §16 canonical
// entity list). Attached to exactly one linked_object via an opaque,
// caller-supplied type+id pair — never a foreign key, same posture as
// every other cross-service reference in this platform.
type CommentThread struct {
	ThreadID         string       `json:"thread_id"`
	TenantID         string       `json:"tenant_id"`
	LegalEntityID    string       `json:"legal_entity_id"`
	LinkedObjectType string       `json:"linked_object_type"`
	LinkedObjectID   string       `json:"linked_object_id"`
	Restricted       bool         `json:"restricted"`
	Status           ThreadStatus `json:"status"`
	CreatedBy        string       `json:"created_by"`
	CreatedAt        time.Time    `json:"created_at"`
	ResolvedBy       string       `json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time   `json:"resolved_at,omitempty"`
	ResolutionNote   string       `json:"resolution_note,omitempty"`
}

// Comment is one message in a thread, optionally a reply to another
// comment in the same thread. Its own content never lives here —
// CurrentVersionID always points at the latest CommentVersion; Body is
// never a column on this table.
type Comment struct {
	CommentID              string        `json:"comment_id"`
	ThreadID               string        `json:"thread_id"`
	TenantID               string        `json:"tenant_id"`
	ParentCommentID        string        `json:"parent_comment_id,omitempty"`
	CurrentVersionID       string        `json:"current_version_id"`
	Status                 CommentStatus `json:"status"`
	CreatedBy              string        `json:"created_by"`
	CreatedAt              time.Time     `json:"created_at"`
	UpdatedAt              time.Time     `json:"updated_at"`
	ModeratedAt            *time.Time    `json:"moderated_at,omitempty"`
	ModeratedByPrincipalID string        `json:"moderated_by_principal_id,omitempty"`
	ModerationReason       string        `json:"moderation_reason,omitempty"`
	DeletedAt              *time.Time    `json:"deleted_at,omitempty"`
	DeletedByPrincipalID   string        `json:"deleted_by_principal_id,omitempty"`
	DeletionReason         string        `json:"deletion_reason,omitempty"`

	// CurrentBody is populated by the store as a read-time join against
	// comment_versions — not a real column, a convenience for callers so
	// they don't have to make a second GetCommentHistory call just to see
	// what a comment currently says.
	CurrentBody string `json:"current_body,omitempty"`
}

// CommentVersion is BIZ-09's own "CommentVersion" entity (doc §16:
// "Attributable comment revision history") — append-only. EditComment
// inserts a new row here; nothing already written is ever changed.
type CommentVersion struct {
	VersionID           string    `json:"version_id"`
	CommentID           string    `json:"comment_id"`
	TenantID            string    `json:"tenant_id"`
	VersionNumber       int       `json:"version_number"`
	Body                string    `json:"body"`
	EditedByPrincipalID string    `json:"edited_by_principal_id"`
	EditedAt            time.Time `json:"edited_at"`
}

// Mention — Wave 2's own entity; declared here so callers/tests can
// reference the type ahead of the commands that create them.
type Mention struct {
	MentionID            string    `json:"mention_id"`
	CommentID            string    `json:"comment_id"`
	TenantID             string    `json:"tenant_id"`
	MentionedPrincipalID string    `json:"mentioned_principal_id"`
	VisibilityGranted    bool      `json:"visibility_granted"`
	CreatedAt            time.Time `json:"created_at"`
}

type Reaction struct {
	ReactionID   string    `json:"reaction_id"`
	CommentID    string    `json:"comment_id"`
	TenantID     string    `json:"tenant_id"`
	PrincipalID  string    `json:"principal_id"`
	ReactionType string    `json:"reaction_type"`
	CreatedAt    time.Time `json:"created_at"`
}

type CommentAttachment struct {
	AttachmentID          string    `json:"attachment_id"`
	CommentID             string    `json:"comment_id"`
	TenantID              string    `json:"tenant_id"`
	LinkedObjectType      string    `json:"linked_object_type"`
	LinkedObjectID        string    `json:"linked_object_id"`
	AttachedByPrincipalID string    `json:"attached_by_principal_id"`
	AttachedAt            time.Time `json:"attached_at"`
}

type ModerationEntry struct {
	ModerationID         string    `json:"moderation_id"`
	CommentID            string    `json:"comment_id"`
	TenantID             string    `json:"tenant_id"`
	ModeratorPrincipalID string    `json:"moderator_principal_id"`
	Action               string    `json:"action"`
	Reason               string    `json:"reason"`
	OccurredAt           time.Time `json:"occurred_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// AddCommentParams — BIZ-09's own AddComment command. Finds-or-creates
// the thread for LinkedObjectType/LinkedObjectID; a RESOLVED thread found
// this way is reopened to ACTIVE as part of adding the new comment — see
// migration 000001's own doc comment on why.
type AddCommentParams struct {
	TenantID, LegalEntityID          string
	LinkedObjectType, LinkedObjectID string
	Restricted                       bool
	ParentCommentID                  string
	Body                             string
	CreatedByPrincipalID             string
}

// EditCommentParams — BIZ-09's own EditComment command. Never mutates an
// existing CommentVersion; always inserts a new one.
type EditCommentParams struct {
	CommentID, TenantID, ActorPrincipalID, Body string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrThreadNotFound  = errors.New("comment thread not found")
	ErrCommentNotFound = errors.New("comment not found")

	ErrCommentInvalidState = errors.New("comment is not in a state that permits this action")

	ErrEmptyBody = errors.New("body is required")

	// ErrParentCommentNotInThread guards replies — a ParentCommentID must
	// belong to the same thread the new comment is being added to,
	// otherwise a reply could silently cross discussions.
	ErrParentCommentNotInThread = errors.New("parent_comment_id does not belong to this thread")
)
