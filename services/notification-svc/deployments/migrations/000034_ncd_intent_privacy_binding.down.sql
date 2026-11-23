-- 000034 down: removes the NCD intent privacy binding and restores the 000016 immutability guard.
CREATE OR REPLACE FUNCTION ncd_reject_intent_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ncd_communication_intents rows are never deleted';
    END IF;
    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.purpose_class IS DISTINCT FROM OLD.purpose_class
        OR NEW.sensitivity IS DISTINCT FROM OLD.sensitivity
        OR NEW.urgency IS DISTINCT FROM OLD.urgency
        OR NEW.evidence_class IS DISTINCT FROM OLD.evidence_class
        OR NEW.allowed_channels IS DISTINCT FROM OLD.allowed_channels
        OR NEW.fallback_allowed IS DISTINCT FROM OLD.fallback_allowed
        OR NEW.marketing_allowed IS DISTINCT FROM OLD.marketing_allowed
        OR NEW.mandatory IS DISTINCT FROM OLD.mandatory
        OR NEW.preference_override_allowed IS DISTINCT FROM OLD.preference_override_allowed
        OR NEW.quiet_hours_policy IS DISTINCT FROM OLD.quiet_hours_policy
        OR NEW.bulk_allowed IS DISTINCT FROM OLD.bulk_allowed
        OR NEW.record_requirement IS DISTINCT FROM OLD.record_requirement
        OR NEW.ack_requirement IS DISTINCT FROM OLD.ack_requirement
        OR NEW.variable_contract IS DISTINCT FROM OLD.variable_contract
        OR NEW.attachment_contract IS DISTINCT FROM OLD.attachment_contract
        OR NEW.approved_url_domains IS DISTINCT FROM OLD.approved_url_domains
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'intent version %/% is immutable; create a new version', OLD.intent_id, OLD.version;
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'intent version %/% is retired and cannot change', OLD.intent_id, OLD.version;
    END IF;
    IF OLD.status = 'ACTIVE' AND NEW.status <> 'RETIRED' AND NEW.status <> 'ACTIVE' THEN
        RAISE EXCEPTION 'an ACTIVE intent version can only be retired';
    END IF;
    IF OLD.activated_at IS NOT NULL AND (NEW.activated_at IS DISTINCT FROM OLD.activated_at
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'intent version %/% activation is immutable', OLD.intent_id, OLD.version;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE ncd_communication_intents
    DROP CONSTRAINT IF EXISTS ncd_intent_privacy_binding,
    DROP COLUMN IF EXISTS privacy_purpose_id,
    DROP COLUMN IF EXISTS privacy_activity_id;
