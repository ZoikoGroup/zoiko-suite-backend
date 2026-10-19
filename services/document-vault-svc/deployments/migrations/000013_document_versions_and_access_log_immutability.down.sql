-- 000013_document_versions_and_access_log_immutability.down.sql

DROP TRIGGER IF EXISTS trg_document_access_log_reject_truncate ON document_access_log;
DROP TRIGGER IF EXISTS trg_document_access_log_reject_delete ON document_access_log;
DROP TRIGGER IF EXISTS trg_document_access_log_reject_update ON document_access_log;
DROP TRIGGER IF EXISTS trg_document_versions_reject_truncate ON document_versions;
DROP TRIGGER IF EXISTS trg_document_versions_reject_delete ON document_versions;
DROP TRIGGER IF EXISTS trg_document_versions_reject_update ON document_versions;
DROP FUNCTION IF EXISTS reject_append_only_mutation();
