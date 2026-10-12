DROP TRIGGER IF EXISTS access_decision_log_no_update ON access_decision_log;
DROP FUNCTION IF EXISTS access_decision_log_immutable();

ALTER TABLE access_decision_log
    DROP COLUMN IF EXISTS decision,
    DROP COLUMN IF EXISTS policy_set_version,
    DROP COLUMN IF EXISTS obligations,
    DROP COLUMN IF EXISTS reason_codes,
    DROP COLUMN IF EXISTS matched_grants,
    DROP COLUMN IF EXISTS resource_type,
    DROP COLUMN IF EXISTS resource_id,
    DROP COLUMN IF EXISTS resource_version,
    DROP COLUMN IF EXISTS attributes_digest,
    DROP COLUMN IF EXISTS session_assurance,
    DROP COLUMN IF EXISTS on_behalf_of,
    DROP COLUMN IF EXISTS delegation_id,
    DROP COLUMN IF EXISTS expires_at;
