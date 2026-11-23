-- 000026_remediation_cases.up.sql
-- ZS-JUR-001 s23, s31 and JUR-NEG-19: rolling back or withdrawing a pack version changes FUTURE resolution only. Outcomes already produced with it are
-- not rewritten (s31: "do not silently rewrite history; explicit remediation workflow"), and until now the rollback response only said a remediation
-- workflow "does not exist". This is that workflow's register.
--
-- A remediation case snapshots the decisions made with a version (in the rolled-back ring and region, or all of them for a withdrawal) and tracks one
-- DISPOSITION per decision: what was done about it, with evidence. The decisions themselves are never changed. Re-evaluating an outcome is the job of the
-- domain service that used it; this register records that it happened, who vouched for it, and refuses to close until every decision has a disposition.
--
--   RECONFIRMED                  the outcome was re-checked and stands (the faulty content did not affect it)
--   NOT_AFFECTED                 the decision did not depend on the faulty content
--   RECALCULATED                 redone with a correct version; new_decision_id names the new decision
--   SUPERSEDED_BY_NEW_DECISION   replaced by a later decision; new_decision_id names it
--
-- Closing needs someone other than the person who opened the case (segregation of duties) and a closure note.

CREATE TABLE remediation_cases (
    case_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    trigger         VARCHAR(16)  NOT NULL,
    -- The scope the decisions were taken from; NULL means every ring and region.
    ring_code       VARCHAR(64),
    region_code     VARCHAR(64),
    reason          TEXT         NOT NULL,
    evidence_ref    TEXT,
    status          VARCHAR(8)   NOT NULL DEFAULT 'OPEN',
    decisions_total INTEGER      NOT NULL,
    opened_by       TEXT         NOT NULL,
    opened_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    closed_by       TEXT,
    closed_at       TIMESTAMPTZ,
    closure_note    TEXT,
    CONSTRAINT ck_remediation_trigger CHECK (trigger IN ('ROLLBACK', 'WITHDRAW', 'MANUAL')),
    CONSTRAINT ck_remediation_status CHECK (status IN ('OPEN', 'CLOSED')),
    CONSTRAINT ck_remediation_reason CHECK (length(btrim(reason)) > 0),
    CONSTRAINT ck_remediation_total CHECK (decisions_total > 0),
    CONSTRAINT ck_remediation_closed_consistent CHECK (
        (status = 'OPEN' AND closed_by IS NULL AND closed_at IS NULL AND closure_note IS NULL)
        OR (status = 'CLOSED' AND closed_by IS NOT NULL AND closed_at IS NOT NULL AND length(btrim(coalesce(closure_note, ''))) > 0))
);
-- One OPEN case per version, trigger and scope: a repeated rollback or withdrawal attaches to the case already open.
CREATE UNIQUE INDEX uq_remediation_open_scope ON remediation_cases (pack_version_id, trigger, COALESCE(ring_code, ''), COALESCE(region_code, '')) WHERE status = 'OPEN';
CREATE INDEX idx_remediation_cases_status ON remediation_cases (status, opened_at DESC);

CREATE TABLE remediation_case_decisions (
    case_id     UUID NOT NULL REFERENCES remediation_cases(case_id),
    decision_id UUID NOT NULL REFERENCES rule_decision_evidence(decision_id),
    PRIMARY KEY (case_id, decision_id)
);

CREATE TABLE remediation_dispositions (
    disposition_id  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id         UUID         NOT NULL,
    decision_id     UUID         NOT NULL,
    disposition     VARCHAR(32)  NOT NULL,
    evidence_ref    TEXT         NOT NULL,
    note            TEXT,
    new_decision_id UUID         REFERENCES rule_decision_evidence(decision_id),
    recorded_by     TEXT         NOT NULL,
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_remediation_one_disposition UNIQUE (case_id, decision_id),
    CONSTRAINT fk_remediation_disposition_decision FOREIGN KEY (case_id, decision_id) REFERENCES remediation_case_decisions(case_id, decision_id),
    CONSTRAINT ck_remediation_disposition CHECK (disposition IN ('RECONFIRMED', 'NOT_AFFECTED', 'RECALCULATED', 'SUPERSEDED_BY_NEW_DECISION')),
    CONSTRAINT ck_remediation_evidence CHECK (length(btrim(evidence_ref)) > 0),
    -- A redone or replaced decision names its replacement; a reconfirmation names none.
    CONSTRAINT ck_remediation_new_decision CHECK (
        (disposition IN ('RECALCULATED', 'SUPERSEDED_BY_NEW_DECISION') AND new_decision_id IS NOT NULL AND new_decision_id <> decision_id)
        OR (disposition IN ('RECONFIRMED', 'NOT_AFFECTED') AND new_decision_id IS NULL))
);

