-- Global Accounting Kernel, Wave 6: advanced policy - jurisdiction extensions, allocations (standard §6.1, §21, §35).

-- ── jurisdiction extensions ──────────────────────────────────────────────────────────────────────────────────────────
-- A book may name the jurisdiction it is kept under; a rule set may be an extension for one jurisdiction. A jurisdiction rule set
-- overrides the framework's global rule set for that event in books of that jurisdiction (hierarchy: kernel invariant, framework /
-- jurisdiction policy, book policy, tenant configuration). The empty string is "global".

ALTER TABLE accounting_books ADD COLUMN jurisdiction VARCHAR(8) NOT NULL DEFAULT '';
ALTER TABLE accounting_books ADD CONSTRAINT accounting_books_jurisdiction CHECK (jurisdiction = '' OR jurisdiction ~ '^[A-Z]{2}(-[A-Z0-9]{1,5})?$');
ALTER TABLE posting_rule_sets ADD COLUMN jurisdiction VARCHAR(8) NOT NULL DEFAULT '';
ALTER TABLE posting_rule_sets ADD CONSTRAINT posting_rule_sets_jurisdiction CHECK (jurisdiction = '' OR jurisdiction ~ '^[A-Z]{2}(-[A-Z0-9]{1,5})?$');

CREATE OR REPLACE FUNCTION kernel_jurisdiction_fixed() RETURNS trigger AS $$
BEGIN
    IF NEW.jurisdiction IS DISTINCT FROM OLD.jurisdiction THEN
        RAISE EXCEPTION 'the jurisdiction of a book or rule set is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER accounting_books_jurisdiction_trg BEFORE UPDATE ON accounting_books FOR EACH ROW EXECUTE FUNCTION kernel_jurisdiction_fixed();
CREATE TRIGGER posting_rule_sets_jurisdiction_trg BEFORE UPDATE ON posting_rule_sets FOR EACH ROW EXECUTE FUNCTION kernel_jurisdiction_fixed();

-- the activation overlap check now also scopes by jurisdiction: a jurisdiction extension and the global rule set can both be active
CREATE OR REPLACE FUNCTION posting_rule_sets_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a posting rule set is never deleted: retire it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'a posting rule set is created as a draft' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.posting_rule_set_id <> OLD.posting_rule_set_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.name <> OLD.name OR NEW.domain <> OLD.domain
       OR NEW.event_type <> OLD.event_type OR NEW.book_type <> OLD.book_type OR NEW.accounting_framework_id <> OLD.accounting_framework_id
       OR NEW.version <> OLD.version OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity of a posting rule set version is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'rule set version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.content::text <> OLD.content::text OR NEW.content_hash <> OLD.content_hash OR NEW.test_pack::text <> OLD.test_pack::text
       OR NEW.test_pack_id <> OLD.test_pack_id OR NEW.effective_from <> OLD.effective_from) THEN
        RAISE EXCEPTION 'the content, test pack and start date of a rule set are fixed once it leaves draft' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired rule set is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS DISTINCT FROM NEW.effective_to AND OLD.status NOT IN ('DRAFT', 'ACTIVE') THEN
        RAISE EXCEPTION 'the end date of a rule set is fixed once it is superseded or retired' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status THEN
        IF NOT (
               (OLD.status = 'DRAFT' AND NEW.status = 'TEST')
            OR (OLD.status = 'TEST' AND NEW.status IN ('DRAFT', 'APPROVED'))
            OR (OLD.status = 'APPROVED' AND NEW.status IN ('ACTIVE', 'DRAFT'))
            OR (OLD.status = 'ACTIVE' AND NEW.status IN ('SUPERSEDED', 'RETIRED'))
            OR (OLD.status = 'SUPERSEDED' AND NEW.status = 'RETIRED')) THEN
            RAISE EXCEPTION 'rule set status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status IN ('APPROVED', 'ACTIVE') AND (NEW.tests_passed_hash IS NULL OR NEW.tests_passed_hash <> NEW.content_hash) THEN
            RAISE EXCEPTION 'the test pack has not passed for exactly this content' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status = 'APPROVED' AND (NEW.approved_by IS NULL OR NEW.approved_by = NEW.created_by) THEN
            RAISE EXCEPTION 'a rule set is approved by someone other than its author' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status = 'ACTIVE' THEN
            IF NEW.activated_by IS NULL OR NEW.activated_by = NEW.created_by OR NEW.approved_by IS NULL THEN
                RAISE EXCEPTION 'a rule set is activated by someone other than its author, after approval' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            PERFORM pg_advisory_xact_lock(hashtext(NEW.tenant_id::text || '|' || NEW.event_type || '|' || NEW.book_type || '|' || NEW.accounting_framework_id || '|' || NEW.jurisdiction));
            IF EXISTS (SELECT 1 FROM posting_rule_sets o
                       WHERE o.tenant_id = NEW.tenant_id AND o.event_type = NEW.event_type AND o.book_type = NEW.book_type
                         AND o.accounting_framework_id = NEW.accounting_framework_id AND o.jurisdiction = NEW.jurisdiction
                         AND o.status = 'ACTIVE' AND o.posting_rule_set_id <> NEW.posting_rule_set_id
                         AND o.effective_from < COALESCE(NEW.effective_to, DATE '9999-01-01')
                         AND NEW.effective_from < COALESCE(o.effective_to, DATE '9999-01-01')) THEN
                RAISE EXCEPTION 'an active rule set already covers this event type, book type and framework on a common date' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── allocations (§21): a source pool spread over recipients by a driver, balanced, with the driver snapshot retained ──────

