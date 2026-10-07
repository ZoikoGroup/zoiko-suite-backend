-- BIZ-09 Comments & Collaboration — a new, dedicated platform service.
--
-- Placement decision (see the architecture review this build followed):
-- unlike every prior BIZ-0x domain this project extended into an existing
-- service, comments have no single privileged parent entity — they attach
-- symmetrically to documents, tasks, deadlines, CRM opportunities, catalog
-- offerings and any future object type. No existing service's boundary
-- could own that without owning something outside it. This mirrors the one
-- precedent in this repo for a genuinely new service over a bolt-on
-- (commercial_account/membership, "per doc7 §3's own mandatory Plane 1/
-- Plane 2 separation") rather than the many precedents for bolting on
-- (BIZ-04->workflow-svc, BIZ-05->exception-escalation-svc, BIZ-06->
-- counterparty-management-svc, BIZ-08->exception-escalation-svc again).
--
-- Authoritative ownership (doc §11, verbatim): "Own attributable contextual
-- discussion, mentions, reactions, attachments/links and moderation/edit
-- history linked to authorized business objects." This service NEVER
-- writes to the object it discusses — linked_object_type/linked_object_id
-- is an opaque, caller-supplied string pair, no FK — the exact "no FK,
-- never validated live" doctrine already proven 4+ times elsewhere in this
-- platform (exception-escalation-svc's Task/Deadline, counterparty-
-- management-svc's commercial_object_links, document-vault-svc's
-- document_links, workflow-svc's Form target_domain).
--
-- Lifecycle design decision: the doc's own line — "Active / Edited /
-- Resolved / Moderated / Deleted-Redacted; original history preserved per
-- policy" — does not say whether these states belong to a comment or a
-- thread. Resolved is modeled as a THREAD-level state (ResolveThread is
-- its own named command, operating on the whole discussion) and Active/
-- Edited/Moderated/Deleted-Redacted as COMMENT-level states (EditComment/
-- Moderate/DeleteComment all name a single comment as their target).
--
-- CommentVersion is append-only — content is never mutated in place;
-- EditComment inserts a new version and repoints comments.current_version_id.
-- This is the doc's own named entity (§16 canonical entity list:
-- "CommentVersion | BIZ-09 | Attributable comment revision history"),
-- same immutable-revision shape as document-vault-svc's DocumentVersion.
--
-- There is no named "CreateThread" command — same class of gap as BIZ-05's
-- missing CreateCase. Filled honestly: AddComment finds-or-creates the
-- thread for a given linked_object, and reopens a RESOLVED thread back to
-- ACTIVE if new discussion lands on it (a resolved thread is not a dead
-- end — new activity is real activity, not an error).

CREATE TABLE comment_threads (
    thread_id           TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    legal_entity_id     TEXT        NOT NULL,
    linked_object_type  TEXT        NOT NULL,
    linked_object_id    TEXT        NOT NULL,
    -- restricted is this wave's real, minimal implementation of the doc's
    -- own authorization note, "restricted threads require purpose and
    -- membership controls" — no generic object-instance ACL exists
    -- anywhere in this platform (confirmed against authorization-svc's own
    -- CheckAllowed, which is action-type + legal-entity scoped only), so a
    -- richer membership subsystem would be fabricated scope. A restricted
    -- thread instead gates on a distinct action type
    -- (COMMENT_READ_RESTRICTED) — a real, honest, minimal mechanism.
    restricted          BOOLEAN     NOT NULL DEFAULT false,
    status              TEXT        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RESOLVED')),
    created_by          TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_by         TEXT        NOT NULL DEFAULT '',
    resolved_at         TIMESTAMPTZ,
    resolution_note     TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (thread_id, tenant_id)
);
CREATE INDEX idx_comment_threads_linked_object ON comment_threads (tenant_id, legal_entity_id, linked_object_type, linked_object_id);
CREATE INDEX idx_comment_threads_status ON comment_threads (tenant_id, status);

CREATE TABLE comments (
    comment_id          TEXT        NOT NULL,
    thread_id           TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    -- No hard FK to comments(comment_id) — a self-reference under RLS is
    -- fine functionally, but every other opaque/optional reference in this
    -- service follows the same plain-column posture for consistency.
    parent_comment_id   TEXT,
    -- Nullable for the instant between inserting this row and inserting
    -- its first CommentVersion — comment_versions has an FK back to this
    -- table, so the comment must exist before a version referencing it
    -- can be inserted, and this column is backfilled in the same
    -- transaction immediately after. Never NULL once a caller can see it.
    current_version_id  TEXT,
    status               TEXT        NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'EDITED', 'MODERATED', 'DELETED_REDACTED')),
    created_by            TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    moderated_at                TIMESTAMPTZ,
    moderated_by_principal_id     TEXT        NOT NULL DEFAULT '',
    moderation_reason               TEXT        NOT NULL DEFAULT '',
    deleted_at                        TIMESTAMPTZ,
    deleted_by_principal_id             TEXT        NOT NULL DEFAULT '',
    deletion_reason                       TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (comment_id, tenant_id),
    FOREIGN KEY (thread_id, tenant_id) REFERENCES comment_threads (thread_id, tenant_id)
);
CREATE INDEX idx_comments_thread ON comments (tenant_id, thread_id, created_at);
CREATE INDEX idx_comments_parent ON comments (tenant_id, parent_comment_id) WHERE parent_comment_id IS NOT NULL;

