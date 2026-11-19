-- Migration 000061 down: remove identity-context-svc provisioning operator grant
DROP TRIGGER IF EXISTS roles_identity_provisioning_operator ON roles;
DROP FUNCTION IF EXISTS acs_roles_grant_identity_provisioning_operator();
DROP FUNCTION IF EXISTS acs_grant_identity_provisioning_operator(UUID);
DROP FUNCTION IF EXISTS acs_provision_identity_provisioning_role(UUID, TEXT);