-- 000008_suspension_review_and_event_context.up.sql
--
-- GOV-05 negative path #3: "Emergency suspension is evidenced and reviewed".
-- 000007 evidenced it (transition log, reason, actor); this records the
-- independent review: who reviewed the suspension, when, and whether it was
-- upheld. The reviewer may not be the principal who suspended.
--
-- Doc 03 §19 event envelope: jurisdiction_context travels with each event,
-- taken from the request's X-Jurisdiction-Context.

ALTER TABLE policy_versions
    ADD COLUMN suspended_by_principal_id            TEXT,
    ADD COLUMN suspension_reviewed_by_principal_id  TEXT,
    ADD COLUMN suspension_reviewed_at               TIMESTAMPTZ,
    ADD COLUMN suspension_review_outcome            VARCHAR(16)
        CHECK (suspension_review_outcome IN ('UPHELD', 'OVERTURNED'));

-- Versions suspended before this migration: the suspender is in the log.
ALTER TABLE policy_versions NO FORCE ROW LEVEL SECURITY;
UPDATE policy_versions pv
SET suspended_by_principal_id = t.actor_id
FROM (
    SELECT DISTINCT ON (policy_version_id) policy_version_id, actor_id
    FROM policy_version_transitions
    WHERE to_status = 'SUSPENDED'
    ORDER BY policy_version_id, transition_id DESC
) t
WHERE pv.policy_version_id = t.policy_version_id
  AND pv.version_status = 'SUSPENDED';
ALTER TABLE policy_versions FORCE ROW LEVEL SECURITY;

ALTER TABLE outbox ADD COLUMN jurisdiction_context TEXT;
