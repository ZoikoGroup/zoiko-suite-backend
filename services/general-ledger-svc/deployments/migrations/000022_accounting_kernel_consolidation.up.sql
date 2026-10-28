-- Global Accounting Kernel, Wave 5: intercompany pairing, translation, elimination and the group book (standard §25, §26).
--
-- A consolidation group names a group book (a GROUP_CONSOLIDATION book of a group reporting entity) and the member entities whose
-- books it consumes, with effective-dated ownership. A run freezes the members' populations (signed snapshots at a ledger
-- watermark), translates them to the presentation currency under approved rate types, eliminates the matched intercompany
-- positions and posts the result into the group book only. Nothing here ever writes a member's statutory ledger.

-- ── group, members, mapping versions ─────────────────────────────────────────────────────────────────────────────────

CREATE TABLE consolidation_groups (
    group_id              UUID PRIMARY KEY,
    tenant_id             UUID          NOT NULL,
    group_code            VARCHAR(63)   NOT NULL,
    group_name            VARCHAR(200)  NOT NULL,
    parent_entity_id      UUID          NOT NULL,
    group_book_id         UUID          NOT NULL REFERENCES accounting_books(book_id),
    presentation_currency CHAR(3)       NOT NULL,
    ic_tolerance          NUMERIC(20,2) NOT NULL DEFAULT 0,
    status                VARCHAR(10)   NOT NULL DEFAULT 'ACTIVE',
    created_by            VARCHAR(255)  NOT NULL,
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT consolidation_groups_status CHECK (status IN ('ACTIVE', 'RETIRED')),
    CONSTRAINT consolidation_groups_tolerance CHECK (ic_tolerance >= 0),
    CONSTRAINT consolidation_groups_code UNIQUE (tenant_id, group_code)
);
ALTER TABLE consolidation_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_groups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_groups
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE TABLE consolidation_members (
    member_id        UUID PRIMARY KEY,
    tenant_id        UUID          NOT NULL,
    group_id         UUID          NOT NULL REFERENCES consolidation_groups(group_id),
    legal_entity_id  UUID          NOT NULL,
    source_book_id   UUID          NOT NULL REFERENCES accounting_books(book_id),
    method           VARCHAR(10)   NOT NULL DEFAULT 'FULL',
    ownership_pct    NUMERIC(7,4)  NOT NULL,
    effective_from   DATE          NOT NULL,
    effective_to     DATE,
    created_by       VARCHAR(255)  NOT NULL,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT consolidation_members_method CHECK (method IN ('FULL')),
    CONSTRAINT consolidation_members_pct CHECK (ownership_pct > 0 AND ownership_pct <= 100),
    CONSTRAINT consolidation_members_window CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE INDEX idx_consolidation_members_group ON consolidation_members (tenant_id, group_id, legal_entity_id, effective_from);
ALTER TABLE consolidation_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_members
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
-- a membership row is history: only its end date may be set, once
CREATE OR REPLACE FUNCTION consolidation_member_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation membership is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
       OR (NEW.member_id, NEW.tenant_id, NEW.group_id, NEW.legal_entity_id, NEW.source_book_id, NEW.method, NEW.ownership_pct, NEW.effective_from, NEW.created_by, NEW.created_at)
          IS DISTINCT FROM (OLD.member_id, OLD.tenant_id, OLD.group_id, OLD.legal_entity_id, OLD.source_book_id, OLD.method, OLD.ownership_pct, OLD.effective_from, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'a consolidation membership can only be ended, once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER consolidation_member_guard_trg BEFORE UPDATE OR DELETE ON consolidation_members FOR EACH ROW EXECUTE FUNCTION consolidation_member_guard();

-- the local-account -> group-account map and the intercompany account roles, as one immutable version
CREATE TABLE consolidation_mappings (
    tenant_id    UUID          NOT NULL,
    group_id     UUID          NOT NULL REFERENCES consolidation_groups(group_id),
    version      INTEGER       NOT NULL,
    status       VARCHAR(12)   NOT NULL,
    account_map  JSONB         NOT NULL,
    ic_roles     JSONB         NOT NULL,
    created_by   VARCHAR(255)  NOT NULL,
    created_at   TIMESTAMPTZ   NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id, version),
    CONSTRAINT consolidation_mappings_status CHECK (status IN ('ACTIVE', 'SUPERSEDED'))
);
CREATE UNIQUE INDEX uq_consolidation_mappings_active ON consolidation_mappings (tenant_id, group_id) WHERE status = 'ACTIVE';
ALTER TABLE consolidation_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_mappings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION consolidation_mapping_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation mapping version is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'ACTIVE' OR NEW.status <> 'SUPERSEDED'
       OR (NEW.tenant_id, NEW.group_id, NEW.version, NEW.account_map, NEW.ic_roles, NEW.created_by, NEW.created_at)
          IS DISTINCT FROM (OLD.tenant_id, OLD.group_id, OLD.version, OLD.account_map, OLD.ic_roles, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'a mapping version only changes by being superseded' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER consolidation_mapping_guard_trg BEFORE UPDATE OR DELETE ON consolidation_mappings FOR EACH ROW EXECUTE FUNCTION consolidation_mapping_guard();

-- ── consolidation runs ───────────────────────────────────────────────────────────────────────────────────────────────

CREATE TABLE consolidation_runs (
    run_id                   UUID PRIMARY KEY,
    tenant_id                UUID          NOT NULL,
    group_id                 UUID          NOT NULL REFERENCES consolidation_groups(group_id),
    fiscal_period            VARCHAR(20)   NOT NULL,
    as_of                    DATE          NOT NULL,
    status                   VARCHAR(12)   NOT NULL,
    mapping_version          INTEGER       NOT NULL,
    prior_run_id             UUID REFERENCES consolidation_runs(run_id),
    population               JSONB         NOT NULL,
    blocking_count           INTEGER       NOT NULL DEFAULT 0,
    translation_journal_id   UUID,
    elimination_journal_id   UUID,
    group_snapshot_id        UUID REFERENCES balance_snapshots(snapshot_id),
    run_hash                 CHAR(64)      NOT NULL,
    created_by               VARCHAR(255)  NOT NULL,
    created_at               TIMESTAMPTZ   NOT NULL,
    certified_by             VARCHAR(255),
    certified_at             TIMESTAMPTZ,
    superseded_by            VARCHAR(255),
    superseded_at            TIMESTAMPTZ,
    supersede_reason         TEXT,
    CONSTRAINT consolidation_runs_status CHECK (status IN ('PROCESSED', 'CERTIFIED', 'SUPERSEDED')),
    CONSTRAINT consolidation_runs_certified CHECK (status <> 'CERTIFIED' OR (certified_by IS NOT NULL AND group_snapshot_id IS NOT NULL AND blocking_count = 0 AND certified_by <> created_by))
);
CREATE UNIQUE INDEX uq_consolidation_runs_live ON consolidation_runs (tenant_id, group_id, fiscal_period) WHERE status <> 'SUPERSEDED';
CREATE INDEX idx_consolidation_runs_group ON consolidation_runs (tenant_id, group_id, as_of DESC);
ALTER TABLE consolidation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
-- what a run consumed and produced never changes; only its review state moves forward
CREATE OR REPLACE FUNCTION consolidation_run_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.run_id, NEW.tenant_id, NEW.group_id, NEW.fiscal_period, NEW.as_of, NEW.mapping_version, NEW.prior_run_id, NEW.population, NEW.blocking_count,
        NEW.translation_journal_id, NEW.elimination_journal_id, NEW.run_hash, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.run_id, OLD.tenant_id, OLD.group_id, OLD.fiscal_period, OLD.as_of, OLD.mapping_version, OLD.prior_run_id, OLD.population, OLD.blocking_count,
        OLD.translation_journal_id, OLD.elimination_journal_id, OLD.run_hash, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'what a consolidation run consumed and produced is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'CERTIFIED' AND NEW.status = 'PROCESSED' THEN
        RAISE EXCEPTION 'a certified run cannot go back to review' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER consolidation_run_guard_trg BEFORE UPDATE OR DELETE ON consolidation_runs FOR EACH ROW EXECUTE FUNCTION consolidation_run_guard();

-- ── what the kernel may originate ────────────────────────────────────────────────────────────────────────────────────

ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_purpose;
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_purpose CHECK (purpose IN ('CLOSE', 'RECLOSE', 'CONSOLIDATION_FREEZE', 'CONSOLIDATION_CLOSE', 'AD_HOC'));

ALTER TABLE journal_headers DROP CONSTRAINT journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT',
     'ALLOCATION', 'REVALUATION', 'RECONCILIATION_ADJUSTMENT', 'CONSOLIDATION', 'ELIMINATION', 'OTHER'));
