-- Migration 000055: register the ORG-9 master import actions (ZS-SVC-U-001 s19), enforced by tenant-entity-registry-svc.
-- Uploading a file and cancelling an operation before its point of no return are steward work (STANDARD). Approving an import
-- starts creating master records in bulk, so it is HIGH, and tenant-entity-registry-svc additionally refuses an approval by the
-- principal who uploaded the file, and any approval of a file with an invalid row, whoever holds the permission. Importing creates
-- parties through the ordinary party creation path as the uploader, so that principal also needs PARTY_CREATE. Who holds these
-- is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('MASTER_IMPORT_UPLOAD',  'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: upload a master import file for validation and preview'),
    ('MASTER_IMPORT_APPROVE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: approve a validated master import, starting it; never by the principal who uploaded it'),
    ('MASTER_IMPORT_CANCEL',  'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: cancel a master import operation before its point of no return')
ON CONFLICT (action_name) DO NOTHING;
