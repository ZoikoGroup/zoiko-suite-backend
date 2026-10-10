-- Immutability, enforced in the database so that neither a bug in this service
-- nor a fix-up script can rewrite a calendar's history ("never rewrite
-- historical periods"; corrections create a new version).

CREATE OR REPLACE FUNCTION fiscal_calendar_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is forbidden: this table is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION fiscal_calendar_forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'DELETE on % is forbidden: supersede by a new version, never delete', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- Append-only evidence.
CREATE TRIGGER trg_fcsh_append_only
    BEFORE UPDATE OR DELETE ON fiscal_calendar_status_history
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendar_forbid_mutation();

-- Updatable (lifecycle columns only) but never deletable.
CREATE TRIGGER trg_fiscal_calendars_no_delete
    BEFORE DELETE ON fiscal_calendars
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendar_forbid_delete();
CREATE TRIGGER trg_fcv_no_delete
    BEFORE DELETE ON fiscal_calendar_versions
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendar_forbid_delete();
CREATE TRIGGER trg_ctp_no_delete
    BEFORE DELETE ON calendar_transition_plans
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendar_forbid_delete();

-- Calendar header: identity is immutable; status only moves DRAFT -> ACTIVE.
CREATE OR REPLACE FUNCTION fiscal_calendars_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.calendar_id <> OLD.calendar_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.code <> OLD.code OR NEW.scope <> OLD.scope OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'calendar identity (id, tenant, legal entity, code, scope) is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.status <> OLD.status AND NOT (OLD.status = 'DRAFT' AND NEW.status = 'ACTIVE') THEN
        RAISE EXCEPTION 'calendar status cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'calendar version must increase' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_fiscal_calendars_guard
    BEFORE UPDATE ON fiscal_calendars
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendars_guard();

-- Calendar version: the definition (pattern, start anchor, effective_from,
-- identity) NEVER changes, in any status. effective_to changes exactly once,
-- from NULL, when an ACTIVE version is superseded (end-dating, not rewriting).
-- Status only moves DRAFT -> APPROVED -> ACTIVE -> SUPERSEDED.
CREATE OR REPLACE FUNCTION fiscal_calendar_versions_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.version_id <> OLD.version_id OR NEW.calendar_id <> OLD.calendar_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.scope <> OLD.scope OR NEW.version_no <> OLD.version_no
       OR NEW.pattern IS DISTINCT FROM OLD.pattern
       OR NEW.fiscal_year_start_month <> OLD.fiscal_year_start_month OR NEW.fiscal_year_start_day <> OLD.fiscal_year_start_day
       OR NEW.effective_from <> OLD.effective_from
       OR NEW.proposed_by <> OLD.proposed_by OR NEW.proposal_reason <> OLD.proposal_reason THEN
        RAISE EXCEPTION 'a calendar version definition (pattern, fiscal-year start, effective_from, identity) is immutable; propose a new version' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a SUPERSEDED calendar version can no longer change' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.effective_to IS DISTINCT FROM OLD.effective_to THEN
        IF NOT (OLD.effective_to IS NULL AND OLD.status = 'ACTIVE' AND NEW.status = 'SUPERSEDED') THEN
            RAISE EXCEPTION 'effective_to may only be set once, when an ACTIVE version is superseded' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'DRAFT'    AND NEW.status = 'APPROVED')
        OR (OLD.status = 'APPROVED' AND NEW.status = 'ACTIVE')
        OR (OLD.status = 'ACTIVE'   AND NEW.status = 'SUPERSEDED')) THEN
        RAISE EXCEPTION 'calendar version status cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
    END IF;
    IF (OLD.approved_by IS NOT NULL AND NEW.approved_by IS DISTINCT FROM OLD.approved_by)
       OR (OLD.activated_by IS NOT NULL AND NEW.activated_by IS DISTINCT FROM OLD.activated_by) THEN
        RAISE EXCEPTION 'approval and activation evidence is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'calendar version number must increase' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_fcv_guard
    BEFORE UPDATE ON fiscal_calendar_versions
    FOR EACH ROW EXECUTE FUNCTION fiscal_calendar_versions_guard();

-- Transition plan: content is immutable; only the decision may be recorded,
-- once (PROPOSED -> APPROVED | REJECTED).
CREATE OR REPLACE FUNCTION calendar_transition_plans_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.plan_id <> OLD.plan_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.calendar_id <> OLD.calendar_id OR NEW.from_version_id <> OLD.from_version_id OR NEW.to_version_id <> OLD.to_version_id
       OR NEW.impact_assessment IS DISTINCT FROM OLD.impact_assessment OR NEW.mapping IS DISTINCT FROM OLD.mapping
       OR NEW.affects_posted_periods <> OLD.affects_posted_periods
       OR NEW.proposed_by <> OLD.proposed_by OR NEW.reason <> OLD.reason OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a transition plan content is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.status <> 'PROPOSED' THEN
        RAISE EXCEPTION 'a % transition plan can no longer change', OLD.status USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.version <= OLD.version THEN
        RAISE EXCEPTION 'plan version must increase' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_ctp_guard
    BEFORE UPDATE ON calendar_transition_plans
    FOR EACH ROW EXECUTE FUNCTION calendar_transition_plans_guard();
