-- ZS-SVC-Z-001 DQC-05 section 8 (docs/architecture/DQC_GAP_MATRIX.md, step H): "No unresolved BLOCKER exception unless
-- certification profile explicitly permits and records governed risk acceptance."
--
-- A run whose mandatory rule FAILED could never be certified: the only route was a clean reperform. A contract whose
-- certification profile sets allow_risk_acceptance can now be certified over a failed mandatory rule, but only for a
-- result that carries a GOVERNED risk acceptance: requested by one person with a reason and evidence, and approved by
-- TWO other people. Missing evidence (Indeterminate) can never be risk-accepted: that would coerce an absence of
-- evidence into a pass (INV-04).
--
--   dq_risk_acceptances            one per failing result: who asked, why, on what evidence. Append-only.
--   dq_risk_acceptance_approvals   one per approver, distinct, never the requester. Append-only.
--
-- An acceptance is ACCEPTED when it has two approvals. The run lifecycle trigger now lets an ExceptionsOpen run become
-- Certified in exactly one case: the run is a contract run, the contract allows risk acceptance, and every mandatory
-- result that is not a Pass is a Fail with an accepted acceptance. Everything else is refused as before.

CREATE TABLE IF NOT EXISTS dq_risk_acceptances (
    acceptance_id  TEXT         PRIMARY KEY
        CHECK (acceptance_id ~ '^dqk_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id      VARCHAR(64)  NOT NULL,
    run_id         TEXT         NOT NULL REFERENCES dq_runs(run_id),
    result_id      TEXT         NOT NULL REFERENCES dq_results(result_id),
    reason         TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    evidence_refs  TEXT[]       NOT NULL CHECK (cardinality(evidence_refs) > 0 AND dq_refs_nonblank(evidence_refs)),
    requested_by   VARCHAR(128) NOT NULL,
    requested_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (result_id)
);

CREATE TABLE IF NOT EXISTS dq_risk_acceptance_approvals (
    approval_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      VARCHAR(64)  NOT NULL,
    acceptance_id  TEXT         NOT NULL REFERENCES dq_risk_acceptances(acceptance_id),
    approver       VARCHAR(128) NOT NULL,
    reason         TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    approved_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (acceptance_id, approver)
);

CREATE OR REPLACE FUNCTION enforce_dq_risk_append_only()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq risk acceptance records are append-only; a certification resting on one is withdrawn by revoking it';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_risk_acceptance_append_only ON dq_risk_acceptances;
CREATE TRIGGER trigger_dq_risk_acceptance_append_only BEFORE UPDATE OR DELETE ON dq_risk_acceptances
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_risk_append_only();
DROP TRIGGER IF EXISTS trigger_dq_risk_approval_append_only ON dq_risk_acceptance_approvals;
CREATE TRIGGER trigger_dq_risk_approval_append_only BEFORE UPDATE OR DELETE ON dq_risk_acceptance_approvals
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_risk_append_only();

-- What may be risk-accepted: a FAILED, MANDATORY result of a run that is still open, of a contract that permits it.
CREATE OR REPLACE FUNCTION enforce_dq_risk_acceptance_subject()
RETURNS trigger AS $$
DECLARE
    r dq_results%ROWTYPE;
    run_status TEXT;
    allowed BOOLEAN;
BEGIN
    SELECT * INTO r FROM dq_results WHERE result_id = NEW.result_id;
    IF r.run_id IS DISTINCT FROM NEW.run_id THEN
        RAISE EXCEPTION 'the result does not belong to that run';
    END IF;
    IF r.status <> 'Fail' THEN
        RAISE EXCEPTION 'only a Fail can be risk-accepted; a % result is an absence of evidence, not a risk', r.status;
    END IF;
    IF r.mandatory IS NOT TRUE THEN
        RAISE EXCEPTION 'only a mandatory result needs a risk acceptance';
    END IF;
    SELECT status INTO run_status FROM dq_runs WHERE run_id = NEW.run_id;
    IF run_status <> 'ExceptionsOpen' THEN
        RAISE EXCEPTION 'a risk can only be accepted on a run that is ExceptionsOpen (it is %)', run_status;
    END IF;
    SELECT COALESCE((c.body->'certification_profile'->>'allow_risk_acceptance')::boolean, FALSE) INTO allowed
      FROM dq_runs q JOIN dqc_contract_versions c ON c.contract_id = q.contract_id WHERE q.run_id = NEW.run_id;
    IF allowed IS NOT TRUE THEN
        RAISE EXCEPTION 'the contract does not permit risk acceptance';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_risk_acceptance_subject ON dq_risk_acceptances;
CREATE TRIGGER trigger_dq_risk_acceptance_subject BEFORE INSERT ON dq_risk_acceptances
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_risk_acceptance_subject();

-- Independence: nobody approves their own request.
CREATE OR REPLACE FUNCTION enforce_dq_risk_approver_independence()
RETURNS trigger AS $$
DECLARE
    requester TEXT;
BEGIN
    SELECT requested_by INTO requester FROM dq_risk_acceptances WHERE acceptance_id = NEW.acceptance_id;
    IF NEW.approver = requester THEN
        RAISE EXCEPTION 'the person who requested a risk acceptance cannot approve it';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_risk_approver_independence ON dq_risk_acceptance_approvals;
CREATE TRIGGER trigger_dq_risk_approver_independence BEFORE INSERT ON dq_risk_acceptance_approvals
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_risk_approver_independence();

-- The run lifecycle: ExceptionsOpen -> Certified only over fully, governedly accepted risk.
CREATE OR REPLACE FUNCTION enforce_dq_run_lifecycle()
RETURNS trigger AS $$
DECLARE
    allowed BOOLEAN;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dq run cannot be deleted';
    END IF;
    IF OLD.status IN ('Certified', 'Reperformed', 'Failed') THEN
        RAISE EXCEPTION 'dq run % is terminal (%) and immutable', OLD.run_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Running' THEN
                IF NEW.status NOT IN ('ExceptionsOpen', 'Certified', 'Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid dq run transition from Running to %', NEW.status;
                END IF;
            WHEN 'ExceptionsOpen' THEN
                IF NEW.status = 'Certified' THEN
                    -- Only over governed risk acceptance: a contract run, a contract that permits it, and every
                    -- mandatory result that is not a Pass is a Fail with TWO approvals.
                    IF NEW.contract_id IS NULL THEN
                        RAISE EXCEPTION 'a rule-set run with exceptions open cannot be certified; reperform it cleanly';
                    END IF;
                    SELECT COALESCE((c.body->'certification_profile'->>'allow_risk_acceptance')::boolean, FALSE) INTO allowed
                      FROM dqc_contract_versions c WHERE c.contract_id = NEW.contract_id;
                    IF allowed IS NOT TRUE THEN
                        RAISE EXCEPTION 'the contract does not permit certifying over a failed rule; reperform it cleanly';
                    END IF;
                    IF EXISTS (
                        SELECT 1 FROM dq_results r
                         WHERE r.run_id = NEW.run_id AND r.mandatory IS TRUE AND r.status <> 'Pass'
                           AND (r.status <> 'Fail'
                                OR (SELECT count(*) FROM dq_risk_acceptance_approvals a
                                      JOIN dq_risk_acceptances k ON k.acceptance_id = a.acceptance_id
                                     WHERE k.result_id = r.result_id) < 2)
                    ) THEN
                        RAISE EXCEPTION 'dq run % has a mandatory result that is neither a Pass nor a Fail carrying two approvals of its risk acceptance', OLD.run_id;
                    END IF;
                ELSIF NEW.status NOT IN ('Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid dq run transition from ExceptionsOpen to %; certify requires a clean Reperform first', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown dq run status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE dq_risk_acceptances ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_risk_acceptances FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dq_risk_acceptances_tenant_isolation ON dq_risk_acceptances;
CREATE POLICY dq_risk_acceptances_tenant_isolation ON dq_risk_acceptances
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_risk_acceptance_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_risk_acceptance_approvals FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dq_risk_approvals_tenant_isolation ON dq_risk_acceptance_approvals;
CREATE POLICY dq_risk_approvals_tenant_isolation ON dq_risk_acceptance_approvals
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
