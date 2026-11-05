-- Migration 000046 down: revert evidence PBC action registrations and templates.

DELETE FROM role_template_versions WHERE template_code = 'EVIDENCE_REQUEST_MANAGER' AND template_version = 1;
DELETE FROM role_template_versions WHERE template_code = 'OBLIGATION_MANAGER' AND template_version = 2;
DELETE FROM role_template_versions WHERE template_code = 'COMPLIANCE_CATALOGUE_ADMINISTRATOR' AND template_version = 2;
DELETE FROM role_templates WHERE template_code = 'EVIDENCE_REQUEST_MANAGER';

DELETE FROM permission_definitions WHERE action_name IN (
    'EVIDENCE_REQUIREMENT_CREATE',
    'EVIDENCE_REQUIREMENT_RETIRE',
    'EVIDENCE_REQUEST_CREATE',
    'EVIDENCE_REQUEST_MANAGE',
    'EVIDENCE_REQUEST_SUBMIT_RESPONSE',
    'EVIDENCE_REQUEST_EVALUATE'
);