CREATE TABLE allocation_rules (
    allocation_rule_id  UUID PRIMARY KEY,
    tenant_id           UUID          NOT NULL,
    rule_code           VARCHAR(63)   NOT NULL,
    version             INTEGER       NOT NULL,
    status              VARCHAR(10)   NOT NULL,
    content             JSONB         NOT NULL,
    content_hash        CHAR(64)      NOT NULL,
    effective_from      DATE          NOT NULL,
    created_by          VARCHAR(255)  NOT NULL,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    approved_by         VARCHAR(255),
    approved_at         TIMESTAMPTZ,
    retired_by          VARCHAR(255),
    retired_at          TIMESTAMPTZ,
    CONSTRAINT allocation_rules_version UNIQUE (tenant_id, rule_code, version),
    CONSTRAINT allocation_rules_status CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    CONSTRAINT allocation_rules_active CHECK (status = 'DRAFT' OR (approved_by IS NOT NULL AND approved_by <> created_by))
);
CREATE UNIQUE INDEX uq_allocation_rules_active ON allocation_rules (tenant_id, rule_code) WHERE status = 'ACTIVE';
ALTER TABLE allocation_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_rules
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION allocation_rules_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an allocation rule is never deleted: retire it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.allocation_rule_id, NEW.tenant_id, NEW.rule_code, NEW.version, NEW.content::text, NEW.content_hash, NEW.effective_from, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.allocation_rule_id, OLD.tenant_id, OLD.rule_code, OLD.version, OLD.content::text, OLD.content_hash, OLD.effective_from, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'the content of an allocation rule version is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT ((OLD.status = 'DRAFT' AND NEW.status IN ('ACTIVE', 'RETIRED')) OR (OLD.status = 'ACTIVE' AND NEW.status = 'RETIRED') OR OLD.status = NEW.status) THEN
        RAISE EXCEPTION 'allocation rule status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER allocation_rules_guard_trg BEFORE UPDATE OR DELETE ON allocation_rules FOR EACH ROW EXECUTE FUNCTION allocation_rules_guard();

CREATE TABLE allocation_runs (
    allocation_run_id    UUID PRIMARY KEY,
    tenant_id            UUID          NOT NULL,
    allocation_rule_id   UUID          NOT NULL REFERENCES allocation_rules(allocation_rule_id),
    legal_entity_id      UUID          NOT NULL,
    accounting_book_id   UUID          NOT NULL REFERENCES accounting_books(book_id),
    fiscal_period        VARCHAR(20)   NOT NULL,
    status               VARCHAR(10)   NOT NULL,
    ledger_watermark     BIGINT        NOT NULL,
    pool_amount          NUMERIC(24,2) NOT NULL,
    population           JSONB         NOT NULL,
    journal_id           UUID,
    reversal_journal_id  UUID,
    created_by           VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL,
    reversed_by          VARCHAR(255),
    reversed_at          TIMESTAMPTZ,
    reversal_reason      TEXT,
    CONSTRAINT allocation_runs_status CHECK (status IN ('POSTED', 'REVERSED')),
    CONSTRAINT allocation_runs_reversed CHECK (status = 'POSTED' OR (reversed_by IS NOT NULL AND COALESCE(btrim(reversal_reason), '') <> ''))
);
CREATE UNIQUE INDEX uq_allocation_runs_live ON allocation_runs (tenant_id, allocation_rule_id, accounting_book_id, fiscal_period) WHERE status = 'POSTED';
CREATE INDEX idx_allocation_runs_entity ON allocation_runs (tenant_id, legal_entity_id, fiscal_period);
ALTER TABLE allocation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION allocation_runs_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an allocation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.allocation_run_id, NEW.tenant_id, NEW.allocation_rule_id, NEW.legal_entity_id, NEW.accounting_book_id, NEW.fiscal_period, NEW.ledger_watermark, NEW.pool_amount,
        NEW.population::text, NEW.journal_id, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.allocation_run_id, OLD.tenant_id, OLD.allocation_rule_id, OLD.legal_entity_id, OLD.accounting_book_id, OLD.fiscal_period, OLD.ledger_watermark, OLD.pool_amount,
        OLD.population::text, OLD.journal_id, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'what an allocation run took and posted is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'REVERSED' THEN
        RAISE EXCEPTION 'a reversed allocation run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER allocation_runs_guard_trg BEFORE UPDATE OR DELETE ON allocation_runs FOR EACH ROW EXECUTE FUNCTION allocation_runs_guard();
