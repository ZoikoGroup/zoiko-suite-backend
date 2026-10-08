-- Migration: 000016_add_close_requirements.up.sql
--
-- ACC-14's close checklist (ZS-SVC-B-001 §17: the close owns "blockers,
-- checklist items, approvals"), per legal entity.
--
-- ZS-CONTROL-001 §22 makes a close depend on "Mandatory AR/AP/Payroll/Tax/
-- Assets/Inventory controls" and on "bank reconciliations complete for
-- material accounts". Which controls are mandatory and which accounts are
-- material is per-entity policy, recorded here explicitly rather than
-- inferred from whatever data happens to exist.
--
--   SUBLEDGER_CONTROL       a subledger-to-GL control the entity's close
--                           requires beyond the AR/AP baseline. AR and AP are
--                           required for every entity and are deliberately
--                           not representable here, so they cannot be removed.
--                           ASSETS names the asset book it reconciles.
--   BANK_ACCOUNT_EXCLUSION  a bank account taken OUT of the close's bank
--                           reconciliation requirement (every account is in
--                           by default), with the reason it is immaterial.
--
-- Rows are never deleted. Removing a requirement stamps who, when and why —
-- ZS-CONTROL-001 forbids silently deleting a control, and a close's evidence
-- must be able to say which checklist applied when it ran.

CREATE TABLE close_requirements (
    requirement_id           UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    kind                     VARCHAR(32)  NOT NULL,
    subledger                VARCHAR(32),
    book_id                  VARCHAR(255),
    bank_account_id          VARCHAR(255),
    reason                   TEXT         NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    removed_at               TIMESTAMPTZ,
    removed_by_principal_id  VARCHAR(255),
    removal_reason           TEXT,

    CONSTRAINT close_requirements_shape CHECK (
        (kind = 'SUBLEDGER_CONTROL'
            AND subledger IN ('ASSETS', 'DEPRECIATION_COMPLETENESS', 'INVENTORY_QUANTITY',
                              'INVENTORY_VALUE', 'PROJECT_REVENUE', 'STOCK_COUNT')
            AND bank_account_id IS NULL
            AND ((subledger = 'ASSETS') = (book_id IS NOT NULL AND btrim(book_id) <> '')))
        OR
        (kind = 'BANK_ACCOUNT_EXCLUSION'
            AND subledger IS NULL AND book_id IS NULL
            AND bank_account_id IS NOT NULL AND btrim(bank_account_id) <> ''
            AND btrim(reason) <> '')
    ),
    CONSTRAINT close_requirements_removal_complete CHECK (
        (removed_at IS NULL AND removed_by_principal_id IS NULL AND removal_reason IS NULL)
        OR (removed_at IS NOT NULL AND removed_by_principal_id IS NOT NULL
            AND removal_reason IS NOT NULL AND btrim(removal_reason) <> '')
    )
);

-- One active row per requirement: adding the same one twice is a replay.
CREATE UNIQUE INDEX close_requirements_active
    ON close_requirements (tenant_id, legal_entity_id, kind,
                           (COALESCE(subledger, '')), (COALESCE(book_id, '')), (COALESCE(bank_account_id, '')))
    WHERE removed_at IS NULL;

-- Only the removal stamp may ever be written after insert, and only once.
CREATE OR REPLACE FUNCTION guard_close_requirement_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'close_requirements rows are never deleted; remove a requirement instead';
    END IF;
    IF OLD.removed_at IS NOT NULL THEN
        RAISE EXCEPTION 'close requirement % is already removed', OLD.requirement_id;
    END IF;
    IF (NEW.requirement_id, NEW.tenant_id, NEW.legal_entity_id, NEW.kind, NEW.subledger, NEW.book_id,
        NEW.bank_account_id, NEW.reason, NEW.created_at, NEW.created_by_principal_id)
       IS DISTINCT FROM
       (OLD.requirement_id, OLD.tenant_id, OLD.legal_entity_id, OLD.kind, OLD.subledger, OLD.book_id,
        OLD.bank_account_id, OLD.reason, OLD.created_at, OLD.created_by_principal_id) THEN
        RAISE EXCEPTION 'a close requirement is immutable except for its removal stamp';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER close_requirements_guard_update
    BEFORE UPDATE ON close_requirements
    FOR EACH ROW EXECUTE FUNCTION guard_close_requirement_mutation();
CREATE TRIGGER close_requirements_guard_delete
    BEFORE DELETE ON close_requirements
    FOR EACH ROW EXECUTE FUNCTION guard_close_requirement_mutation();

ALTER TABLE close_requirements ENABLE ROW LEVEL SECURITY;
ALTER TABLE close_requirements FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON close_requirements
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
