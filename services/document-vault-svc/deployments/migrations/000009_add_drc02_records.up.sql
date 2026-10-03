-- DRC-02 Record Declaration & Classification (ZS-SVC-S-001 §4), Wave 1:
-- schema + domain model.
--
-- This is a NEW, separate "records" aggregate, distinct from the
-- existing documents.declared_* columns (migration 000005) and from
-- record_classifications (migration 000008, which is BIZ-02's SECURITY
-- classification — PUBLIC/INTERNAL/CONFIDENTIAL/RESTRICTED — a
-- completely different concept from this table's record-type
-- taxonomy). documents/document_versions are untouched.
--
-- Per ZS-SVC-S-001's own explicit distinction (§0.1/§3.1): "Document
-- state and record state are separate; declaration captures an exact
-- immutable version." A document is a mutable metadata wrapper; a
-- record is created only by declaring one exact, already-committed
-- document_version as governed evidence. Once DECLARED, a record
-- "cannot be edited" (DRC-I04) — correction happens only through an
-- explicit relationship (SUPERSEDES/CORRECTS/AMENDS/...) to a new
-- record, never an in-place change.
--
-- retention_schedule_ref is stored as a caller-supplied reference only
-- — no retention-rule engine exists in this codebase yet (that is
-- DRC-03, explicitly deferred: "implement DRC-03 in dry-run before
-- destructive disposition"). This table does not enforce or compute
-- any retention deadline; it only records which schedule a record
-- claims to be bound to, so DRC-03 has something real to join against
-- when it's built.
CREATE TABLE records (
    record_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    document_id              UUID NOT NULL REFERENCES documents(document_id),
    document_version_id      UUID NOT NULL REFERENCES document_versions(document_version_id),
    record_class             VARCHAR(32) NOT NULL
        CHECK (record_class IN (
            'ACCOUNTING_WORKPAPER', 'TAX_RETURN_SUPPORT', 'PAYROLL_OUTPUT', 'EMPLOYMENT_RECORD',
            'LEGAL_CONTRACT', 'LEGAL_MATTER_RECORD', 'COMPLIANCE_EVIDENCE', 'CORPORATE_RECORD'
        )),
    jurisdiction_scope       VARCHAR(64) NOT NULL CHECK (jurisdiction_scope <> ''),
    business_context         TEXT NOT NULL DEFAULT '',
    retention_schedule_ref   TEXT,
    -- record_state per §3.1/§4: "no mutable draft record" — a record is
    -- born DECLARED. DISPOSED is schema-ready but unreachable by any
    -- command in this wave: disposition execution is DRC-03's job,
    -- deliberately not built here (wrong retention logic can destroy
    -- evidence irreversibly, per the spec's own caution).
    record_state             VARCHAR(16) NOT NULL DEFAULT 'DECLARED'
        CHECK (record_state IN ('DECLARED', 'SUPERSEDED', 'DISPOSED')),
    declared_by_principal_id VARCHAR(255) NOT NULL CHECK (declared_by_principal_id <> ''),
    declaration_reason       TEXT NOT NULL DEFAULT '',
    declared_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id           VARCHAR(255),
    -- Each exact document version can be declared as a record at most
    -- once — "converts exact document versions into governed records"
    -- (§1.3) is a one-to-one mapping, not a repeatable action.
    CONSTRAINT records_document_version_unique UNIQUE (document_version_id),
    CONSTRAINT records_tenant_correlation_unique UNIQUE (tenant_id, correlation_id)
);

CREATE INDEX idx_records_tenant_state ON records (tenant_id, record_state);
CREATE INDEX idx_records_document ON records (document_id);

ALTER TABLE records ENABLE ROW LEVEL SECURITY;
ALTER TABLE records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON records
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- A declared record's facts (class/jurisdiction/context/version/
-- declarer) are permanent once written (DRC-I04: "a declared record
-- cannot be 'edited'"). Only record_state may move, and only forward,
-- DECLARED -> SUPERSEDED, exactly once.
CREATE OR REPLACE FUNCTION reject_record_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'records rows are never deleted';
    END IF;
    IF NEW.document_id IS DISTINCT FROM OLD.document_id
        OR NEW.document_version_id IS DISTINCT FROM OLD.document_version_id
        OR NEW.record_class IS DISTINCT FROM OLD.record_class
        OR NEW.jurisdiction_scope IS DISTINCT FROM OLD.jurisdiction_scope
        OR NEW.business_context IS DISTINCT FROM OLD.business_context
        OR NEW.retention_schedule_ref IS DISTINCT FROM OLD.retention_schedule_ref
        OR NEW.declared_by_principal_id IS DISTINCT FROM OLD.declared_by_principal_id
        OR NEW.declaration_reason IS DISTINCT FROM OLD.declaration_reason
        OR NEW.declared_at IS DISTINCT FROM OLD.declared_at
    THEN
        RAISE EXCEPTION 'record % facts are immutable once declared; correction requires a new record and a relationship', OLD.record_id;
    END IF;
    IF OLD.record_state <> 'DECLARED' AND NEW.record_state IS DISTINCT FROM OLD.record_state THEN
        RAISE EXCEPTION 'record % is % and its state cannot change again', OLD.record_id, OLD.record_state;
    END IF;
    IF OLD.record_state = 'DECLARED' AND NEW.record_state NOT IN ('DECLARED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'invalid record state transition from DECLARED to %', NEW.record_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_record_mutation
    BEFORE UPDATE OR DELETE ON records
    FOR EACH ROW EXECUTE FUNCTION reject_record_mutation();

-- record_relationships: the evidentiary relationship vocabulary (§4.5).
-- Append-only — a relationship, once recorded, is a permanent claim
-- about how two records relate; it is never edited or withdrawn
-- (correcting a mistaken relationship means recording a new one, same
-- doctrine as everything else in this table set).
--
-- source_record_id is the NEW/acting record; target_record_id is the
-- one being acted upon. For relationship_type = SUPERSEDES, the
-- target's record_state is moved to SUPERSEDED in the same
-- transaction (see records_store.go) — the only command in this wave
-- that changes record_state at all.
CREATE TABLE record_relationships (
    relationship_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    source_record_id      UUID NOT NULL REFERENCES records(record_id),
    target_record_id      UUID NOT NULL REFERENCES records(record_id),
    relationship_type     VARCHAR(16) NOT NULL
        CHECK (relationship_type IN ('SUPERSEDES', 'CORRECTS', 'AMENDS', 'ATTACHES_TO', 'EVIDENCES', 'DERIVED_FROM', 'DUPLICATE_OF')),
    created_by_principal_id VARCHAR(255) NOT NULL CHECK (created_by_principal_id <> ''),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_record_id <> target_record_id),
    CONSTRAINT record_relationships_unique UNIQUE (source_record_id, target_record_id, relationship_type)
);

CREATE INDEX idx_record_relationships_source ON record_relationships (source_record_id);
CREATE INDEX idx_record_relationships_target ON record_relationships (target_record_id);

ALTER TABLE record_relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_relationships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON record_relationships
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_record_relationship_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'record_relationships rows are append-only and never modified or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_record_relationship_mutation
    BEFORE UPDATE OR DELETE ON record_relationships
    FOR EACH ROW EXECUTE FUNCTION reject_record_relationship_mutation();
