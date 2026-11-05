DROP TRIGGER trg_guard_tolerance_policy ON tolerance_policies;
DROP FUNCTION guard_tolerance_policy();
DROP TRIGGER trg_guard_materiality_policy ON materiality_policies;
DROP FUNCTION guard_materiality_policy();

CREATE TRIGGER trg_immutable_tolerance BEFORE UPDATE OR DELETE ON tolerance_policies
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_materiality BEFORE UPDATE OR DELETE ON materiality_policies
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

ALTER TABLE tolerance_policies DROP COLUMN approved_at;
ALTER TABLE tolerance_policies ALTER COLUMN approved_by SET NOT NULL;

ALTER TABLE materiality_policies DROP COLUMN approved_at;
ALTER TABLE materiality_policies ALTER COLUMN approved_by SET NOT NULL;
