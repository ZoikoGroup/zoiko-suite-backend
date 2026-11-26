-- Refused while any risk acceptance exists: dropping them would leave certifications that rested on them unexplained.
DO $$
BEGIN
    IF to_regclass('dq_risk_acceptances') IS NOT NULL AND EXISTS (SELECT 1 FROM dq_risk_acceptances) THEN
        RAISE EXCEPTION '000010 down refused: risk acceptances have been recorded';
    END IF;
END
$$;

-- The run lifecycle as 000001 defined it: ExceptionsOpen can only move to Reperformed or Failed.
CREATE OR REPLACE FUNCTION enforce_dq_run_lifecycle()
RETURNS trigger AS $$
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
                IF NEW.status NOT IN ('Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid dq run transition from ExceptionsOpen to %; certify requires a clean Reperform first', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown dq run status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TABLE IF EXISTS dq_risk_acceptance_approvals;
DROP TABLE IF EXISTS dq_risk_acceptances;
DROP FUNCTION IF EXISTS enforce_dq_risk_append_only();
DROP FUNCTION IF EXISTS enforce_dq_risk_acceptance_subject();
DROP FUNCTION IF EXISTS enforce_dq_risk_approver_independence();
