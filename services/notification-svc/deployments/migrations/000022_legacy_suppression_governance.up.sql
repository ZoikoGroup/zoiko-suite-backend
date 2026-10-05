-- 000022: the legacy suppression list gets the canonical list's governance
-- (ZS-SVC-Y-001 §7.3, INV-10).
--
-- email_suppressions is still written by the provider webhook and the legacy
-- admin API, and the send gate reads it alongside ncd_suppressions. Until now
-- a row could be DELETEd by any one principal holding NOTIFICATION_SUPPRESS —
-- a recorded hard bounce or complaint lifted with no evidence and no second
-- approver, and the row gone, so nothing showed it had ever existed. §7.3:
-- "reactivation of a hard-bounced or complaint-suppressed endpoint requires
-- governed evidence of correction ... operator toggles alone are insufficient".
--
-- Now a legacy row is lifted exactly like a canonical one — through
-- POST /v1/suppressions/{id}/lift, with evidence, and with a second principal
-- for HARD_BOUNCE, COMPLAINT and ADMIN_SUPPRESSED — and it is never deleted.

ALTER TABLE email_suppressions
    ADD COLUMN IF NOT EXISTS lifted_at                     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS lifted_by_principal_id        VARCHAR(255),
    ADD COLUMN IF NOT EXISTS lift_evidence_ref             VARCHAR(500),
    ADD COLUMN IF NOT EXISTS lift_approved_by_principal_id VARCHAR(255);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'email_supp_lift_has_evidence') THEN
        ALTER TABLE email_suppressions ADD CONSTRAINT email_supp_lift_has_evidence CHECK (
            lifted_at IS NULL OR (lift_evidence_ref IS NOT NULL AND lift_evidence_ref <> '' AND lifted_by_principal_id IS NOT NULL));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'email_supp_governed_reactivation') THEN
        ALTER TABLE email_suppressions ADD CONSTRAINT email_supp_governed_reactivation CHECK (
            lifted_at IS NULL OR reason NOT IN ('HARD_BOUNCE','COMPLAINT','ADMIN_SUPPRESSED')
            OR (lift_approved_by_principal_id IS NOT NULL AND lift_approved_by_principal_id <> lifted_by_principal_id));
    END IF;
END $$;

-- Only an ACTIVE row is unique per (tenant, address, stream). A lifted row
-- stays as the record of the lift, and a later bounce for the same address
-- inserts a new active row beside it instead of overwriting that record.
DROP INDEX IF EXISTS idx_email_suppressions_tenant_email_stream;
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_suppressions_active_tenant_email_stream
    ON email_suppressions (tenant_id, recipient_email, source_stream) WHERE lifted_at IS NULL;

-- Rows are never deleted, a lifted row never changes again, and lifting
-- changes nothing but the lift columns. An active row may still be
-- strengthened in place by the upsert (UNSUBSCRIBE -> HARD_BOUNCE); the
-- upsert's own predicate refuses to weaken one.
CREATE OR REPLACE FUNCTION email_supp_reject_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'email_suppressions rows are never deleted; lift them with evidence (POST /v1/suppressions/{id}/lift)';
    END IF;
    IF OLD.lifted_at IS NOT NULL THEN
        RAISE EXCEPTION 'suppression % is already lifted', OLD.suppression_id;
    END IF;
    IF NEW.suppression_id IS DISTINCT FROM OLD.suppression_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.recipient_email IS DISTINCT FROM OLD.recipient_email
        OR NEW.source_stream IS DISTINCT FROM OLD.source_stream
    THEN
        RAISE EXCEPTION 'suppression % keeps its identity, tenant, address and stream', OLD.suppression_id;
    END IF;
    IF NEW.lifted_at IS NOT NULL AND (NEW.reason IS DISTINCT FROM OLD.reason OR NEW.created_at IS DISTINCT FROM OLD.created_at) THEN
        RAISE EXCEPTION 'a lift changes nothing but the lift columns of suppression %', OLD.suppression_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_email_supp_reject_mutation ON email_suppressions;
CREATE TRIGGER trg_email_supp_reject_mutation
    BEFORE UPDATE OR DELETE ON email_suppressions
    FOR EACH ROW EXECUTE FUNCTION email_supp_reject_mutation();
