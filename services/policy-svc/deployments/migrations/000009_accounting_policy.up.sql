-- 000009_accounting_policy.up.sql
--
-- REF-07 Accounting Policy (ZS-SVC-I-001 s4.17): "effective-dated approved
-- accounting-policy versions and rule mappings by book/entity/jurisdiction".
--
-- An accounting policy is a policy_type ACCOUNTING_POLICY, so it rides the
-- GOV-05 machinery this service already has: policy_versions, the lifecycle
-- Draft -> Review -> Approved -> (Scheduled) -> Active -> Superseded/Suspended,
-- maker-checker, bitemporal resolution. What REF-07 adds is WHERE a policy
-- applies and WHAT it rests on, in three tables:
--
--   accounting_policy_applicability   PolicyApplicability: one policy family per
--                                     (tenant, policy domain, book, jurisdiction),
--                                     so its versions chain and supersede each
--                                     other and nothing else.
--   accounting_policy_version_details the semantic version, explicit precedence,
--                                     the framework source and approved policy
--                                     text it rests on, and the impact analysis
--                                     for future and open periods.
--   accounting_policy_rule_mappings   PolicyRuleMapping: the rules, jurisdiction
--                                     packs and other policy versions the policy
--                                     depends on, each pinned to a version.
--
-- All three are append-only, like the versions they describe, and tenant
-- isolated by the same row-level-security rule as policies.

CREATE TABLE accounting_policy_applicability (
    policy_id               UUID         PRIMARY KEY REFERENCES policies (policy_id),
    tenant_id               UUID         NOT NULL,

    -- The accounting subject, e.g. REVENUE_RECOGNITION, LEASES, FX_TRANSLATION.
    -- Data, not an enum: a new domain needs no migration.
    policy_domain           VARCHAR(64)  NOT NULL,
    -- The accounting framework, e.g. IFRS, US_GAAP.
    framework               VARCHAR(64)  NOT NULL,
    -- NULL = every book (REF-06) / every jurisdiction (REF-01) of the tenant.
    book_id                 UUID,
    jurisdiction_id         UUID,

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,

    CONSTRAINT ck_acct_domain    CHECK (policy_domain ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    CONSTRAINT ck_acct_framework CHECK (framework ~ '^[A-Z][A-Z0-9_.-]{1,63}$')
);

-- One family per applicability: two drafts for the same book, jurisdiction and
-- domain are versions of one policy, never two competing policies.
CREATE UNIQUE INDEX uq_acct_applicability
    ON accounting_policy_applicability (
        tenant_id, policy_domain,
        COALESCE(book_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(jurisdiction_id, '00000000-0000-0000-0000-000000000000'::UUID));

CREATE INDEX idx_acct_applicability_resolve
    ON accounting_policy_applicability (tenant_id, policy_domain);

CREATE TABLE accounting_policy_version_details (
    policy_version_id   UUID         PRIMARY KEY REFERENCES policy_versions (policy_version_id),
    policy_id           UUID         NOT NULL REFERENCES policies (policy_id),
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID,

    -- "Policy semantic version + scope/effective interval": a semantic version
    -- is used once per policy and entity scope.
    semantic_version    VARCHAR(32)  NOT NULL,
    -- Explicit precedence between policies that could both apply to a request
    -- and are equally specific. Without it such a pair is ambiguous, and an
    -- ambiguous policy blocks calculation instead of being guessed at.
    precedence          INTEGER      NOT NULL DEFAULT 0,

    -- Framework source and the approved policy text or rule reference.
    framework_source    TEXT         NOT NULL,
    policy_text_ref     TEXT         NOT NULL,
    -- Impact analysis for future and open periods; closed history is untouched.
    impact_analysis     JSONB        NOT NULL,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT ck_acct_semver     CHECK (semantic_version ~ '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'),
    CONSTRAINT ck_acct_precedence CHECK (precedence BETWEEN 0 AND 1000),
    CONSTRAINT ck_acct_sources    CHECK (btrim(framework_source) <> '' AND btrim(policy_text_ref) <> ''),
    CONSTRAINT ck_acct_impact     CHECK (jsonb_typeof(impact_analysis) = 'object'
                                         AND btrim(COALESCE(impact_analysis ->> 'summary', '')) <> ''
                                         AND impact_analysis ->> 'affected_periods' IN ('FUTURE_ONLY', 'OPEN_PERIODS')
                                         AND impact_analysis -> 'closed_history_unchanged' = 'true'::jsonb)
);

CREATE UNIQUE INDEX uq_acct_semver
    ON accounting_policy_version_details (
        policy_id, COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID), semantic_version);

CREATE TABLE accounting_policy_rule_mappings (
    mapping_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_version_id      UUID         NOT NULL REFERENCES policy_versions (policy_version_id),
    tenant_id              UUID         NOT NULL,
    mapping_no             INTEGER      NOT NULL,

    -- What is depended on: JURISDICTION_PACK, ACCOUNTING_RULE, POLICY_VERSION, ...
    ref_kind               VARCHAR(32)  NOT NULL,
    ref                    TEXT         NOT NULL,
    -- The version of it this policy version was approved against, so the
    -- dependency graph of an applied policy is reproducible.
    ref_version            VARCHAR(64)  NOT NULL,
    mapping_rationale      TEXT         NOT NULL,
    maps_to                TEXT,

    -- For a dependency on another policy version, a real foreign key: a
    -- dependency on a version that does not exist cannot be recorded, and a
    -- version only ever depends on versions that came before it, so the graph
    -- cannot have a cycle.
    depends_on_version_id  UUID         REFERENCES policy_versions (policy_version_id),

    CONSTRAINT uq_acct_mapping_no   UNIQUE (policy_version_id, mapping_no),
    CONSTRAINT ck_acct_mapping_kind CHECK (ref_kind ~ '^[A-Z][A-Z0-9_]{1,31}$'),
    CONSTRAINT ck_acct_mapping_text CHECK (btrim(ref) <> '' AND btrim(ref_version) <> '' AND btrim(mapping_rationale) <> ''),
    CONSTRAINT ck_acct_mapping_dep  CHECK ((ref_kind = 'POLICY_VERSION') = (depends_on_version_id IS NOT NULL))
);

CREATE INDEX idx_acct_mapping_dependency
    ON accounting_policy_rule_mappings (depends_on_version_id) WHERE depends_on_version_id IS NOT NULL;

-- Tenant isolation: the same rule as policies, for tenant-owned rows only.
ALTER TABLE accounting_policy_applicability ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_policy_applicability FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accounting_policy_applicability
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounting_policy_version_details ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_policy_version_details FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accounting_policy_version_details
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounting_policy_rule_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_policy_rule_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accounting_policy_rule_mappings
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Append-only. What a policy rested on is evidence; a change is a new version.
CREATE TRIGGER trg_acct_applicability_append_only
    BEFORE UPDATE OR DELETE ON accounting_policy_applicability
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();
CREATE TRIGGER trg_acct_applicability_no_truncate
    BEFORE TRUNCATE ON accounting_policy_applicability
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

CREATE TRIGGER trg_acct_details_append_only
    BEFORE UPDATE OR DELETE ON accounting_policy_version_details
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();
CREATE TRIGGER trg_acct_details_no_truncate
    BEFORE TRUNCATE ON accounting_policy_version_details
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

CREATE TRIGGER trg_acct_mappings_append_only
    BEFORE UPDATE OR DELETE ON accounting_policy_rule_mappings
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();
CREATE TRIGGER trg_acct_mappings_no_truncate
    BEFORE TRUNCATE ON accounting_policy_rule_mappings
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();
