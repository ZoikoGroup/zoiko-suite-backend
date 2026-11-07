-- Doc 06 §21, Doc 09 §05/§22: every operator control action (flag and kill
-- switch changes, release promotion and rollback, revocation, dead-letter
-- replay) is recorded with actor, approval, reason and before/after hashes.
-- Append-only: a trigger rejects UPDATE and DELETE, like the revocations
-- ledger, so the record is tamper-evident.
CREATE TABLE public_ai.control_audit (
    audit_id     BIGSERIAL PRIMARY KEY,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL,
    actor        TEXT NOT NULL CHECK (actor <> ''),
    approval     TEXT NOT NULL CHECK (approval <> ''),
    reason       TEXT NOT NULL DEFAULT '',
    before_hash  TEXT NOT NULL DEFAULT '',
    after_hash   TEXT NOT NULL DEFAULT '',
    environment  TEXT NOT NULL DEFAULT '',
    at           TIMESTAMPTZ NOT NULL
);
CREATE FUNCTION public_ai.reject_control_audit_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'control_audit is append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER control_audit_append_only
    BEFORE UPDATE OR DELETE ON public_ai.control_audit
    FOR EACH ROW EXECUTE FUNCTION public_ai.reject_control_audit_change();
