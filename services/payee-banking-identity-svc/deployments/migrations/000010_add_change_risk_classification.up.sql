-- The literal fix for §4.10's "server-resolved context" line's
-- "change-risk classification; prior version" (SPEC_DEVIATIONS A3).
-- Computed once at propose time (see internal/store/pg_store.go's
-- ProposeDestination) by comparing the candidate against the party/scope's
-- current ACTIVE row, if any, and never changed afterward.
ALTER TABLE payee_destinations
    ADD COLUMN change_risk_classification TEXT NOT NULL DEFAULT 'NEW_DESTINATION'
        CHECK (change_risk_classification IN ('NEW_DESTINATION', 'DESTINATION_CHANGE')),
    ADD COLUMN prior_destination_id UUID NULL REFERENCES payee_destinations (destination_id);

CREATE INDEX idx_payee_destinations_prior_destination ON payee_destinations (prior_destination_id);

-- Extend the existing immutability guard (000002/000009) to cover the two
-- new columns: like every other identity/lineage field on this row, a
-- destination's change-risk classification and the prior version it was
-- compared against are decided once, at propose time, and can never be
-- rewritten afterward.
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
        OR NEW.change_risk_classification IS DISTINCT FROM OLD.change_risk_classification
        OR NEW.prior_destination_id IS DISTINCT FROM OLD.prior_destination_id
    THEN
        RAISE EXCEPTION 'destination % authorized fields can never change once proposed', OLD.destination_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
