-- 000007 down: removes certification evidence.
DROP TRIGGER IF EXISTS trg_pack_certified_status_guard ON jurisdiction_pack_versions;
DROP TABLE IF EXISTS pack_certifications;
DROP TABLE IF EXISTS pack_reviews;
DROP TABLE IF EXISTS pack_test_runs;
DROP TABLE IF EXISTS pack_test_bundles;
DROP FUNCTION IF EXISTS jur_pack_certified_status_guard();
DROP FUNCTION IF EXISTS jur_certification_guard();
DROP FUNCTION IF EXISTS jur_review_guard();
DROP FUNCTION IF EXISTS jur_cert_evidence_guard();
DROP FUNCTION IF EXISTS jur_forbid_update();
