-- A generated evidence manifest is immutable evidence (docs/architecture/01-backend.md:
-- "The platform produces immutable operational evidence"; "Silent overwrite behavior
-- is architecturally prohibited"; domain.EvidenceManifest: once GENERATED "this row ...
-- never mutated"). Migration 000002 left the row unprotected because it has a real
-- lifecycle: PENDING -> GENERATED / FAILED, backfilling checksum_sha256, generated_at
-- and failure_reason. That lifecycle is the ONLY mutation permitted, and only out of
-- PENDING: GENERATED and FAILED are terminal, a retry creates a new manifest.
CREATE OR REPLACE FUNCTION reject_manifest_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evidence_manifests rows are never deleted (manifest %)', OLD.manifest_id;
    END IF;
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'evidence manifest % is % and immutable', OLD.manifest_id, OLD.status;
    END IF;
    IF NEW.manifest_id IS DISTINCT FROM OLD.manifest_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.scenario_type IS DISTINCT FROM OLD.scenario_type
        OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at THEN
        RAISE EXCEPTION 'evidence manifest % identity and scope columns are immutable', OLD.manifest_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS evidence_manifests_immutable ON evidence_manifests;
CREATE TRIGGER evidence_manifests_immutable
    BEFORE UPDATE OR DELETE ON evidence_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_manifest_mutation();
