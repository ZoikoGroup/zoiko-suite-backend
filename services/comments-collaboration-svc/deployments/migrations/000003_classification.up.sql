-- BIZ-09 classification input and inheritance.
--
-- BIZ-09 lists "classification/purpose" among its required inputs and states
-- that comments, mentions and collaboration attachments "SHALL preserve
-- edit/delete/moderation history and classification inheritance"; §15 adds
-- that every comment derivative must retain "source/tenant/classification
-- lineage". Purpose already arrives through the envelope's purpose_context;
-- this migration adds the classification half.
--
-- Vocabulary: the platform's data-classification baseline
-- (04-data-model.md §20.1), already enforced the same way by
-- document-vault-svc: PUBLIC, INTERNAL, CONFIDENTIAL, RESTRICTED. BIZ-09 does
-- not define a vocabulary of its own, so none is invented here.
--
-- Where it lives: on the thread (set when the thread is created, from the
-- classification of the linked object as asserted by the caller) and copied
-- onto each comment when it is written — the comment "inherits" the thread's
-- classification and keeps that snapshot as its own lineage evidence.
--
-- Nullable on purpose: rows written before this migration have no recorded
-- classification and none is fabricated for them. NULL means "not recorded".
-- New threads must carry one (enforced by the service).
--
-- Not defined by BIZ-09, deliberately not decided here: whether RESTRICTED
-- implies a restricted thread, and whether the classification should be looked
-- up from the owning service instead of asserted by the caller.

ALTER TABLE comment_threads
    ADD COLUMN classification TEXT
        CHECK (classification IS NULL OR classification IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED'));

ALTER TABLE comments
    ADD COLUMN classification TEXT
        CHECK (classification IS NULL OR classification IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED'));