CREATE TRIGGER trg_remediation_decisions_no_update BEFORE UPDATE ON remediation_case_decisions FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_remediation_decisions_no_delete BEFORE DELETE ON remediation_case_decisions FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_remediation_dispositions_no_update BEFORE UPDATE ON remediation_dispositions FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_remediation_dispositions_no_delete BEFORE DELETE ON remediation_dispositions FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_remediation_cases_no_delete BEFORE DELETE ON remediation_cases FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- A disposition is recorded only while the case is OPEN, and a replacement decision must come from a DIFFERENT pack version than the one under
-- remediation (redoing it with the same faulty version remediates nothing).
CREATE FUNCTION jur_remediation_disposition_guard() RETURNS trigger AS $$
DECLARE
    v_status  TEXT;
    v_version UUID;
    v_new_ver UUID;
BEGIN
    SELECT status, pack_version_id INTO v_status, v_version FROM remediation_cases WHERE case_id = NEW.case_id;
    IF v_status <> 'OPEN' THEN
        RAISE EXCEPTION 'a closed remediation case takes no more dispositions' USING ERRCODE = '23514';
    END IF;
    IF NEW.new_decision_id IS NOT NULL THEN
        SELECT pack_version_id INTO v_new_ver FROM rule_decision_evidence WHERE decision_id = NEW.new_decision_id;
        IF v_new_ver IS NOT DISTINCT FROM v_version THEN
            RAISE EXCEPTION 'the replacement decision was made with the same pack version under remediation' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_remediation_disposition_guard BEFORE INSERT ON remediation_dispositions FOR EACH ROW EXECUTE FUNCTION jur_remediation_disposition_guard();

-- A case only moves OPEN -> CLOSED, by someone other than its opener, once EVERY snapshotted decision has a disposition; nothing else about it changes.
CREATE FUNCTION jur_remediation_case_guard() RETURNS trigger AS $$
DECLARE
    v_pending INTEGER;
BEGIN
    IF OLD.status <> 'OPEN' OR NEW.status <> 'CLOSED'
       OR NEW.case_id <> OLD.case_id OR NEW.pack_version_id <> OLD.pack_version_id OR NEW.trigger <> OLD.trigger
       OR NEW.ring_code IS DISTINCT FROM OLD.ring_code OR NEW.region_code IS DISTINCT FROM OLD.region_code OR NEW.reason <> OLD.reason
       OR NEW.evidence_ref IS DISTINCT FROM OLD.evidence_ref OR NEW.decisions_total <> OLD.decisions_total
       OR NEW.opened_by <> OLD.opened_by OR NEW.opened_at <> OLD.opened_at THEN
        RAISE EXCEPTION 'a remediation case can only move OPEN -> CLOSED' USING ERRCODE = '23514';
    END IF;
    IF NEW.closed_by = OLD.opened_by THEN
        RAISE EXCEPTION 'a remediation case must be closed by someone other than the person who opened it' USING ERRCODE = '23514';
    END IF;
    SELECT COUNT(*) INTO v_pending FROM remediation_case_decisions d
      WHERE d.case_id = OLD.case_id
        AND NOT EXISTS (SELECT 1 FROM remediation_dispositions x WHERE x.case_id = d.case_id AND x.decision_id = d.decision_id);
    IF v_pending > 0 THEN
        RAISE EXCEPTION '% decision(s) still have no disposition', v_pending USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_remediation_case_guard BEFORE UPDATE ON remediation_cases FOR EACH ROW EXECUTE FUNCTION jur_remediation_case_guard();
