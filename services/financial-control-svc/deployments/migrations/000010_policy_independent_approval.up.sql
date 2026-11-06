-- SPEC_DEVIATIONS A4: tolerance and materiality approval was self-declared —
-- CreateTolerancePolicy/CreateMaterialityPolicy took approved_by as a caller-
-- supplied free-text field, so one principal could create and "approve" a
-- wider tolerance by typing a colleague's id. No second authenticated actor
-- ever approved it. This mirrors control_rule_versions' already-governed
-- shape: approved_by/approved_at become nullable, set only by a later,
-- separately-authorized approve command.
--
-- The existing CHECK (approved_by <> created_by) needs no change: Postgres
-- treats a CHECK as satisfied when the expression is NULL, not just TRUE, so
-- "approved_by <> created_by" already admits a NULL (pending) approved_by.
--
-- Both tables were (000001) made blanket append-only via
-- trg_immutable_tolerance/trg_immutable_materiality, which reject ANY
-- UPDATE — including the one the approve command now needs to make. Replace
-- both with a guard mirroring control_rule_versions' own guard_rule_version
-- (000001): only an unapproved row may be updated, and only its approval
-- columns may change.

ALTER TABLE tolerance_policies
    ALTER COLUMN approved_by DROP NOT NULL,
    ADD COLUMN approved_at TIMESTAMPTZ;

ALTER TABLE materiality_policies
    ALTER COLUMN approved_by DROP NOT NULL,
    ADD COLUMN approved_at TIMESTAMPTZ;

DROP TRIGGER trg_immutable_tolerance ON tolerance_policies;
DROP TRIGGER trg_immutable_materiality ON materiality_policies;

CREATE OR REPLACE FUNCTION guard_tolerance_policy() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'tolerance_policies rows can never be deleted';
    END IF;
    IF OLD.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'approved tolerance_policies rows are immutable';
    END IF;
    IF NEW.tolerance_id <> OLD.tolerance_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.metric <> OLD.metric
       OR NEW.tolerance_version <> OLD.tolerance_version
       OR NEW.absolute_tolerance <> OLD.absolute_tolerance OR NEW.percentage_tolerance <> OLD.percentage_tolerance
       OR NEW.percentage_base <> OLD.percentage_base OR NEW.date_tolerance_days <> OLD.date_tolerance_days
       OR NEW.date_basis <> OLD.date_basis OR NEW.currency <> OLD.currency
       OR NEW.permitted_contexts <> OLD.permitted_contexts OR NEW.rationale <> OLD.rationale
       OR NEW.effective_from <> OLD.effective_from OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'only approval columns of tolerance_policies may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_tolerance_policy BEFORE UPDATE OR DELETE ON tolerance_policies
    FOR EACH ROW EXECUTE FUNCTION guard_tolerance_policy();

CREATE OR REPLACE FUNCTION guard_materiality_policy() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'materiality_policies rows can never be deleted';
    END IF;
    IF OLD.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'approved materiality_policies rows are immutable';
    END IF;
    IF NEW.materiality_id <> OLD.materiality_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.reporting_basis <> OLD.reporting_basis
       OR NEW.materiality_version <> OLD.materiality_version
       OR NEW.amount_threshold <> OLD.amount_threshold OR NEW.aggregate_threshold <> OLD.aggregate_threshold
       OR NEW.currency <> OLD.currency OR NEW.qualitative_triggers <> OLD.qualitative_triggers
       OR NEW.aggregation_basis <> OLD.aggregation_basis OR NEW.effective_from <> OLD.effective_from
       OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'only approval columns of materiality_policies may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_materiality_policy BEFORE UPDATE OR DELETE ON materiality_policies
    FOR EACH ROW EXECUTE FUNCTION guard_materiality_policy();
