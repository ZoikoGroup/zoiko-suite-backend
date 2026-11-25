-- Migration 000061: register the controlled-import actions enforced by data-ingestion-svc (INT-06). None was
-- registered, so every import call was permanently denied by a real authorization service. Working import
-- items, deciding a release from quarantine, approving and executing an import are separate actions. Who
-- holds which is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DATA_IMPORT_PROFILE_PUBLISH',    'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: publish an import profile'),
    ('DATA_IMPORT_PROFILE_CERTIFY',    'LEGACY', 'HIGH',     'Enforced by data-ingestion-svc: certify an import profile (never its author)'),
    ('DATA_IMPORT_CREATE',             'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: create an import batch'),
    ('DATA_IMPORT_UPLOAD',             'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: upload and seal import chunks'),
    ('DATA_IMPORT_VALIDATE',           'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: validate and preview an import'),
    ('DATA_IMPORT_APPROVE',            'LEGACY', 'HIGH',     'Enforced by data-ingestion-svc: approve an import (never its uploader)'),
    ('DATA_IMPORT_EXECUTE',            'LEGACY', 'HIGH',     'Enforced by data-ingestion-svc: execute an approved import'),
    ('DATA_IMPORT_CANCEL',             'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: cancel an import'),
    ('DATA_IMPORT_ITEM_MANAGE',        'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: quarantine, retry or request release of an import item'),
    ('DATA_IMPORT_QUARANTINE_RELEASE', 'LEGACY', 'HIGH',     'Enforced by data-ingestion-svc: decide a release from quarantine (never the requester or the quarantiner)'),
    ('DATA_IMPORT_RECONCILE',          'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: reconcile an import against the owning domain'),
    ('DATA_IMPORT_READ',               'LEGACY', 'STANDARD', 'Enforced by data-ingestion-svc: read imports and their quarantine ledger')
ON CONFLICT (action_name) DO NOTHING;
