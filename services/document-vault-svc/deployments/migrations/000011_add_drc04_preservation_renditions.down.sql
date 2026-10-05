DROP TRIGGER IF EXISTS trg_reject_export_package_item_mutation ON export_package_items;
DROP FUNCTION IF EXISTS reject_export_package_item_mutation() CASCADE;
DROP TABLE IF EXISTS export_package_items;

DROP TRIGGER IF EXISTS trg_reject_export_package_mutation ON export_packages;
DROP FUNCTION IF EXISTS reject_export_package_mutation() CASCADE;
DROP TABLE IF EXISTS export_packages;

DROP TRIGGER IF EXISTS trg_reject_redaction_profile_mutation ON redaction_profiles;
DROP FUNCTION IF EXISTS reject_redaction_profile_mutation() CASCADE;
DROP TABLE IF EXISTS redaction_profiles;

DROP TRIGGER IF EXISTS trg_reject_fixity_manifest_mutation ON fixity_manifests;
DROP FUNCTION IF EXISTS reject_fixity_manifest_mutation() CASCADE;
DROP TABLE IF EXISTS fixity_manifests;

DROP TRIGGER IF EXISTS trg_reject_rendition_mutation ON renditions;
DROP FUNCTION IF EXISTS reject_rendition_mutation() CASCADE;
DROP TABLE IF EXISTS renditions;
