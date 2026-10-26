-- A group adjustment submitted by another service (consolidation-svc) names the record it comes from, so that a retried submission is the
-- same adjustment and never a second one.

ALTER TABLE consolidation_adjustments ADD COLUMN source_ref VARCHAR(255);
CREATE UNIQUE INDEX uq_consolidation_adjustments_source ON consolidation_adjustments (tenant_id, source_ref) WHERE source_ref IS NOT NULL;

CREATE OR REPLACE FUNCTION consolidation_adjustment_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a group adjustment is never deleted: withdraw it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.adjustment_id, NEW.tenant_id, NEW.group_id, NEW.fiscal_period, NEW.description, NEW.policy_ref, NEW.evidence_ref, NEW.lines::text, NEW.created_by, NEW.created_at, NEW.source_ref)
       IS DISTINCT FROM
       (OLD.adjustment_id, OLD.tenant_id, OLD.group_id, OLD.fiscal_period, OLD.description, OLD.policy_ref, OLD.evidence_ref, OLD.lines::text, OLD.created_by, OLD.created_at, OLD.source_ref) THEN
        RAISE EXCEPTION 'the content of a group adjustment is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT ((OLD.status = 'PROPOSED' AND NEW.status IN ('APPROVED', 'WITHDRAWN')) OR (OLD.status = 'APPROVED' AND NEW.status = 'WITHDRAWN') OR OLD.status = NEW.status) THEN
        RAISE EXCEPTION 'group adjustment status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'WITHDRAWN' THEN
        RAISE EXCEPTION 'a withdrawn adjustment is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
