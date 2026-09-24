-- BIZ-01's GetLinkedObjects query: "what business objects reference this
-- document." No data source existed for this anywhere in the codebase —
-- callers fetch a document by ID today, but nothing records the reverse
-- link. This is that real registry, not an inference/guess: a caller
-- explicitly links a document to whatever object it attached the
-- document to, via LinkDocument.
--
-- linked_object_type/linked_object_id are opaque, caller-supplied
-- strings, not a foreign key — the linked object lives in another
-- service's own database (e.g. expense-claim-svc's expense_claims,
-- workflow-svc's workflow_instances), which this service has no access
-- to and must not assume a schema for.
--
-- Append-only, same posture as document_access_log: a link is a
-- historical fact ("X was linked to Y at time T"), never edited or
-- deleted. If a link becomes stale, the caller creates a new,
-- superseding fact rather than erasing the old one — consistent with
-- this service's own "declared records are never destructively
-- overwritten" invariant.
CREATE TABLE document_links (
    link_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id           UUID NOT NULL REFERENCES documents(document_id),
    linked_object_type    VARCHAR(100) NOT NULL CHECK (linked_object_type <> ''),
    linked_object_id      VARCHAR(255) NOT NULL CHECK (linked_object_id <> ''),
    linked_by_principal_id VARCHAR(255) NOT NULL CHECK (linked_by_principal_id <> ''),
    correlation_id        VARCHAR(255),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The same document linked to the same object twice is a caller bug,
    -- not a new fact — reject rather than accumulate duplicates.
    UNIQUE (document_id, linked_object_type, linked_object_id)
);

CREATE INDEX idx_document_links_document ON document_links (document_id, created_at);

-- Same transitive tenant-scoping idiom as document_versions/
-- document_access_log (migration 000002): document_links carries no
-- tenant_id of its own, scoped instead through its parent document, whose
-- own policy already filters by tenant.
ALTER TABLE document_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_links FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON document_links FOR ALL
    USING (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_links.document_id))
    WITH CHECK (EXISTS (SELECT 1 FROM documents d WHERE d.document_id = document_links.document_id));

CREATE OR REPLACE FUNCTION reject_document_link_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'document_links rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_document_link_mutation
    BEFORE UPDATE OR DELETE ON document_links
    FOR EACH ROW EXECUTE FUNCTION reject_document_link_mutation();
