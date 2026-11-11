-- Migration: 000016_evidence_ledger.up.sql
--
-- G-07 evidence ledger, and the register fields the console reads.
--
-- The first version of this file could not be applied anywhere: it referenced
-- a tenants table this database does not have, UPDATEd governance_decisions
-- with the append-only trigger live, concatenated bytea with text, and chained
-- decisions in caller-supplied decided_at order, so a decision recorded late
-- with an early decided_at silently re-ordered history. It was never recorded
-- as applied in any environment, so it is replaced rather than amended.
--
-- What this migration builds, in order:
--
--   1. Register fields: title, summary, domain, priority, tags and
--      related_obligation_id. All optional on the wire; domain is classified
--      from action_type when a caller does not send one (data-driven: the
--      decision_domain_rules table, not code).
--   2. Outcome categories: the outcome column is free text written by several
--      services (GRANTED, APPROVED, DENIED, APPROVAL_REQUIRED ...). The
--      decision_outcome_categories table maps each spelling onto one of four
--      categories the register filters and counts by. Unmapped outcomes read as
--      OTHER; they are never guessed into a category.
--   3. The hash chain. Every decision gets chain_seq (1, 2, 3 ... per tenant,
--      in the order this service stored them), prev_hash (the previous
--      decision's curr_hash; 32 zero bytes for the first) and
--      curr_hash = sha256(prev_hash || canonical(decision)). The canonical form
--      is jsonb_build_object(...)::text over every stored column, whose text is
--      deterministic for a given value, so the verifier and the writer cannot
--      disagree about key order or number formatting. The link is computed by a
--      BEFORE INSERT trigger under a per-tenant advisory lock: two concurrent
--      appends cannot both chain to the same predecessor, and no caller can
--      supply its own hashes.
--   4. evidence_packages: sealed, append-only snapshots of the chain. Each
--      records the Merkle root (RFC 6962) over curr_hash of decisions
--      1..to_seq, the head hash at to_seq, and package_hash chaining it to the
--      previous package. Re-computing the root later proves none of those
--      decisions changed after the seal.

BEGIN;

-- ── 1. reference data ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS decision_domains (
    domain_code VARCHAR(32) PRIMARY KEY,
    label       VARCHAR(64) NOT NULL,
    sort_order  INT         NOT NULL
);

INSERT INTO decision_domains (domain_code, label, sort_order) VALUES
    ('GOVERNANCE_COMPLIANCE',  'Governance & Compliance',  10),
    ('LEGAL_CONTRACTS',        'Legal & Contracts',        20),
    ('FINANCE_TAX',            'Finance & Tax',            30),
    ('PEOPLE_PAYROLL',         'People & Payroll',         40),
    ('PROCUREMENT_COMMERCIAL', 'Procurement & Commercial', 50),
    ('DATA_PRIVACY',           'Data & Privacy',           60),
    ('ORGANIZATION_ACCESS',    'Organization & Access',    70),
    ('AI_GOVERNANCE',          'AI Governance',            80),
    ('OTHER',                  'Other',                    90)
ON CONFLICT (domain_code) DO NOTHING;

-- A keyword found anywhere in upper(action_type) classifies the decision. The
-- lowest precedence wins, then the longest keyword, so DATA_ACCESS_EXCEPTION is
-- Data & Privacy (20) rather than Organization & Access (70).
CREATE TABLE IF NOT EXISTS decision_domain_rules (
    keyword     VARCHAR(64) PRIMARY KEY,
    domain_code VARCHAR(32) NOT NULL REFERENCES decision_domains (domain_code),
    precedence  INT         NOT NULL
);

INSERT INTO decision_domain_rules (keyword, domain_code, precedence) VALUES
    ('AI_',          'AI_GOVERNANCE',          10),
    ('MODEL',        'AI_GOVERNANCE',          10),
    ('PRIVACY',      'DATA_PRIVACY',           20),
    ('CONSENT',      'DATA_PRIVACY',           20),
    ('DSAR',         'DATA_PRIVACY',           20),
    ('RETENTION',    'DATA_PRIVACY',           20),
    ('DATA_',        'DATA_PRIVACY',           20),
    ('PAYROLL',      'PEOPLE_PAYROLL',         30),
    ('EMPLOY',       'PEOPLE_PAYROLL',         30),
    ('LEAVE',        'PEOPLE_PAYROLL',         30),
    ('COMPENSATION', 'PEOPLE_PAYROLL',         30),
    ('BENEFIT',      'PEOPLE_PAYROLL',         30),
    ('OFFBOARD',     'PEOPLE_PAYROLL',         30),
    ('CONTRACT',     'LEGAL_CONTRACTS',        40),
    ('CLAUSE',       'LEGAL_CONTRACTS',        40),
    ('LEGAL',        'LEGAL_CONTRACTS',        40),
    ('SIGNATURE',    'LEGAL_CONTRACTS',        40),
    ('MEETING',      'LEGAL_CONTRACTS',        40),
    ('RESOLUTION',   'LEGAL_CONTRACTS',        40),
    ('PROCURE',      'PROCUREMENT_COMMERCIAL', 50),
    ('PURCHASE',     'PROCUREMENT_COMMERCIAL', 50),
    ('VENDOR',       'PROCUREMENT_COMMERCIAL', 50),
    ('SUPPLIER',     'PROCUREMENT_COMMERCIAL', 50),
    ('SPEND',        'PROCUREMENT_COMMERCIAL', 50),
    ('PAYMENT',      'FINANCE_TAX',            60),
    ('INVOICE',      'FINANCE_TAX',            60),
    ('JOURNAL',      'FINANCE_TAX',            60),
    ('LEDGER',       'FINANCE_TAX',            60),
    ('TAX',          'FINANCE_TAX',            60),
    ('BANK',         'FINANCE_TAX',            60),
    ('THRESHOLD',    'FINANCE_TAX',            60),
    ('BUDGET',       'FINANCE_TAX',            60),
    ('EXPENSE',      'FINANCE_TAX',            60),
    ('FINANCE',      'FINANCE_TAX',            60),
    ('ACCESS',       'ORGANIZATION_ACCESS',    70),
    ('ROLE',         'ORGANIZATION_ACCESS',    70),
    ('USER',         'ORGANIZATION_ACCESS',    70),
    ('IDENTITY',     'ORGANIZATION_ACCESS',    70),
    ('ENTITY',       'ORGANIZATION_ACCESS',    70),
    ('TENANT',       'ORGANIZATION_ACCESS',    70),
    ('ORGANIZATION', 'ORGANIZATION_ACCESS',    70),
    ('DELEGATION',   'ORGANIZATION_ACCESS',    70),
    ('PERMISSION',   'ORGANIZATION_ACCESS',    70),
    ('POLICY',       'GOVERNANCE_COMPLIANCE',  80),
    ('COMPLIANCE',   'GOVERNANCE_COMPLIANCE',  80),
    ('OBLIGATION',   'GOVERNANCE_COMPLIANCE',  80),
    ('GOVERNANCE',   'GOVERNANCE_COMPLIANCE',  80),
    ('CONTROL',      'GOVERNANCE_COMPLIANCE',  80),
    ('AUDIT',        'GOVERNANCE_COMPLIANCE',  80),
    ('EVIDENCE',     'GOVERNANCE_COMPLIANCE',  80),
    ('WORKFLOW',     'GOVERNANCE_COMPLIANCE',  80),
    ('RISK',         'GOVERNANCE_COMPLIANCE',  80)
ON CONFLICT (keyword) DO NOTHING;

CREATE OR REPLACE FUNCTION gdl_classify_domain(p_action_type TEXT) RETURNS VARCHAR
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT r.domain_code
           FROM decision_domain_rules r
          WHERE strpos(upper(p_action_type), r.keyword) > 0
          ORDER BY r.precedence, length(r.keyword) DESC, r.keyword
          LIMIT 1),
        'OTHER')
