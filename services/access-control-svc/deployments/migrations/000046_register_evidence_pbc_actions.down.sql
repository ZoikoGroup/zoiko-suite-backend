-- Migration 000046 down: withdraw the templates this migration published and
-- restore the empty descriptions 000008 gave the six actions. The actions stay
-- registered (they are 000008's). A role already instantiated from one of these
-- template versions keeps its own copy of the actions.

DELETE FROM role_template_versions WHERE template_code = 'EVIDENCE_REQUEST_MANAGER' AND template_version = 1 AND published_by = 'migration:000046';
DELETE FROM role_template_versions WHERE template_code = 'OBLIGATION_MANAGER' AND template_version = 2 AND published_by = 'migration:000046';
DELETE FROM role_template_versions WHERE template_code = 'COMPLIANCE_CATALOGUE_ADMINISTRATOR' AND template_version = 2 AND published_by = 'migration:000046';
DELETE FROM role_templates WHERE template_code = 'EVIDENCE_REQUEST_MANAGER'
   AND NOT EXISTS (SELECT 1 FROM role_template_versions WHERE template_code = 'EVIDENCE_REQUEST_MANAGER');

UPDATE permission_definitions SET description = ''
 WHERE action_name IN ('EVIDENCE_REQUIREMENT_CREATE', 'EVIDENCE_REQUIREMENT_RETIRE', 'EVIDENCE_REQUEST_CREATE',
                       'EVIDENCE_REQUEST_MANAGE', 'EVIDENCE_REQUEST_SUBMIT_RESPONSE', 'EVIDENCE_REQUEST_EVALUATE')
   AND description LIKE 'Enforced by evidence-requirements-svc:%';
