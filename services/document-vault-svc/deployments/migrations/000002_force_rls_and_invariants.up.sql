-- Row-level security, which this service had none of.
--
-- Unlike its siblings there was no ENABLE-without-FORCE here to correct: the
-- three tables had no row security at all, no policy, and nothing that would
-- have failed closed if the application's own tenant predicate were wrong or
-- absent. It WAS absent -- the store's predicate read
-- `($2::uuid IS NULL OR tenant_id = $2::uuid)`, which disables itself when the
-- request carries no X-Tenant-Id -- so on a vault holding CONFIDENTIAL and
-- RESTRICTED documents there was no tenant boundary of any kind for a caller
-- who simply omitted the header.
--
-- FORCE from the start, because these services connect as the table owner and
-- the owner is exempt from row security unless the table is declared FORCE.
-- Enabling without forcing here would install the same decorative control the
-- rest of the estate has had to correct.
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON documents FOR ALL
    USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- document_versions and document_access_log carry no tenant_id of their own --
-- they hang off documents. Scoping them through that parent rather than
-- denormalising a tenant column keeps one source of truth for which tenant a
-- document belongs to, and means a version or an access-log row can never
-- disagree with its document about who owns it.
--
-- EXISTS against documents is itself subject to the policy above, so a version
-- whose parent is invisible to this tenant is invisible too.
ALTER TABLE document_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_versions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON document_versions FOR ALL
    USING (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_versions.document_id))
    WITH CHECK (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_versions.document_id));

ALTER TABLE document_access_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_access_log FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON document_access_log FOR ALL
    USING (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_access_log.document_id))
    WITH CHECK (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_access_log.document_id));

-- The access log is the evidence of who read what. It was accepting the
-- literal string 'unknown' as a principal, because the handler substituted
-- that when a request carried no identity -- so the append-only record that
-- exists to answer "who downloaded this RESTRICTED document" could answer
-- "unknown", and read as though it had answered.
--
-- NOT VALID: existing rows are preserved. They are the true record of what the
-- service actually did, and a migration must not rewrite an audit trail to
-- make a constraint pass. Run VALIDATE CONSTRAINT once the backlog is known
-- clean.
ALTER TABLE document_access_log
    ADD CONSTRAINT document_access_log_principal_identified
    CHECK (accessed_by_principal_id <> '' AND accessed_by_principal_id <> 'unknown') NOT VALID;

ALTER TABLE documents
    ADD CONSTRAINT documents_created_by_identified
    CHECK (created_by_principal_id <> '' AND created_by_principal_id <> 'unknown') NOT VALID;

ALTER TABLE document_versions
    ADD CONSTRAINT document_versions_created_by_identified
    CHECK (created_by_principal_id <> '' AND created_by_principal_id <> 'unknown') NOT VALID;

-- current_version must be a real version number. A document claiming version 0
-- or a negative one points at a version row that cannot exist.
ALTER TABLE documents
    ADD CONSTRAINT documents_current_version_positive
    CHECK (current_version >= 1) NOT VALID;

ALTER TABLE document_versions
    ADD CONSTRAINT document_versions_version_positive
    CHECK (version >= 1) NOT VALID;

-- A checksum is the integrity control (§8.3) and is recomputed on every read.
-- A row with a blank one has no integrity control at all.
ALTER TABLE document_versions
    ADD CONSTRAINT document_versions_checksum_present
    CHECK (length(checksum_sha256) = 64) NOT VALID;

ALTER TABLE document_versions
    ADD CONSTRAINT document_versions_size_nonnegative
    CHECK (size_bytes >= 0) NOT VALID;

-- The access log is read newest-first and is paged now; accessed_at alone is
-- not a total order, so the index carries the primary key as a tiebreaker for
-- the same reason the ORDER BY does.
CREATE INDEX IF NOT EXISTS idx_document_access_log_document_paged
    ON document_access_log (document_id, accessed_at DESC, access_log_id DESC);

-- The register is listed per tenant, newest first.
CREATE INDEX IF NOT EXISTS idx_documents_tenant_created
    ON documents (tenant_id, created_at DESC, document_id DESC);
