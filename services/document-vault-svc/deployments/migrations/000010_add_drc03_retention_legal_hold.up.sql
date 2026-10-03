-- DRC-03 Retention, Disposition & Legal Hold (ZS-SVC-S-001 §5), Wave 1:
-- dry-run retention evaluation + legal hold. Additive alongside
-- records/record_relationships (migration 000009), which are
-- untouched.
--
-- Deliberately dry-run only, per the spec's own build order ("Wave 3 —
-- Retention Engine: ... Exit: Automatic deletion remains disabled."
-- and "Wave 4 — Legal Hold: ... Exit: Disposition still dry-run
-- only."). record_retention_states.state stops at
-- APPROVED_FOR_DISPOSITION — DISPOSITION_IN_PROGRESS/DISPOSED/
-- DISPOSITION_FAILED are deliberately NOT modeled here; no command in
-- this wave ever deletes, destroys or anonymizes anything. Likewise
-- legal_holds.status omits PARTIALLY_APPLIED/RELEASE_PENDING/ERROR —
-- this wave has no asynchronous apply pipeline that could produce
-- those states, so they are not fabricated.

-- Retention Rule Versions: the governed, versioned registry a record's
-- retention is bound to. Mutable only while DRAFT; APPROVED freezes
-- the rule's content (DRC-I09: "no code path may convert an unresolved
-- retention-rule conflict into silent indefinite retention or silent
-- deletion" — a rule's terms must be fixed and auditable before
-- anything relies on them).
CREATE TABLE retention_rule_versions (
    retention_rule_version_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    record_class              VARCHAR(32) NOT NULL
        CHECK (record_class IN (
            'ACCOUNTING_WORKPAPER', 'TAX_RETURN_SUPPORT', 'PAYROLL_OUTPUT', 'EMPLOYMENT_RECORD',
            'LEGAL_CONTRACT', 'LEGAL_MATTER_RECORD', 'COMPLIANCE_EVIDENCE', 'CORPORATE_RECORD'
        )),
    jurisdiction_selector     VARCHAR(64) NOT NULL CHECK (jurisdiction_selector <> ''),
    legal_basis_ref           TEXT NOT NULL DEFAULT '',
    purpose_ref               TEXT NOT NULL DEFAULT '',
    trigger_type              VARCHAR(16) NOT NULL
        CHECK (trigger_type IN ('CREATED_AT', 'DECLARED_AT', 'PERIOD_END', 'TAX_YEAR_END', 'EMPLOYMENT_END', 'CONTRACT_END', 'MATTER_CLOSED', 'CUSTOM_EVENT')),
    -- duration is modeled as a plain day count, not an ISO-8601
    -- interval or calendar-aware year count — a deliberate
    -- simplification; this is reference data to compute against, not a
    -- jurisdiction-accurate legal calendar engine.
    duration_days             INT NOT NULL CHECK (duration_days > 0),
    disposition_action        VARCHAR(16) NOT NULL
        CHECK (disposition_action IN ('DELETE', 'DESTROY', 'ANONYMIZE', 'REVIEW', 'ARCHIVE')),
    status                    VARCHAR(16) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'APPROVED', 'ACTIVE', 'SUPERSEDED', 'WITHDRAWN')),
    created_by_principal_id   VARCHAR(255) NOT NULL CHECK (created_by_principal_id <> ''),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by_principal_id  VARCHAR(255),
    approved_at               TIMESTAMPTZ,
    superseded_by_version_id  UUID REFERENCES retention_rule_versions(retention_rule_version_id),
    CHECK (
        (approved_by_principal_id IS NULL AND approved_at IS NULL)
        OR (approved_by_principal_id IS NOT NULL AND approved_at IS NOT NULL)
    ),
    -- Maker-checker: the same principal cannot both author and approve
    -- a retention rule — same doctrine as record_classifications'
    -- CONFIRMED gate (migration 000008).
    CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id)
);

CREATE INDEX idx_retention_rule_versions_lookup ON retention_rule_versions (tenant_id, record_class, jurisdiction_selector, status);

