DROP INDEX uq_payee_destinations_active_scope;
CREATE UNIQUE INDEX uq_payee_destinations_active_scope
    ON payee_destinations (party_ref, scope)
    WHERE status = 'ACTIVE';

DROP INDEX uq_payee_destinations_fingerprint;
CREATE UNIQUE INDEX uq_payee_destinations_fingerprint
    ON payee_destinations (party_ref, fingerprint)
    WHERE status <> 'SUPERSEDED';
