-- Reverses 012. Dropping these columns loses the retrievable
-- raw-file-in-document-vault-svc reference for every statement that has
-- one; the file itself stays in document-vault-svc, but nothing here would
-- point to it any more.
ALTER TABLE bank_statements
    DROP COLUMN IF EXISTS raw_file_document_id,
    DROP COLUMN IF EXISTS raw_file_checksum_sha256;
