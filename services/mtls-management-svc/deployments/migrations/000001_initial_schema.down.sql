DROP TABLE IF EXISTS mtls_communication_policies;
DROP TRIGGER IF EXISTS trg_mtls_certificates_revoked_is_terminal ON mtls_certificates;
DROP TABLE IF EXISTS mtls_certificates;
DROP FUNCTION IF EXISTS mtls_certificates_revoked_is_terminal();
