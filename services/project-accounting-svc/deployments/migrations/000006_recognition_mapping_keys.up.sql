-- ACC-02 governance: PRJ-03 posts recognition through mapping keys, never
-- caller-supplied account codes. A run snapshots the keys it was created
-- with so a later deployment-config change cannot alter an approved run's
-- own posting rule. Legacy account-code columns remain for historical runs.
ALTER TABLE project_recognition_runs
    ADD COLUMN revenue_mapping_key VARCHAR(128),
    ADD COLUMN wip_mapping_key     VARCHAR(128);
