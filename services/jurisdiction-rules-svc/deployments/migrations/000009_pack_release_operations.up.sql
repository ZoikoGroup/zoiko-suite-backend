-- 000009_pack_release_operations.up.sql
-- ZS-JUR-001 Wave 7: release, withdrawal, deployment rings and regions,
-- rollback, emergency hotfix, verification-failure telemetry and source-change
-- intake. Additive over 000005-000008. All of it is evidence: append-only, with
-- the few allowed state moves enforced by trigger.

-- ── release lifecycle events (s23) ──────────────────────────────────────────
CREATE TABLE pack_release_events (
    event_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    action          VARCHAR(16)  NOT NULL,
    from_status     VARCHAR(32)  NOT NULL,
    to_status       VARCHAR(32)  NOT NULL,
    actor           TEXT         NOT NULL,
    reason          TEXT,
    evidence_ref    TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_release_action CHECK (action IN ('RELEASE', 'WITHDRAW', 'BLOCK', 'UNBLOCK')),
    -- Withdrawing or blocking a regulatory artifact must say why (s23, s25).
    CONSTRAINT ck_release_reason_required CHECK (action NOT IN ('WITHDRAW', 'BLOCK') OR length(btrim(coalesce(reason, ''))) > 0)
);
CREATE INDEX idx_pack_release_events_version ON pack_release_events (pack_version_id, created_at);
CREATE TRIGGER trg_pack_release_events_append_only BEFORE UPDATE ON pack_release_events FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_release_events_no_delete   BEFORE DELETE ON pack_release_events FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- RELEASED, WITHDRAWN and EMERGENCY_BLOCKED can only be reached through a
-- recorded release event written in the same transaction, never by a bare
-- status update.
CREATE FUNCTION jur_release_status_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status AND NEW.status IN ('RELEASED', 'WITHDRAWN', 'EMERGENCY_BLOCKED')
       AND NOT EXISTS (SELECT 1 FROM pack_release_events
                       WHERE pack_version_id = NEW.pack_version_id AND from_status = OLD.status
                         AND to_status = NEW.status AND created_at = NOW()) THEN
        RAISE EXCEPTION 'status % is reached only through a recorded release event', NEW.status USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_release_status_guard BEFORE UPDATE ON jurisdiction_pack_versions
    FOR EACH ROW EXECUTE FUNCTION jur_release_status_guard();

-- ── regions and rings (s23, s27 PackDeployment) ─────────────────────────────
CREATE TABLE deployment_regions (
    region_code             VARCHAR(64)  PRIMARY KEY,
    description             TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    CONSTRAINT ck_region_code_shape CHECK (region_code ~ '^[a-z0-9][a-z0-9._-]*$')
);

CREATE TABLE deployment_rings (
    ring_code               VARCHAR(64)  PRIMARY KEY,
    -- Rollout order: a version is promoted to ring N only after ring N-1.
    ordinal                 INTEGER      NOT NULL,
    -- How long a version must have been active in THIS ring (and region)
    -- before it may be promoted to the next one.
    min_soak_seconds        INTEGER      NOT NULL DEFAULT 0,
    description             TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    CONSTRAINT uq_deployment_ring_ordinal UNIQUE (ordinal),
    CONSTRAINT ck_ring_ordinal CHECK (ordinal >= 1),
    CONSTRAINT ck_ring_soak CHECK (min_soak_seconds >= 0),
    CONSTRAINT ck_ring_code_shape CHECK (ring_code ~ '^[a-z0-9][a-z0-9._-]*$')
);
CREATE TRIGGER trg_deployment_regions_append_only BEFORE UPDATE ON deployment_regions FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_deployment_regions_no_delete   BEFORE DELETE ON deployment_regions FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_deployment_rings_append_only   BEFORE UPDATE ON deployment_rings   FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_deployment_rings_no_delete     BEFORE DELETE ON deployment_rings   FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