-- CommentVersion — append-only. EditComment inserts here and repoints
-- comments.current_version_id; the row already pointed at stays exactly
-- as it was, forever — this table IS "original history preserved."
CREATE TABLE comment_versions (
    version_id             TEXT        NOT NULL,
    comment_id             TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    version_number         INT         NOT NULL,
    body                   TEXT        NOT NULL,
    edited_by_principal_id TEXT        NOT NULL,
    edited_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (version_id, tenant_id),
    UNIQUE (tenant_id, comment_id, version_number),
    FOREIGN KEY (comment_id, tenant_id) REFERENCES comments (comment_id, tenant_id)
);
CREATE INDEX idx_comment_versions_comment ON comment_versions (tenant_id, comment_id, version_number);

-- Mention — append-only. visibility_granted records the outcome of the
-- mention-privacy check made AT MENTION TIME (T29: "unauthorized mention
-- must not disclose object existence") — Wave 2's own real mechanism, not
-- implemented against yet in Wave 1, but the evidence column exists now so
-- Wave 2 has nothing to migrate later.
CREATE TABLE mentions (
    mention_id             TEXT        NOT NULL,
    comment_id             TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    mentioned_principal_id TEXT        NOT NULL,
    visibility_granted     BOOLEAN     NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mention_id, tenant_id),
    FOREIGN KEY (comment_id, tenant_id) REFERENCES comments (comment_id, tenant_id)
);
CREATE INDEX idx_mentions_comment ON mentions (tenant_id, comment_id);
CREATE INDEX idx_mentions_principal ON mentions (tenant_id, mentioned_principal_id);

-- Reaction — NOT append-only: React/un-react is a legitimate toggle, not
-- evidence that must never disappear. One row per (comment, principal,
-- reaction_type).
CREATE TABLE reactions (
    reaction_id   TEXT        NOT NULL,
    comment_id    TEXT        NOT NULL,
    tenant_id     TEXT        NOT NULL,
    principal_id  TEXT        NOT NULL,
    reaction_type TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (reaction_id, tenant_id),
    UNIQUE (tenant_id, comment_id, principal_id, reaction_type),
    FOREIGN KEY (comment_id, tenant_id) REFERENCES comments (comment_id, tenant_id)
);
CREATE INDEX idx_reactions_comment ON reactions (tenant_id, comment_id);

-- CommentAttachment — append-only, opaque reference. AttachReference
-- never stores bytes: a file attachment is a reference to a document
-- already stored in document-vault-svc, same "files live in the vault,
-- comments only point at them" boundary the architecture review requires.
CREATE TABLE comment_attachments (
    attachment_id            TEXT        NOT NULL,
    comment_id               TEXT        NOT NULL,
    tenant_id                TEXT        NOT NULL,
    linked_object_type       TEXT        NOT NULL DEFAULT 'DOCUMENT',
    linked_object_id         TEXT        NOT NULL,
    attached_by_principal_id TEXT        NOT NULL,
    attached_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (attachment_id, tenant_id),
    FOREIGN KEY (comment_id, tenant_id) REFERENCES comments (comment_id, tenant_id)
);
CREATE INDEX idx_comment_attachments_comment ON comment_attachments (tenant_id, comment_id);

-- ModerationTrail — append-only evidence. Moderate always requires a
-- reason (doc's own T46: "Comment moderation deletes evidence without
-- reason -> Fail; moderation history/reason required") — the NOT NULL
-- reason column plus this table's own immutability is that requirement.
CREATE TABLE moderation_trail (
    moderation_id           TEXT        NOT NULL,
    comment_id              TEXT        NOT NULL,
    tenant_id               TEXT        NOT NULL,
    moderator_principal_id  TEXT        NOT NULL,
    action                  TEXT        NOT NULL,
    reason                  TEXT        NOT NULL,
    occurred_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (moderation_id, tenant_id),
    FOREIGN KEY (comment_id, tenant_id) REFERENCES comments (comment_id, tenant_id)
);
CREATE INDEX idx_moderation_trail_comment ON moderation_trail (tenant_id, comment_id);

CREATE OR REPLACE FUNCTION reject_comments_collaboration_append_only_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_comment_version_mutation
    BEFORE UPDATE OR DELETE ON comment_versions
    FOR EACH ROW EXECUTE FUNCTION reject_comments_collaboration_append_only_mutation();

CREATE TRIGGER trg_reject_mention_mutation
    BEFORE UPDATE OR DELETE ON mentions
    FOR EACH ROW EXECUTE FUNCTION reject_comments_collaboration_append_only_mutation();

CREATE TRIGGER trg_reject_comment_attachment_mutation
    BEFORE UPDATE OR DELETE ON comment_attachments
    FOR EACH ROW EXECUTE FUNCTION reject_comments_collaboration_append_only_mutation();

CREATE TRIGGER trg_reject_moderation_trail_mutation
    BEFORE UPDATE OR DELETE ON moderation_trail
    FOR EACH ROW EXECUTE FUNCTION reject_comments_collaboration_append_only_mutation();

ALTER TABLE comment_threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE comment_threads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON comment_threads
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE comments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON comments
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE comment_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE comment_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON comment_versions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE mentions ENABLE ROW LEVEL SECURITY;
ALTER TABLE mentions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON mentions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE reactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reactions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reactions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE comment_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE comment_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON comment_attachments
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE moderation_trail ENABLE ROW LEVEL SECURITY;
ALTER TABLE moderation_trail FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON moderation_trail
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
