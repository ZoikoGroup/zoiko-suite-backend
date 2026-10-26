-- ACC-01 A1: an account has an entity/book dimension. The chart stays tenant-wide for what an account IS (its code, type and control
-- nature mean the same thing everywhere in a tenant, and every journal, ledger entry and mapping keeps pointing at it), and gains
-- entity- and book-scoped VERSIONS for what can legitimately differ by entity or book: the name, the place in the hierarchy and whether
-- the account is open for use. The most specific version in force on a date wins: book, then entity, then the tenant version.
-- Each row is append-only except that a newer version closes the one before it.

CREATE TABLE account_scope_versions (
    scope_version_id    UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    account_id          UUID         NOT NULL REFERENCES chart_of_accounts(account_id),
    account_code        VARCHAR(64)  NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    accounting_book_id  UUID         REFERENCES accounting_books(book_id),
    version             INTEGER      NOT NULL,
    account_name        TEXT,
    parent_account_id   UUID         REFERENCES chart_of_accounts(account_id),
    status              VARCHAR(20),
    effective_from      DATE         NOT NULL,
    effective_to        DATE,
    change_reason       TEXT         NOT NULL,
    evidence_refs       TEXT[],
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_by          VARCHAR(255) NOT NULL,
    CONSTRAINT account_scope_versions_status CHECK (status IS NULL OR status IN ('ACTIVE', 'INACTIVE')),
    CONSTRAINT account_scope_versions_something CHECK (account_name IS NOT NULL OR parent_account_id IS NOT NULL OR status IS NOT NULL),
    CONSTRAINT account_scope_versions_window CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT account_scope_versions_reason CHECK (btrim(change_reason) <> ''),
    CONSTRAINT account_scope_versions_parent CHECK (parent_account_id IS NULL OR parent_account_id <> account_id)
);
CREATE UNIQUE INDEX uq_account_scope_versions_number ON account_scope_versions (tenant_id, account_id, legal_entity_id, COALESCE(accounting_book_id, '00000000-0000-0000-0000-000000000000'::uuid), version);
CREATE UNIQUE INDEX uq_account_scope_versions_open ON account_scope_versions (tenant_id, account_id, legal_entity_id, COALESCE(accounting_book_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE effective_to IS NULL;
CREATE INDEX idx_account_scope_versions_lookup ON account_scope_versions (tenant_id, account_code, legal_entity_id, effective_from DESC);

ALTER TABLE account_scope_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_scope_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON account_scope_versions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE OR REPLACE FUNCTION account_scope_versions_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an account scope version is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
       OR (NEW.scope_version_id, NEW.tenant_id, NEW.account_id, NEW.account_code, NEW.legal_entity_id, NEW.accounting_book_id, NEW.version, NEW.account_name, NEW.parent_account_id, NEW.status,
           NEW.effective_from, NEW.change_reason, NEW.evidence_refs, NEW.created_at, NEW.created_by)
          IS DISTINCT FROM
          (OLD.scope_version_id, OLD.tenant_id, OLD.account_id, OLD.account_code, OLD.legal_entity_id, OLD.accounting_book_id, OLD.version, OLD.account_name, OLD.parent_account_id, OLD.status,
           OLD.effective_from, OLD.change_reason, OLD.evidence_refs, OLD.created_at, OLD.created_by) THEN
        RAISE EXCEPTION 'an account scope version can only be closed, once, by a newer version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER account_scope_versions_guard_trg BEFORE UPDATE OR DELETE ON account_scope_versions FOR EACH ROW EXECUTE FUNCTION account_scope_versions_guard();
