-- Restores 000009's immutability guard (without the two new columns) before
-- dropping them.
CREATE OR REPLACE FUNCTION reject_payee_destination_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payee_destinations rows are never deleted';
    END IF;

    IF OLD.status IN ('SUSPENDED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'destination % is terminal (%), and cannot be modified', OLD.destination_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.party_ref IS DISTINCT FROM OLD.party_ref
        OR NEW.scope IS DISTINCT FROM OLD.scope
        OR NEW.financial_institution IS DISTINCT FROM OLD.financial_institution
        OR (NEW.account_identifier IS DISTINCT FROM OLD.account_identifier
            AND NOT (OLD.account_identifier IS NOT NULL AND OLD.account_identifier <> '' AND NEW.account_identifier IS NULL))
        OR NEW.account_last4 IS DISTINCT FROM OLD.account_last4
        OR NEW.country_code IS DISTINCT FROM OLD.country_code
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.payee_name IS DISTINCT FROM OLD.payee_name
        OR NEW.source_type IS DISTINCT FROM OLD.source_type
        OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
        OR NEW.proposed_by_principal_id IS DISTINCT FROM OLD.proposed_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'destination % authorized fields can never change once proposed', OLD.destination_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP INDEX IF EXISTS idx_payee_destinations_prior_destination;

ALTER TABLE payee_destinations
    DROP COLUMN IF EXISTS prior_destination_id,
    DROP COLUMN IF EXISTS change_risk_classification;