ALTER TABLE retention_rule_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_rule_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON retention_rule_versions
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- A DRAFT rule may still be edited freely (not yet relied upon by any
-- record); once APPROVED, its terms are permanent — only status may
-- move again, forward-only.
CREATE OR REPLACE FUNCTION reject_retention_rule_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'retention_rule_versions rows are never deleted';
    END IF;
    IF OLD.status <> 'DRAFT' THEN
        IF NEW.record_class IS DISTINCT FROM OLD.record_class
            OR NEW.jurisdiction_selector IS DISTINCT FROM OLD.jurisdiction_selector
            OR NEW.legal_basis_ref IS DISTINCT FROM OLD.legal_basis_ref
            OR NEW.purpose_ref IS DISTINCT FROM OLD.purpose_ref
            OR NEW.trigger_type IS DISTINCT FROM OLD.trigger_type
            OR NEW.duration_days IS DISTINCT FROM OLD.duration_days
            OR NEW.disposition_action IS DISTINCT FROM OLD.disposition_action
        THEN
            RAISE EXCEPTION 'retention_rule_version % is no longer DRAFT; its terms are immutable', OLD.retention_rule_version_id;
        END IF;
    END IF;
    IF OLD.status IN ('SUPERSEDED', 'WITHDRAWN') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'retention_rule_version % is % and immutable', OLD.retention_rule_version_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'DRAFT' THEN
                IF NEW.status NOT IN ('APPROVED', 'WITHDRAWN') THEN
                    RAISE EXCEPTION 'invalid retention_rule_version transition from DRAFT to %', NEW.status;
                END IF;
            WHEN 'APPROVED' THEN
                IF NEW.status NOT IN ('ACTIVE', 'WITHDRAWN') THEN
                    RAISE EXCEPTION 'invalid retention_rule_version transition from APPROVED to %', NEW.status;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.status <> 'SUPERSEDED' THEN
                    RAISE EXCEPTION 'invalid retention_rule_version transition from ACTIVE to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown retention_rule_version status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_retention_rule_mutation
    BEFORE UPDATE OR DELETE ON retention_rule_versions
    FOR EACH ROW EXECUTE FUNCTION reject_retention_rule_mutation();

-- Record Retention States: one row per record, tracking the dry-run
-- retention lifecycle (§5.4) up to APPROVED_FOR_DISPOSITION only.
CREATE TABLE record_retention_states (
    retention_state_id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                            UUID NOT NULL,
    record_id                            UUID NOT NULL UNIQUE REFERENCES records(record_id),
    retention_rule_version_id            UUID NOT NULL REFERENCES retention_rule_versions(retention_rule_version_id),
    trigger_date                         TIMESTAMPTZ,
    due_at                               TIMESTAMPTZ,
    state                                VARCHAR(24) NOT NULL DEFAULT 'WAITING_FOR_TRIGGER'
        CHECK (state IN ('WAITING_FOR_TRIGGER', 'ACTIVE', 'DUE', 'REVIEW_REQUIRED', 'APPROVED_FOR_DISPOSITION')),
    review_notes                         TEXT NOT NULL DEFAULT '',
    reviewed_by_principal_id             VARCHAR(255),
    reviewed_at                          TIMESTAMPTZ,
    approved_for_disposition_by_principal_id VARCHAR(255),
    approved_for_disposition_at          TIMESTAMPTZ,
    created_by_principal_id              VARCHAR(255) NOT NULL CHECK (created_by_principal_id <> ''),
    created_at                           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((trigger_date IS NULL) = (due_at IS NULL))
);

CREATE INDEX idx_record_retention_states_tenant_state ON record_retention_states (tenant_id, state);

ALTER TABLE record_retention_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_retention_states FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON record_retention_states
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- Forward-only per §5.4, stopping at APPROVED_FOR_DISPOSITION (the
-- dry-run boundary this wave does not cross).
CREATE OR REPLACE FUNCTION reject_retention_state_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'record_retention_states rows are never deleted';
    END IF;
    IF NEW.record_id IS DISTINCT FROM OLD.record_id
        OR NEW.retention_rule_version_id IS DISTINCT FROM OLD.retention_rule_version_id
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'record_retention_state % binding is immutable', OLD.retention_state_id;
    END IF;
    IF OLD.state = 'APPROVED_FOR_DISPOSITION' AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'record_retention_state % is APPROVED_FOR_DISPOSITION and immutable in this wave', OLD.retention_state_id;
    END IF;
    IF OLD.state <> NEW.state THEN
        CASE OLD.state
            WHEN 'WAITING_FOR_TRIGGER' THEN
                IF NEW.state <> 'ACTIVE' THEN
                    RAISE EXCEPTION 'invalid record_retention_state transition from WAITING_FOR_TRIGGER to %', NEW.state;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.state <> 'DUE' THEN
                    RAISE EXCEPTION 'invalid record_retention_state transition from ACTIVE to %', NEW.state;
                END IF;
            WHEN 'DUE' THEN
                IF NEW.state <> 'REVIEW_REQUIRED' THEN
                    RAISE EXCEPTION 'invalid record_retention_state transition from DUE to %', NEW.state;
                END IF;
            WHEN 'REVIEW_REQUIRED' THEN
                IF NEW.state <> 'APPROVED_FOR_DISPOSITION' THEN
                    RAISE EXCEPTION 'invalid record_retention_state transition from REVIEW_REQUIRED to %', NEW.state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown record_retention_state %', OLD.state;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_retention_state_mutation
    BEFORE UPDATE OR DELETE ON record_retention_states
    FOR EACH ROW EXECUTE FUNCTION reject_retention_state_mutation();