$$;

CREATE TABLE IF NOT EXISTS decision_outcome_categories (
    outcome  VARCHAR(32) PRIMARY KEY,
    category VARCHAR(16) NOT NULL
        CHECK (category IN ('APPROVED', 'PENDING', 'ESCALATED', 'DENIED'))
);

INSERT INTO decision_outcome_categories (outcome, category) VALUES
    ('GRANTED', 'APPROVED'), ('APPROVED', 'APPROVED'), ('ALLOW', 'APPROVED'),
    ('ALLOWED', 'APPROVED'), ('PERMIT', 'APPROVED'), ('PERMITTED', 'APPROVED'),
    ('AUTHORIZED', 'APPROVED'), ('ACCEPTED', 'APPROVED'), ('WITHIN_THRESHOLD', 'APPROVED'),
    ('PASSED', 'APPROVED'), ('COMPLIANT', 'APPROVED'),
    ('PENDING', 'PENDING'), ('IN_PROGRESS', 'PENDING'), ('REQUESTED', 'PENDING'),
    ('SUBMITTED', 'PENDING'), ('AWAITING_APPROVAL', 'PENDING'), ('UNDER_REVIEW', 'PENDING'),
    ('DEFERRED', 'PENDING'),
    ('ESCALATED', 'ESCALATED'), ('APPROVAL_REQUIRED', 'ESCALATED'), ('REFERRED', 'ESCALATED'),
    ('REVIEW_REQUIRED', 'ESCALATED'), ('STEP_UP_REQUIRED', 'ESCALATED'),
    ('DENIED', 'DENIED'), ('DENY', 'DENIED'), ('REJECTED', 'DENIED'), ('BLOCKED', 'DENIED'),
    ('REFUSED', 'DENIED'), ('REVOKED', 'DENIED'), ('NON_COMPLIANT', 'DENIED'), ('FAILED', 'DENIED')
