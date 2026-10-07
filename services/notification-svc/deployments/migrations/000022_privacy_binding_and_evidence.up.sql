-- 000022_privacy_binding_and_evidence.up.sql
-- ZS-SVC-Y-001 NCD-02 section 5.3 (privacy permission), INV-09, INV-30, NP-17, NP-18,
-- Wave 3 slice 1.
--
-- Sending a message uses the recipient's personal data (their contact endpoint) for a
-- purpose, and the platform's privacy decision service (PRV, privacy-decision-svc) is
-- the authority on whether that use is permitted. This migration gives that decision a
-- home on both sides of the send:
--
-- 1. An intent VERSION names the privacy processing activity and purpose it runs under
--    (privacy_activity_id, privacy_purpose_id; both or neither), reviewed and frozen with
--    the rest of the version. The sender never chooses them.
--
-- 2. Every delivery ATTEMPT records the privacy decision that governed it
--    (privacy_decision_id) and its result (privacy_result), including a refusal, so a
--    message can be traced to the decision that allowed or stopped it.
--
-- All columns are nullable: an intent without a privacy binding, and every attempt made
-- before the gate existed or without it, records nothing, exactly as before.

ALTER TABLE communication_intent_versions
    ADD COLUMN IF NOT EXISTS privacy_activity_id VARCHAR(128),
    ADD COLUMN IF NOT EXISTS privacy_purpose_id  VARCHAR(128);

ALTER TABLE communication_intent_versions
    DROP CONSTRAINT IF EXISTS ck_intent_privacy_binding,
    ADD CONSTRAINT ck_intent_privacy_binding CHECK (
        (privacy_activity_id IS NULL) = (privacy_purpose_id IS NULL)
        AND (privacy_activity_id IS NULL OR privacy_activity_id ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$')
        AND (privacy_purpose_id  IS NULL OR privacy_purpose_id  ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'));

ALTER TABLE notification_delivery_attempts
    ADD COLUMN IF NOT EXISTS privacy_decision_id VARCHAR(128),
    ADD COLUMN IF NOT EXISTS privacy_result      VARCHAR(24);

ALTER TABLE notification_delivery_attempts
    DROP CONSTRAINT IF EXISTS nda_privacy_result_known,
    ADD CONSTRAINT nda_privacy_result_known CHECK (
        privacy_result IS NULL OR privacy_result IN
            ('PERMIT', 'RESTRICT', 'BLOCK', 'REVIEW_REQUIRED', 'INDETERMINATE', 'UNAVAILABLE', 'NOT_BOUND'));

-- The binding is part of the version's frozen content: extend the guard from 000020.
CREATE OR REPLACE FUNCTION guard_intent_version() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'an intent version is created as DRAFT, not %', NEW.status USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'intent versions are never deleted' USING ERRCODE = '23514';
    END IF;

    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id
       OR NEW.version_number IS DISTINCT FROM OLD.version_number
       OR NEW.purpose_class IS DISTINCT FROM OLD.purpose_class
       OR NEW.evidence_class IS DISTINCT FROM OLD.evidence_class
       OR NEW.allowed_channels IS DISTINCT FROM OLD.allowed_channels
       OR NEW.marketing_allowed IS DISTINCT FROM OLD.marketing_allowed
       OR NEW.record_requirement IS DISTINCT FROM OLD.record_requirement
       OR NEW.variable_contract IS DISTINCT FROM OLD.variable_contract
       OR NEW.privacy_activity_id IS DISTINCT FROM OLD.privacy_activity_id
       OR NEW.privacy_purpose_id IS DISTINCT FROM OLD.privacy_purpose_id
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'intent version % is immutable once written; correct it with a new version', OLD.version_id USING ERRCODE = '23514';
    END IF;
    IF OLD.approved_at IS NOT NULL AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'intent version % is already approved', OLD.version_id USING ERRCODE = '23514';
    END IF;
    IF OLD.published_at IS NOT NULL AND (NEW.published_at IS DISTINCT FROM OLD.published_at OR NEW.effective_from IS DISTINCT FROM OLD.effective_from) THEN
        RAISE EXCEPTION 'intent version % is already published; its dates cannot change', OLD.version_id USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'DRAFT'    AND NEW.status = 'REVIEW'    AND NEW.validated_at IS NOT NULL) OR
        (OLD.status = 'REVIEW'   AND NEW.status = 'APPROVED'  AND NEW.approved_at IS NOT NULL) OR
        (OLD.status = 'APPROVED' AND NEW.status = 'PUBLISHED' AND NEW.published_at IS NOT NULL AND NEW.effective_from IS NOT NULL)) THEN
        RAISE EXCEPTION 'intent version % cannot move from % to % (or is missing the evidence that move requires)',
            OLD.version_id, OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;

    IF NEW.status = 'PUBLISHED' AND OLD.status <> 'PUBLISHED' THEN
        IF NEW.effective_from < NEW.published_at THEN
            RAISE EXCEPTION 'an intent version cannot be effective before it is published (no backdating)' USING ERRCODE = '23514';
        END IF;
        IF EXISTS (SELECT 1 FROM communication_intent_versions v
                   WHERE v.intent_id = NEW.intent_id AND v.version_id <> NEW.version_id
                     AND v.effective_from IS NOT NULL AND v.effective_from >= NEW.effective_from) THEN
            RAISE EXCEPTION 'an intent version must be effective after every version already published for the intent' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
