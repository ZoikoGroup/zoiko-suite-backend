-- Migration 000052: jurisdiction-rules-svc actions carried by role templates (rule R5;
-- tracker D2-1 JR-1, 12 Nov 2026).
--
-- jurisdiction-rules-svc checks 89 actions. Every one is registered (000008, 000023,
-- 000025, 000027, 000037, 000038, 000041), but only two were carried by a template
-- (JURISDICTION_COMPLIANCE_OFFICER, 000023), so the governed way to hold the other 87
-- was the dev seed. This migration registers the six actions the D2-1 completion adds
-- and publishes templates that carry every action the service checks.
--
-- The split follows ZS-JUR-001 s28 ("pack authoring, certification, signing and
-- promotion are separate privileged capabilities") and the maker-checker pairs the
-- service enforces per record (the author of a rule, source, interpretation, calendar
-- version, payment term or import never approves it):
--
--   JURISDICTION_REGISTRY_STEWARD   REF-01 reference-data steward: add, retire, aliases, propose imports
--   JURISDICTION_REGISTRY_APPROVER  the checker of externally sourced imports
--   JURISDICTION_RULE_AUTHOR        rules, sources, interpretations, packs, tests, calendars, terms (authoring)
--   JURISDICTION_RULE_REVIEWER      activation, source review, interpretation approval, publication (checking)
--   JURISDICTION_PACK_CERTIFIER     compile, run the test bundle, certify, verify
--   JURISDICTION_PACK_SIGNER        sign artifacts and keep the signing keys
--   JURISDICTION_RELEASE_MANAGER    publish, deploy, rollback, withdraw, block, hotfix, rollouts, remediation, outbox
--   JURISDICTION_EXPERT_REVIEWER    the qualified local expert decision on a rollout
--   JURISDICTION_RESOLVER_CALLER    workloads that resolve rules and record submissions
--
-- Templates are LEGAL_ENTITY scoped: the service authorizes against the synthetic
-- platform-scope entity (AUTHZ_PLATFORM_SCOPE_ID).

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_OUTBOX_EVENT_VIEW',    'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: list outbox events parked after repeated publish failures (J-16)'),
    ('JURISDICTION_OUTBOX_EVENT_REQUEUE', 'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: requeue a parked outbox event for publication, with a reason and evidence (J-16)'),
    ('JURISDICTION_SUMMARY_VIEW',         'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: read the jurisdiction and rule-pack console summary (counts only)'),
    ('JURISDICTION_ENTITY_BACKFILL',      'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: rebuild entity-jurisdiction applicability from tenant-entity-registry-svc (J-10)'),
    ('JURISDICTION_OPERATION_VIEW',       'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: read a long-running pack compile or test-run operation (API-5)'),
    ('JURISDICTION_OPERATION_CANCEL',     'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: cancel a long-running pack compile or test-run operation before it completes (API-5)')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('JURISDICTION_REGISTRY_STEWARD',  'Jurisdiction Registry Steward',  'Maintain the jurisdiction registry: add and retire jurisdictions, amend aliases, propose source imports.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_REGISTRY_APPROVER', 'Jurisdiction Registry Approver', 'Approve or reject externally sourced jurisdiction imports proposed by a steward.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_RULE_AUTHOR',       'Jurisdiction Rule Author',       'Author rules, sources, interpretations, pack versions, test bundles, calendars and payment terms.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_RULE_REVIEWER',     'Jurisdiction Rule Reviewer',     'Independently review and approve what an author drafted: activate rules, review sources, approve interpretations and terms, publish calendars.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_PACK_CERTIFIER',    'Jurisdiction Pack Certifier',    'Compile pack versions, run their test bundles, certify and verify them.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_PACK_SIGNER',       'Jurisdiction Pack Signer',       'Sign certified pack artifacts and keep the pack signing keys.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_RELEASE_MANAGER',   'Jurisdiction Release Manager',   'Release, deploy, roll back, withdraw and block pack versions; run rollouts, hotfixes, remediation and the event outbox.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_EXPERT_REVIEWER',   'Jurisdiction Expert Reviewer',   'Record the qualified local expert decision a jurisdiction rollout needs before launch.', 'LEGAL_ENTITY', FALSE),
    ('JURISDICTION_RESOLVER_CALLER',   'Jurisdiction Resolver Caller',   'Resolve rules, determinations and calculations, and record regulatory submissions (workloads).', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('JURISDICTION_REGISTRY_STEWARD', 1, ARRAY[
        'JURISDICTION_CREATE','JURISDICTION_DEACTIVATE','JURISDICTION_ALIAS_AMEND','JURISDICTION_ALIAS_END',
        'JURISDICTION_IMPORT_PROPOSE','JURISDICTION_IMPORT_VIEW','JURISDICTION_ENTITY_BACKFILL','JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_REGISTRY_APPROVER', 1, ARRAY[
        'JURISDICTION_IMPORT_APPROVE','JURISDICTION_IMPORT_VIEW','JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_RULE_AUTHOR', 1, ARRAY[
        'JURISDICTION_RULE_CREATE','JURISDICTION_RULE_RECORD_DRIFT','JURISDICTION_RULE_SET_PROVENANCE','JURISDICTION_RULE_SET_PARAMETERS',
        'REGULATORY_REGIME_CREATE','REGULATORY_SOURCE_CREATE','REGULATORY_SOURCE_SUPERSEDE','INTERPRETATION_RECORD_CREATE',
        'JURISDICTION_PACK_CREATE','JURISDICTION_PACK_VERSION_CREATE','JURISDICTION_PACK_VERSION_SUBMIT','JURISDICTION_PACK_VERSION_AUTHOR_TESTS',
        'OBLIGATION_RULE_CREATE','OBLIGATION_RULE_SET_PROVENANCE','REGULATORY_CALENDAR_CREATE','REGULATORY_CALENDAR_VERSION_CREATE',
        'REGULATORY_CALENDAR_VERSION_SET_SOURCES','PAYMENT_TERM_CREATE','SOURCE_CHANGE_NOTICE_CREATE','JURISDICTION_RULE_VALIDATE',
        'JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_RULE_REVIEWER', 1, ARRAY[
        'JURISDICTION_RULE_TRANSITION','REGULATORY_SOURCE_REVIEW','INTERPRETATION_RECORD_APPROVE','JURISDICTION_PACK_VERSION_REVIEW',
        'OBLIGATION_RULE_PUBLISH','REGULATORY_CALENDAR_VERSION_PUBLISH','PAYMENT_TERM_APPROVE','PAYMENT_TERM_SUPERSEDE','PAYMENT_TERM_RETIRE',
        'SOURCE_CHANGE_NOTICE_REVIEW','SOURCE_CHANGE_NOTICE_CLOSE','JURISDICTION_RULE_VALIDATE','JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_PACK_CERTIFIER', 1, ARRAY[
        'JURISDICTION_PACK_VERSION_COMPILE','JURISDICTION_PACK_VERSION_EXECUTE_TESTS','JURISDICTION_PACK_VERSION_CERTIFY',
        'JURISDICTION_PACK_VERSION_VERIFY','JURISDICTION_OPERATION_VIEW','JURISDICTION_OPERATION_CANCEL','JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_PACK_SIGNER', 1, ARRAY[
        'JURISDICTION_PACK_VERSION_SIGN','JURISDICTION_PACK_VERSION_VERIFY','PACK_SIGNING_KEY_REGISTER','PACK_SIGNING_KEY_RETIRE',
        'PACK_SIGNING_KEY_REVOKE'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_RELEASE_MANAGER', 1, ARRAY[
        'JURISDICTION_PACK_VERSION_PUBLISH','JURISDICTION_PACK_VERSION_DEPLOY','JURISDICTION_PACK_VERSION_ROLLBACK',
        'JURISDICTION_PACK_VERSION_SUPERSEDE','JURISDICTION_PACK_VERSION_WITHDRAW','JURISDICTION_PACK_VERSION_BLOCK',
        'JURISDICTION_PACK_VERSION_UNBLOCK','JURISDICTION_PACK_VERSION_DECLARE_HOTFIX','JURISDICTION_PACK_VERSION_COMPLETE_RETROSPECTIVE',
        'DEPLOYMENT_RING_CREATE','DEPLOYMENT_REGION_CREATE','RESOLVER_CACHE_INVALIDATE','PACK_OPS_METRICS_VIEW',
        'REMEDIATION_CASE_OPEN','REMEDIATION_CASE_VIEW','REMEDIATION_CASE_DISPOSE','REMEDIATION_CASE_CLOSE',
        'JURISDICTION_ROLLOUT_CREATE','JURISDICTION_ROLLOUT_VIEW','JURISDICTION_ROLLOUT_SET_OWNER','JURISDICTION_ROLLOUT_LINK_PACKS',
        'JURISDICTION_ROLLOUT_ATTEST','JURISDICTION_ROLLOUT_TRANSITION','JURISDICTION_ROLLOUT_SEED','JURISDICTION_SUPPORT_VIEW',
        'JURISDICTION_OUTBOX_EVENT_VIEW','JURISDICTION_OUTBOX_EVENT_REQUEUE','JURISDICTION_SUMMARY_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_EXPERT_REVIEWER', 1, ARRAY[
        'JURISDICTION_ROLLOUT_EXPERT_APPROVE','JURISDICTION_ROLLOUT_VIEW','JURISDICTION_SUPPORT_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052'),
    ('JURISDICTION_RESOLVER_CALLER', 1, ARRAY[
        'RULE_RESOLUTION_RESOLVE','RULE_DECISION_VIEW','JURISDICTION_DECISION_RESOLVE','OBLIGATION_CALCULATION_CALCULATE',
        'LICENCE_EVALUATE','REGISTRATION_IDENTIFIER_VALIDATE','REGISTRATION_OBLIGATION_RESOLVE','SCHEME_ELECTION_EVALUATE',
        'THRESHOLD_MONITOR_EVALUATE','REGULATORY_CALCULATION_EXECUTE','PAYROLL_PARAMETERS_RESOLVE','RETENTION_RULE_RESOLVE',
        'ACCOUNTING_MAPPING_RESOLVE','SUBMISSION_PROFILE_RESOLVE','REGULATORY_SUBMISSION_CREATE','REGULATORY_SUBMISSION_RECORD_EVENT',
        'REGULATORY_SUBMISSION_VIEW','JURISDICTION_RULE_VALIDATE','JURISDICTION_SUPPORT_VIEW'],
     'Initial template (R5, D2-1)', 'migration:000052')
ON CONFLICT (template_code, template_version) DO NOTHING;

DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO missing
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE v.published_by = 'migration:000052'
       AND NOT EXISTS (SELECT 1 FROM permission_definitions p WHERE p.action_name = a AND p.active_flag);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'role template actions not in permission_definitions: %', missing;
    END IF;
END
$$;
