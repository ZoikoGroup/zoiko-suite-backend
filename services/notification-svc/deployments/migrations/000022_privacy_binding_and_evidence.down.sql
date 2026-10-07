-- 000022 down: removes the privacy binding and the per-attempt privacy evidence, and restores
-- the 000020 intent version guard.
ALTER TABLE notification_delivery_attempts
    DROP CONSTRAINT IF EXISTS nda_privacy_result_known,
    DROP COLUMN IF EXISTS privacy_result,
    DROP COLUMN IF EXISTS privacy_decision_id;

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

ALTER TABLE communication_intent_versions
    DROP CONSTRAINT IF EXISTS ck_intent_privacy_binding,
    DROP COLUMN IF EXISTS privacy_purpose_id,
    DROP COLUMN IF EXISTS privacy_activity_id;