CREATE TABLE pack_deployments (
    deployment_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id  UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    ring_code        VARCHAR(64)  NOT NULL REFERENCES deployment_rings(ring_code),
    region_code      VARCHAR(64)  NOT NULL REFERENCES deployment_regions(region_code),
    status           VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE',
    deployed_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deployed_by      TEXT         NOT NULL,
    -- Set only when ring order was skipped under a documented hotfix exception.
    exception_reason TEXT,
    rolled_back_at   TIMESTAMPTZ,
    rolled_back_by   TEXT,
    rollback_reason  TEXT,
    CONSTRAINT ck_deployment_status CHECK (status IN ('ACTIVE', 'ROLLED_BACK')),
    CONSTRAINT ck_deployment_rollback_consistent CHECK (
        (status = 'ACTIVE' AND rolled_back_at IS NULL AND rolled_back_by IS NULL AND rollback_reason IS NULL)
        OR (status = 'ROLLED_BACK' AND rolled_back_at IS NOT NULL AND rolled_back_by IS NOT NULL AND length(btrim(coalesce(rollback_reason, ''))) > 0))
);
-- A version is ACTIVE at most once per ring and region; history keeps the rest.
CREATE UNIQUE INDEX uq_pack_deployment_active ON pack_deployments (pack_version_id, ring_code, region_code) WHERE status = 'ACTIVE';
CREATE INDEX idx_pack_deployment_scope ON pack_deployments (ring_code, region_code, status);

CREATE FUNCTION jur_deployment_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'RELEASED' THEN
            RAISE EXCEPTION 'only a RELEASED pack version can be deployed' USING ERRCODE = '23514';
        END IF;
        IF NEW.status <> 'ACTIVE' THEN
            RAISE EXCEPTION 'a deployment starts ACTIVE' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'ACTIVE' OR NEW.status <> 'ROLLED_BACK'
       OR NEW.deployment_id <> OLD.deployment_id OR NEW.pack_version_id <> OLD.pack_version_id
       OR NEW.ring_code <> OLD.ring_code OR NEW.region_code <> OLD.region_code
       OR NEW.deployed_at <> OLD.deployed_at OR NEW.deployed_by <> OLD.deployed_by
       OR NEW.exception_reason IS DISTINCT FROM OLD.exception_reason THEN
        RAISE EXCEPTION 'a deployment can only move ACTIVE -> ROLLED_BACK' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_deployment_guard BEFORE INSERT OR UPDATE ON pack_deployments
    FOR EACH ROW EXECUTE FUNCTION jur_deployment_guard();
