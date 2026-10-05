-- 000009 down: removes release/deployment operations and restores the 000007 certification guard.
DROP TABLE IF EXISTS source_change_notices;
DROP FUNCTION IF EXISTS jur_notice_guard();
DROP TABLE IF EXISTS pack_hotfixes;
DROP FUNCTION IF EXISTS jur_hotfix_guard();
ALTER TABLE rule_decision_evidence DROP COLUMN IF EXISTS resolver_region, DROP COLUMN IF EXISTS resolver_ring;
DROP TABLE IF EXISTS pack_verification_failures;
DROP TABLE IF EXISTS pack_deployments;
DROP FUNCTION IF EXISTS jur_deployment_guard();
DROP TABLE IF EXISTS deployment_rings;
DROP TABLE IF EXISTS deployment_regions;
DROP TRIGGER IF EXISTS trg_pack_release_status_guard ON jurisdiction_pack_versions;
DROP FUNCTION IF EXISTS jur_release_status_guard();
DROP TABLE IF EXISTS pack_release_events;

CREATE OR REPLACE FUNCTION jur_certification_guard() RETURNS trigger AS $$
DECLARE
    a_signature TEXT;
    a_digest    TEXT;
    a_compiler  TEXT;
    r_passed    BOOLEAN;
    r_bundle    TEXT;
    r_artifact  TEXT;
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
        RAISE EXCEPTION 'only a version under REVIEW can be certified' USING ERRCODE = '23514';
    END IF;
    SELECT signature, artifact_digest, compiled_by_principal_id INTO a_signature, a_digest, a_compiler
        FROM pack_artifacts WHERE pack_version_id = NEW.pack_version_id;
    IF a_digest IS NULL OR a_signature IS NULL OR a_digest <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a signed artifact with the certified digest' USING ERRCODE = '23514';
    END IF;
    SELECT passed, bundle_digest, artifact_digest INTO r_passed, r_bundle, r_artifact
        FROM pack_test_runs WHERE run_id = NEW.test_run_id AND pack_version_id = NEW.pack_version_id;
    IF r_passed IS DISTINCT FROM TRUE OR r_bundle <> NEW.bundle_digest OR r_artifact <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a PASSED test run of exactly this artifact and bundle' USING ERRCODE = '23514';
    END IF;
    IF NEW.certified_by = (SELECT created_by_principal_id FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by = a_compiler
       OR NEW.certified_by IN (SELECT submitted_by FROM pack_test_bundles WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by IN (SELECT reviewer FROM pack_reviews WHERE pack_version_id = NEW.pack_version_id AND decision = 'APPROVE') THEN
        RAISE EXCEPTION 'the certifier must be independent of the author, compiler, test authors and reviewers'
            USING ERRCODE = '23514', CONSTRAINT = 'ck_pack_cert_independent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
