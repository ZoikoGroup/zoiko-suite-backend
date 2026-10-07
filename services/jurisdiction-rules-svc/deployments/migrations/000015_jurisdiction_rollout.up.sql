-- 000015_jurisdiction_rollout.up.sql
-- ZS-JUR-001 Wave 8 (s32, s35, s36, s37): the governed ROLLOUT of jurisdiction
-- packs, one country or subdivision at a time.
--
-- The standard defines the architecture, not the substantive local law: a
-- jurisdiction family in the portfolio does NOT mean certified rules exist, and
-- production support begins only after local expert review and certification.
-- The tables below hold the portfolio, the Definition of Ready and Definition of
-- Done evidence, the qualified expert approvals, and the launch gate. They hold
-- no regulatory content.

CREATE TABLE jurisdiction_rollouts (
    rollout_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    family_ref        VARCHAR(64)  NOT NULL,
    display_name      TEXT         NOT NULL,
    layer             VARCHAR(24)  NOT NULL CHECK (layer IN ('GLOBAL_REFERENCE', 'GLOBAL_CORE', 'REGIONAL_FRAMEWORK', 'COUNTRY', 'SUBDIVISION')),
    jurisdiction_code VARCHAR(32),
    scope_note        TEXT         NOT NULL DEFAULT '',
    owner             TEXT         NOT NULL,
    support_owner     TEXT,
    status            VARCHAR(24)  NOT NULL DEFAULT 'PLANNED'
        CHECK (status IN ('PLANNED', 'AUTHORING', 'READY', 'LAUNCHED', 'SUSPENDED', 'RETIRED')),
    created_by        TEXT         NOT NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_rollout_family UNIQUE (family_ref),
    CONSTRAINT ck_rollout_family_ref CHECK (family_ref ~ '^[a-z0-9][a-z0-9._-]{0,63}$')
);
CREATE INDEX idx_rollout_jurisdiction ON jurisdiction_rollouts (jurisdiction_code) WHERE jurisdiction_code IS NOT NULL;

-- The packs that make up the rollout.
CREATE TABLE rollout_packs (
    rollout_id UUID NOT NULL REFERENCES jurisdiction_rollouts(rollout_id),
    pack_id    UUID NOT NULL REFERENCES jurisdiction_packs(pack_id),
    linked_by  TEXT NOT NULL,
    linked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (rollout_id, pack_id)
);

-- Definition of Ready (DOR_01..10, s35) and Definition of Done (DOD_01..11, s36) evidence.
-- Append-only: the current answer for an item is its latest attestation.
CREATE TABLE rollout_attestations (
    attestation_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id     UUID        NOT NULL REFERENCES jurisdiction_rollouts(rollout_id),
    checklist      VARCHAR(8)  NOT NULL CHECK (checklist IN ('READY', 'DONE')),
    item_code      VARCHAR(8)  NOT NULL,
    met            BOOLEAN     NOT NULL,
    evidence_ref   TEXT        NOT NULL DEFAULT '',
    attested_by    TEXT        NOT NULL,
    attested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_attestation_item CHECK (
        (checklist = 'READY' AND item_code ~ '^DOR_(0[1-9]|10)$') OR (checklist = 'DONE' AND item_code ~ '^DOD_(0[1-9]|1[01])$')),
    CONSTRAINT ck_attestation_evidence CHECK (NOT met OR length(btrim(evidence_ref)) > 0)
);
CREATE INDEX idx_attestation_latest ON rollout_attestations (rollout_id, item_code, attested_at DESC);

-- Qualified local expert / legal / tax approvals (s20, s38). Append-only.
CREATE TABLE rollout_expert_approvals (
    approval_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id    UUID        NOT NULL REFERENCES jurisdiction_rollouts(rollout_id),
    expert        TEXT        NOT NULL,
    qualification TEXT        NOT NULL,
    scope         TEXT        NOT NULL,
    decision      VARCHAR(8)  NOT NULL CHECK (decision IN ('APPROVE', 'REJECT')),
    notes         TEXT        NOT NULL DEFAULT '',
    decided_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_expert_qualification CHECK (length(btrim(qualification)) > 0 AND length(btrim(scope)) > 0)
);