CREATE TRIGGER trg_pack_deployment_no_delete BEFORE DELETE ON pack_deployments
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- ── runtime verification failures (JUR-NEG-04, s29) ─────────────────────────
CREATE TABLE pack_verification_failures (
    failure_id      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    ring_code       VARCHAR(64),
    region_code     VARCHAR(64),
    reasons         JSONB        NOT NULL,
    detected_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_pack_verification_failures ON pack_verification_failures (pack_version_id, detected_at DESC);
CREATE TRIGGER trg_pack_verification_failures_append_only BEFORE UPDATE ON pack_verification_failures FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_verification_failures_no_delete   BEFORE DELETE ON pack_verification_failures FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- Which resolver instance (ring and region) produced a decision.
ALTER TABLE rule_decision_evidence
    ADD COLUMN resolver_ring   VARCHAR(64),
    ADD COLUMN resolver_region VARCHAR(64);

-- ── emergency hotfix (s24) ──────────────────────────────────────────────────
CREATE TABLE pack_hotfixes (
    pack_version_id         UUID         PRIMARY KEY REFERENCES jurisdiction_pack_versions(pack_version_id),
    severity                VARCHAR(8)   NOT NULL,
    scope_summary           TEXT         NOT NULL,
    incident_ref            TEXT         NOT NULL,
    -- Only this principal may document an exception to ring order.
    incident_commander      TEXT         NOT NULL,
    -- The known prior compatible release, identified BEFORE promotion.
    rollback_target_version VARCHAR(32)  NOT NULL,
    declared_by             TEXT         NOT NULL,
    declared_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- The mandatory retrospective is due within the SLA.
    retro_due_at            TIMESTAMPTZ  NOT NULL,
    retro_completed_at      TIMESTAMPTZ,
    retro_completed_by      TEXT,
    retro_note              TEXT,
    CONSTRAINT ck_hotfix_severity CHECK (severity IN ('P0', 'P1')),
    CONSTRAINT ck_hotfix_text CHECK (length(btrim(scope_summary)) > 0 AND length(btrim(incident_ref)) > 0 AND length(btrim(incident_commander)) > 0),
    CONSTRAINT ck_hotfix_retro_consistent CHECK (
        (retro_completed_at IS NULL AND retro_completed_by IS NULL AND retro_note IS NULL)
        OR (retro_completed_at IS NOT NULL AND retro_completed_by IS NOT NULL AND length(btrim(coalesce(retro_note, ''))) > 0))
);
CREATE FUNCTION jur_hotfix_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Declared before certification, so the enhanced review always applies.
        IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) NOT IN ('DRAFT', 'REVIEW') THEN
            RAISE EXCEPTION 'a hotfix must be declared before the version is certified' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.retro_completed_at IS NOT NULL
       OR NEW.pack_version_id <> OLD.pack_version_id OR NEW.severity <> OLD.severity OR NEW.scope_summary <> OLD.scope_summary
       OR NEW.incident_ref <> OLD.incident_ref OR NEW.incident_commander <> OLD.incident_commander
       OR NEW.rollback_target_version <> OLD.rollback_target_version OR NEW.declared_by <> OLD.declared_by
       OR NEW.declared_at <> OLD.declared_at OR NEW.retro_due_at <> OLD.retro_due_at THEN
        RAISE EXCEPTION 'a hotfix record is immutable; only the retrospective can be completed, once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_hotfix_guard BEFORE INSERT OR UPDATE ON pack_hotfixes FOR EACH ROW EXECUTE FUNCTION jur_hotfix_guard();