ON CONFLICT (outcome) DO NOTHING;

-- ── 2. register fields ──────────────────────────────────────────────────────

ALTER TABLE governance_decisions
    ADD COLUMN IF NOT EXISTS title                 VARCHAR(200),
    ADD COLUMN IF NOT EXISTS summary               VARCHAR(2000),
    ADD COLUMN IF NOT EXISTS domain                VARCHAR(32) REFERENCES decision_domains (domain_code),
    ADD COLUMN IF NOT EXISTS priority              VARCHAR(16),
    ADD COLUMN IF NOT EXISTS tags                  TEXT[],
    ADD COLUMN IF NOT EXISTS related_obligation_id VARCHAR(64),
    ADD COLUMN IF NOT EXISTS chain_seq             BIGINT,
    ADD COLUMN IF NOT EXISTS prev_hash             BYTEA,
    ADD COLUMN IF NOT EXISTS curr_hash             BYTEA;

ALTER TABLE governance_decisions DROP CONSTRAINT IF EXISTS chk_priority;
ALTER TABLE governance_decisions
    ADD CONSTRAINT chk_priority CHECK (priority IS NULL OR priority IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL'));
ALTER TABLE governance_decisions DROP CONSTRAINT IF EXISTS chk_tags;
ALTER TABLE governance_decisions
    ADD CONSTRAINT chk_tags CHECK (tags IS NULL OR cardinality(tags) <= 10);

-- ── 3. hash chain ───────────────────────────────────────────────────────────

-- A timestamp as the canonical form writes it: UTC, microseconds, independent
-- of the session's TimeZone and DateStyle.
CREATE OR REPLACE FUNCTION gdl_ts(t TIMESTAMPTZ) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT to_char(t AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
$$;

-- The canonical form of one decision: every stored column except the two
-- hashes. Version 1. Changing it breaks every existing link, so a change is a
-- new version written alongside, never an edit of this one.
CREATE OR REPLACE FUNCTION gdl_canonical(d governance_decisions) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT jsonb_build_object(
        'v',                     1,
        'tenant_id',             d.tenant_id,
        'chain_seq',             d.chain_seq,
        'decision_id',           d.decision_id,
        'legal_entity_id',       d.legal_entity_id,
        'actor_id',              d.actor_id,
        'action_type',           d.action_type,
        'outcome',               d.outcome,
        'rule_basis',            d.rule_basis,
        'jurisdiction_context',  d.jurisdiction_context,
        'policy_version_id',     d.policy_version_id,
        'action_subject_type',   d.action_subject_type,
        'action_subject_id',     d.action_subject_id,
        'evaluation_context',    d.evaluation_context,
        'correlation_id',        d.correlation_id,
        'causation_id',          d.causation_id,
        'workflow_instance_id',  d.workflow_instance_id,
        'decided_at',            gdl_ts(d.decided_at),
        'stored_at',             gdl_ts(d.stored_at),
        'title',                 d.title,
        'summary',               d.summary,
        'domain',                d.domain,
        'priority',              d.priority,
        'tags',                  to_jsonb(d.tags),
        'related_obligation_id', d.related_obligation_id
    )::text
$$;

-- Backfill. Every existing decision gets its domain and its place in the
-- chain, in the order it was stored. The append-only trigger refuses every
-- UPDATE, so it is disabled for this backfill only, inside this transaction,
-- the same way 000007 backfilled replay_manifests. FORCE row-level security is
-- lifted for the same statements: with no tenant set, the owner would see no row.
ALTER TABLE governance_decisions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE governance_decisions DISABLE TRIGGER governance_decisions_immutable;

UPDATE governance_decisions
   SET domain = gdl_classify_domain(action_type)
 WHERE domain IS NULL;

DO $$
DECLARE
    r        governance_decisions;
    v_tenant TEXT := NULL;
    v_seq    BIGINT := 0;
    v_hash   BYTEA;
BEGIN
    FOR r IN SELECT * FROM governance_decisions ORDER BY tenant_id, stored_at, decision_id LOOP
        IF v_tenant IS DISTINCT FROM r.tenant_id THEN
            v_tenant := r.tenant_id;
            v_seq    := 0;
            v_hash   := decode(repeat('00', 32), 'hex');
        END IF;
        v_seq       := v_seq + 1;
        r.chain_seq := v_seq;
        r.prev_hash := v_hash;
        r.curr_hash := sha256(v_hash || convert_to(gdl_canonical(r), 'UTF8'));
        UPDATE governance_decisions
           SET chain_seq = r.chain_seq, prev_hash = r.prev_hash, curr_hash = r.curr_hash
         WHERE tenant_id = r.tenant_id AND decision_id = r.decision_id;
        v_hash := r.curr_hash;
    END LOOP;
END $$;

ALTER TABLE governance_decisions ENABLE TRIGGER governance_decisions_immutable;
ALTER TABLE governance_decisions FORCE ROW LEVEL SECURITY;

ALTER TABLE governance_decisions
    ALTER COLUMN domain    SET NOT NULL,
    ALTER COLUMN chain_seq SET NOT NULL,
    ALTER COLUMN prev_hash SET NOT NULL,
    ALTER COLUMN curr_hash SET NOT NULL;

ALTER TABLE governance_decisions DROP CONSTRAINT IF EXISTS chk_chain_hashes;
ALTER TABLE governance_decisions
    ADD CONSTRAINT chk_chain_hashes CHECK (length(prev_hash) = 32 AND length(curr_hash) = 32 AND chain_seq >= 1);

CREATE UNIQUE INDEX IF NOT EXISTS uq_governance_decisions_chain
    ON governance_decisions (tenant_id, chain_seq);

-- Classify first (gdl_10), then link (gdl_20): BEFORE triggers fire in name
-- order, and the link hashes the domain.
CREATE OR REPLACE FUNCTION gdl_classify_decision() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.domain IS NULL OR NEW.domain = '' THEN
        NEW.domain := gdl_classify_domain(NEW.action_type);
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION gdl_chain_link() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    v_seq  BIGINT;
    v_hash BYTEA;
BEGIN
    -- One appender per tenant at a time, until commit. The SELECT below runs
    -- after the lock is held, with a fresh snapshot (volatile function), so it
    -- sees the previous appender's committed head.
    PERFORM pg_advisory_xact_lock(hashtextextended('gdl-chain:' || NEW.tenant_id, 0));

    SELECT chain_seq, curr_hash INTO v_seq, v_hash
      FROM governance_decisions
     WHERE tenant_id = NEW.tenant_id
     ORDER BY chain_seq DESC
     LIMIT 1;
    IF NOT FOUND THEN
        v_seq  := 0;
        v_hash := decode(repeat('00', 32), 'hex');
    END IF;

    NEW.chain_seq := v_seq + 1;
    NEW.prev_hash := v_hash;
    NEW.curr_hash := sha256(v_hash || convert_to(gdl_canonical(NEW), 'UTF8'));
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS gdl_10_classify ON governance_decisions;
CREATE TRIGGER gdl_10_classify
    BEFORE INSERT ON governance_decisions
    FOR EACH ROW EXECUTE FUNCTION gdl_classify_decision();

DROP TRIGGER IF EXISTS gdl_20_chain ON governance_decisions;
CREATE TRIGGER gdl_20_chain
    BEFORE INSERT ON governance_decisions
    FOR EACH ROW EXECUTE FUNCTION gdl_chain_link();

-- Register queries: newest first per tenant, and the facets the console
-- filters by.
CREATE INDEX IF NOT EXISTS idx_governance_decisions_tenant_decided
    ON governance_decisions (tenant_id, decided_at DESC, decision_id DESC);
CREATE INDEX IF NOT EXISTS idx_governance_decisions_tenant_domain
    ON governance_decisions (tenant_id, domain);
CREATE INDEX IF NOT EXISTS idx_governance_decisions_tenant_causation
    ON governance_decisions (tenant_id, causation_id)
    WHERE causation_id IS NOT NULL;

-- ── 4. sealed evidence packages ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS evidence_packages (
    tenant_id         VARCHAR(64)  NOT NULL,
    package_id        UUID         NOT NULL,
    package_seq       INT          NOT NULL CHECK (package_seq >= 1),
    to_seq            BIGINT       NOT NULL CHECK (to_seq >= 1),
    leaf_count        BIGINT       NOT NULL CHECK (leaf_count >= 1),
    merkle_root       BYTEA        NOT NULL CHECK (length(merkle_root) = 32),
    head_hash         BYTEA        NOT NULL CHECK (length(head_hash) = 32),
    head_decision_id  VARCHAR(64)  NOT NULL,
    prev_package_hash BYTEA        NOT NULL CHECK (length(prev_package_hash) = 32),
    package_hash      BYTEA        NOT NULL CHECK (length(package_hash) = 32),
    label             VARCHAR(200),
    reason            VARCHAR(500),
    sealed_by         VARCHAR(64)  NOT NULL,
    sealed_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    idempotency_key   VARCHAR(256) NOT NULL,
    request_hash      BYTEA        NOT NULL,
    PRIMARY KEY (tenant_id, package_id),
    UNIQUE (tenant_id, package_seq),
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_evidence_packages_tenant_to_seq
    ON evidence_packages (tenant_id, to_seq);

CREATE OR REPLACE FUNCTION reject_ledger_mutation() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$;

DROP TRIGGER IF EXISTS evidence_packages_immutable ON evidence_packages;
CREATE TRIGGER evidence_packages_immutable
    BEFORE UPDATE OR DELETE ON evidence_packages
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();

-- Its own function rather than 000011's reject_truncate(): databases that
-- applied an earlier 000011 have *_block_truncate triggers and no such function.
CREATE OR REPLACE FUNCTION reject_ledger_truncate() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: TRUNCATE is not permitted', TG_TABLE_NAME;
END;
$$;

DROP TRIGGER IF EXISTS evidence_packages_no_truncate ON evidence_packages;
CREATE TRIGGER evidence_packages_no_truncate
    BEFORE TRUNCATE ON evidence_packages
    FOR EACH STATEMENT EXECUTE FUNCTION reject_ledger_truncate();

ALTER TABLE evidence_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_packages FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON evidence_packages;
CREATE POLICY tenant_isolation_policy ON evidence_packages
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

COMMIT;
