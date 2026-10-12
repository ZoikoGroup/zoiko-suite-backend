-- 000030_merge_event_outbox_union.up.sql
-- 7 Oct 2026 merge of origin/main into shashi-changes.
--
-- Two migration lineages each redefine event_outbox_event_known as a full list:
-- ours in 000021_ncd_canonical_events (the NCD control plane's events), main's in
-- 000025 / 000027 / 000028 (the register's notice, cancel and evidence events).
-- Whichever ran last won, so a database that applied main's after ours refused
-- communication.correction.issued and communication.record.declared, and one that applied ours after main's refused
-- main's notice.* and communication.cancelled events.
--
-- This migration sorts after both lineages on every database, however it was
-- reached, and sets the union. Any later migration that adds an event type must
-- start from this list.

ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent',
        'notification.failed',
        'notification.outcome_unknown',
        'template.created',
        'template.version_approved',
        'template.published',
        'template.retired',
        'delivery.attempt.created',
        'delivery.attempt.unknown',
        'notice.dispatched',
        'notice.delivery_evidenced',
        'notice.exception',
        'notice.acknowledged',
        'notice.declined',
        'notice.disputed',
        'notice.expired',
        'notice.corrected',
        'communication.prepared',
        'communication.blocked',
        'delivery.evidence.recorded',
        'endpoint.suppressed',
        'notice.deadline.at_risk',
        'communication.cancelled',
        'communication.correction.issued',
        'communication.record.declared'
    ));
