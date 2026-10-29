-- Global Accounting Kernel (standard §8.1, §8.2): a journal line links to the tax determination it rests on, and a journal header
-- carries the reference of the evidence chain behind it.

ALTER TABLE journal_lines
    ADD COLUMN tax_detail_ref VARCHAR(200);

ALTER TABLE journal_headers
    ADD COLUMN evidence_bundle_id VARCHAR(200);

-- An evidence bundle is identified by what it holds: the same set of references, in any order, is the same bundle. A header that
-- is given an explicit bundle id (an event that names its evidence bundle) keeps it; otherwise the id follows the references
-- the header carries, and moves with them while the journal is still a draft. A posted journal's references and bundle are
-- fixed by the posted-journal guard like every other column.
CREATE OR REPLACE FUNCTION evidence_bundle_digest(refs TEXT[]) RETURNS VARCHAR AS $$
    SELECT CASE WHEN refs IS NULL OR cardinality(refs) = 0 THEN NULL
                ELSE 'ebnd_' || substr(encode(sha256(convert_to((SELECT string_agg(r, E'\n' ORDER BY r) FROM unnest(refs) r), 'UTF8')), 'hex'), 1, 32) END
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION journal_header_evidence_bundle() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.evidence_bundle_id IS NULL THEN
            NEW.evidence_bundle_id := evidence_bundle_digest(NEW.evidence_refs);
        END IF;
    ELSIF NEW.evidence_refs IS DISTINCT FROM OLD.evidence_refs
          AND NEW.evidence_bundle_id IS NOT DISTINCT FROM OLD.evidence_bundle_id
          AND OLD.evidence_bundle_id IS NOT DISTINCT FROM evidence_bundle_digest(OLD.evidence_refs) THEN
        NEW.evidence_bundle_id := evidence_bundle_digest(NEW.evidence_refs);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER journal_headers_evidence_bundle BEFORE INSERT OR UPDATE OF evidence_refs ON journal_headers
    FOR EACH ROW EXECUTE FUNCTION journal_header_evidence_bundle();

CREATE INDEX idx_journal_headers_evidence_bundle ON journal_headers (tenant_id, evidence_bundle_id) WHERE evidence_bundle_id IS NOT NULL;
CREATE INDEX idx_journal_lines_tax_detail ON journal_lines (tenant_id, tax_detail_ref) WHERE tax_detail_ref IS NOT NULL;