-- Legal Holds (§5.5). PARTIALLY_APPLIED/RELEASE_PENDING/ERROR are
-- deliberately not modeled — this wave has no asynchronous apply
-- pipeline that could produce them, and fabricating states nothing
-- can reach is worse than a smaller, honest state set.
CREATE TABLE legal_holds (
    hold_id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                      UUID NOT NULL,
    matter_ref                     TEXT NOT NULL CHECK (matter_ref <> ''),
    authority_ref                  TEXT NOT NULL DEFAULT '',
    hold_reason_code               VARCHAR(64) NOT NULL CHECK (hold_reason_code <> ''),
    status                         VARCHAR(16) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'RELEASED')),
    issued_by_principal_id         VARCHAR(255) NOT NULL CHECK (issued_by_principal_id <> ''),
    issued_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_by_principal_id      VARCHAR(255),
    activated_at                   TIMESTAMPTZ,
    release_reason                 TEXT,
    released_by_principal_id       VARCHAR(255),
    released_at                    TIMESTAMPTZ,
    CHECK ((activated_by_principal_id IS NULL) = (activated_at IS NULL)),
    CHECK ((released_by_principal_id IS NULL) = (released_at IS NULL))
);

CREATE INDEX idx_legal_holds_tenant_status ON legal_holds (tenant_id, status);

ALTER TABLE legal_holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_holds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_holds
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_legal_hold_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'legal_holds rows are never deleted';
    END IF;
    IF OLD.status = 'RELEASED' THEN
        RAISE EXCEPTION 'legal_hold % is RELEASED and immutable', OLD.hold_id;
    END IF;
    IF NEW.matter_ref IS DISTINCT FROM OLD.matter_ref
        OR NEW.authority_ref IS DISTINCT FROM OLD.authority_ref
        OR NEW.hold_reason_code IS DISTINCT FROM OLD.hold_reason_code
        OR NEW.issued_by_principal_id IS DISTINCT FROM OLD.issued_by_principal_id
        OR NEW.issued_at IS DISTINCT FROM OLD.issued_at
    THEN
        RAISE EXCEPTION 'legal_hold % facts are immutable once issued', OLD.hold_id;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'DRAFT' THEN
                IF NEW.status <> 'ACTIVE' THEN
                    RAISE EXCEPTION 'invalid legal_hold transition from DRAFT to %', NEW.status;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.status <> 'RELEASED' THEN
                    RAISE EXCEPTION 'invalid legal_hold transition from ACTIVE to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown legal_hold status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_legal_hold_mutation
    BEFORE UPDATE OR DELETE ON legal_holds
    FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_mutation();

-- Legal Hold Targets: which records are covered by a hold, and
-- whether/when coverage was applied or released. A target is
-- considered to be actively blocking disposition exactly when
-- applied_at IS NOT NULL AND released_at IS NULL (DRC-I10: "legal hold
-- blocks ordinary disposition for every resolved target").
CREATE TABLE legal_hold_targets (
    hold_target_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    hold_id              UUID NOT NULL REFERENCES legal_holds(hold_id),
    record_id            UUID NOT NULL REFERENCES records(record_id),
    applied_at           TIMESTAMPTZ,
    released_at          TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (released_at IS NULL OR applied_at IS NOT NULL),
    CONSTRAINT legal_hold_targets_unique UNIQUE (hold_id, record_id)
);

CREATE INDEX idx_legal_hold_targets_record ON legal_hold_targets (record_id);
CREATE INDEX idx_legal_hold_targets_hold ON legal_hold_targets (hold_id);

ALTER TABLE legal_hold_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_hold_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_hold_targets
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_legal_hold_target_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'legal_hold_targets rows are never deleted';
    END IF;
    IF NEW.hold_id IS DISTINCT FROM OLD.hold_id OR NEW.record_id IS DISTINCT FROM OLD.record_id THEN
        RAISE EXCEPTION 'legal_hold_target % hold/record binding is immutable', OLD.hold_target_id;
    END IF;
    IF OLD.applied_at IS NOT NULL AND NEW.applied_at IS DISTINCT FROM OLD.applied_at THEN
        RAISE EXCEPTION 'legal_hold_target % has already been applied; applied_at cannot change', OLD.hold_target_id;
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW.released_at IS DISTINCT FROM OLD.released_at THEN
        RAISE EXCEPTION 'legal_hold_target % has already been released; released_at cannot change', OLD.hold_target_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_legal_hold_target_mutation
    BEFORE UPDATE OR DELETE ON legal_hold_targets
    FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_target_mutation();