CREATE TRIGGER trg_pack_hotfix_no_delete BEFORE DELETE ON pack_hotfixes FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- A hotfix needs at least TWO distinct independent approving reviewers
-- (s24, JUR-NEG-14). This replaces the 000007 certification guard with the
-- same checks plus that one.
CREATE OR REPLACE FUNCTION jur_certification_guard() RETURNS trigger AS $$
DECLARE
    a_signature TEXT;
    a_digest    TEXT;
    a_compiler  TEXT;
    r_passed    BOOLEAN;
    r_bundle    TEXT;
    r_artifact  TEXT;
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
        RAISE EXCEPTION 'only a version under REVIEW can be certified' USING ERRCODE = '23514';
    END IF;
    SELECT signature, artifact_digest, compiled_by_principal_id INTO a_signature, a_digest, a_compiler
        FROM pack_artifacts WHERE pack_version_id = NEW.pack_version_id;
    IF a_digest IS NULL OR a_signature IS NULL OR a_digest <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a signed artifact with the certified digest' USING ERRCODE = '23514';
    END IF;
    SELECT passed, bundle_digest, artifact_digest INTO r_passed, r_bundle, r_artifact
        FROM pack_test_runs WHERE run_id = NEW.test_run_id AND pack_version_id = NEW.pack_version_id;
    IF r_passed IS DISTINCT FROM TRUE OR r_bundle <> NEW.bundle_digest OR r_artifact <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a PASSED test run of exactly this artifact and bundle' USING ERRCODE = '23514';
    END IF;
    IF NEW.certified_by = (SELECT created_by_principal_id FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by = a_compiler
       OR NEW.certified_by IN (SELECT submitted_by FROM pack_test_bundles WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by IN (SELECT reviewer FROM pack_reviews WHERE pack_version_id = NEW.pack_version_id AND decision = 'APPROVE') THEN
        RAISE EXCEPTION 'the certifier must be independent of the author, compiler, test authors and reviewers'
            USING ERRCODE = '23514', CONSTRAINT = 'ck_pack_cert_independent';
    END IF;
    IF EXISTS (SELECT 1 FROM pack_hotfixes WHERE pack_version_id = NEW.pack_version_id) AND NEW.min_reviews < 2 THEN
        RAISE EXCEPTION 'an emergency hotfix requires at least two independent approving reviewers' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── source-change intake (s29 "source-change watch", s31) ───────────────────
-- A signal that an authority's material may have changed. It opens a CONTROLLED
-- REVIEW; it never edits or publishes a rule.
CREATE TABLE source_change_notices (
    notice_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id         UUID         REFERENCES jurisdictions(jurisdiction_id),
    authority               TEXT         NOT NULL,
    change_type             VARCHAR(32)  NOT NULL,
    title                   TEXT         NOT NULL,
    location                TEXT,
    observed_hash           VARCHAR(71),
    affected_source_id      UUID         REFERENCES regulatory_sources(source_id),
    detail                  TEXT,
    status                  VARCHAR(16)  NOT NULL DEFAULT 'OPEN',
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    reviewer                TEXT,
    review_started_at       TIMESTAMPTZ,
    closed_at               TIMESTAMPTZ,
    closed_by               TEXT,
    outcome                 VARCHAR(32),
    outcome_note            TEXT,
    linked_interpretation_id UUID        REFERENCES interpretation_records(interpretation_id),
    CONSTRAINT ck_notice_type CHECK (change_type IN ('FUTURE_DATED_LAW', 'SCHEMA_OR_CODE_LIST_UPDATE', 'CLARIFICATION',
        'RETROACTIVE_CHANGE', 'AUTHORITY_PROCESS_CHANGE', 'SOURCE_WITHDRAWN', 'DISPUTED_INTERPRETATION')),
    CONSTRAINT ck_notice_status CHECK (status IN ('OPEN', 'UNDER_REVIEW', 'CLOSED')),
    CONSTRAINT ck_notice_outcome CHECK (outcome IS NULL OR outcome IN ('NO_CHANGE', 'INTERPRETATION_RECORDED', 'RULE_CHANGE_PLANNED', 'CAPABILITY_BLOCKED')),
    CONSTRAINT ck_notice_hash CHECK (observed_hash IS NULL OR observed_hash ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_notice_reviewer_independent CHECK (reviewer IS NULL OR reviewer <> created_by_principal_id),
    CONSTRAINT ck_notice_closed_complete CHECK (
        (status <> 'CLOSED') OR (reviewer IS NOT NULL AND closed_at IS NOT NULL AND closed_by IS NOT NULL AND outcome IS NOT NULL AND length(btrim(coalesce(outcome_note, ''))) > 0))
);
CREATE INDEX idx_source_change_open ON source_change_notices (status, created_at) WHERE status <> 'CLOSED';
CREATE FUNCTION jur_notice_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.notice_id <> OLD.notice_id OR NEW.authority <> OLD.authority OR NEW.change_type <> OLD.change_type OR NEW.title <> OLD.title
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id
       OR OLD.status = 'CLOSED'
       OR NOT ((OLD.status, NEW.status) IN (('OPEN','OPEN'), ('OPEN','UNDER_REVIEW'), ('UNDER_REVIEW','UNDER_REVIEW'), ('UNDER_REVIEW','CLOSED'), ('OPEN','CLOSED'))) THEN
        RAISE EXCEPTION 'illegal change to a source-change notice' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_source_change_guard BEFORE UPDATE ON source_change_notices FOR EACH ROW EXECUTE FUNCTION jur_notice_guard();
CREATE TRIGGER trg_source_change_no_delete BEFORE DELETE ON source_change_notices FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