CREATE TABLE rollout_events (
    event_id    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id  UUID        NOT NULL REFERENCES jurisdiction_rollouts(rollout_id),
    from_status VARCHAR(24),
    to_status   VARCHAR(24) NOT NULL,
    actor       TEXT        NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TRIGGER trg_rollout_attestation_no_update BEFORE UPDATE ON rollout_attestations FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_rollout_attestation_no_delete BEFORE DELETE ON rollout_attestations FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_rollout_expert_no_update BEFORE UPDATE ON rollout_expert_approvals FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_rollout_expert_no_delete BEFORE DELETE ON rollout_expert_approvals FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_rollout_event_no_update BEFORE UPDATE ON rollout_events FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_rollout_event_no_delete BEFORE DELETE ON rollout_events FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_rollout_no_delete BEFORE DELETE ON jurisdiction_rollouts FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- The person responsible for a rollout cannot attest to or approve their own work.
CREATE OR REPLACE FUNCTION jur_rollout_independence() RETURNS trigger AS $$
DECLARE
    who TEXT;
    own TEXT;
BEGIN
    IF TG_TABLE_NAME = 'rollout_attestations' THEN who := NEW.attested_by; ELSE who := NEW.expert; END IF;
    SELECT owner INTO own FROM jurisdiction_rollouts WHERE rollout_id = NEW.rollout_id;
    IF who = own THEN
        RAISE EXCEPTION 'the rollout owner cannot attest to or approve their own rollout' USING ERRCODE = '23514', CONSTRAINT = 'ck_rollout_independent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_rollout_attestation_independent BEFORE INSERT ON rollout_attestations FOR EACH ROW EXECUTE FUNCTION jur_rollout_independence();
CREATE TRIGGER trg_rollout_expert_independent BEFORE INSERT ON rollout_expert_approvals FOR EACH ROW EXECUTE FUNCTION jur_rollout_independence();

-- Status edges and launch gate. Defence in depth: the service computes and explains the
-- blockers; the database refuses the transition regardless of who calls it.
CREATE OR REPLACE FUNCTION jur_rollout_guard() RETURNS trigger AS $$
DECLARE
    met_ready INT;
    met_done  INT;
    approved  INT;
    released  INT;
BEGIN
    IF NEW.status = OLD.status THEN RETURN NEW; END IF;
    IF NOT (
        (OLD.status = 'PLANNED'   AND NEW.status IN ('AUTHORING', 'RETIRED')) OR
        (OLD.status = 'AUTHORING' AND NEW.status IN ('READY', 'RETIRED')) OR
        (OLD.status = 'READY'     AND NEW.status IN ('AUTHORING', 'LAUNCHED', 'RETIRED')) OR
        (OLD.status = 'LAUNCHED'  AND NEW.status IN ('SUSPENDED', 'RETIRED')) OR
        (OLD.status = 'SUSPENDED' AND NEW.status IN ('LAUNCHED', 'RETIRED'))) THEN
        RAISE EXCEPTION 'rollout status % cannot move to %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;

    IF NEW.status IN ('READY', 'LAUNCHED') THEN
        SELECT COUNT(*) INTO met_ready FROM (
            SELECT DISTINCT ON (item_code) met FROM rollout_attestations
            WHERE rollout_id = NEW.rollout_id AND checklist = 'READY' ORDER BY item_code, attested_at DESC, attestation_id DESC) a WHERE met;
        IF met_ready < 10 THEN
            RAISE EXCEPTION 'Definition of Ready is not complete (% of 10 items met)', met_ready USING ERRCODE = '23514';
        END IF;
    END IF;

    IF NEW.status = 'LAUNCHED' THEN
        SELECT COUNT(*) INTO met_done FROM (
            SELECT DISTINCT ON (item_code) met FROM rollout_attestations
            WHERE rollout_id = NEW.rollout_id AND checklist = 'DONE' ORDER BY item_code, attested_at DESC, attestation_id DESC) a WHERE met;
        IF met_done < 11 THEN
            RAISE EXCEPTION 'Definition of Done is not complete (% of 11 items met)', met_done USING ERRCODE = '23514';
        END IF;
        SELECT COUNT(*) INTO approved FROM (
            SELECT DISTINCT ON (expert) decision FROM rollout_expert_approvals
            WHERE rollout_id = NEW.rollout_id ORDER BY expert, decided_at DESC, approval_id DESC) e WHERE decision = 'APPROVE';
        IF approved < 1 THEN
            RAISE EXCEPTION 'no qualified local expert has approved this rollout' USING ERRCODE = '23514';
        END IF;
        SELECT COUNT(*) INTO released FROM rollout_packs rp
            JOIN jurisdiction_pack_versions v ON v.pack_id = rp.pack_id AND v.status = 'RELEASED'
            WHERE rp.rollout_id = NEW.rollout_id;
        IF released < 1 THEN
            RAISE EXCEPTION 'the rollout has no linked pack with a RELEASED version' USING ERRCODE = '23514';
        END IF;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_rollout_guard BEFORE UPDATE ON jurisdiction_rollouts FOR EACH ROW EXECUTE FUNCTION jur_rollout_guard();
