-- Migration 000049: register the identifier verification action (ZS-SVC-U-001 s14.1), enforced by
-- tenant-entity-registry-svc. Recording that an authority confirmed or denied an identifier decides whether other
-- domains may rely on it (a verified identifier is what lets a create link to an existing party automatically), so it is
-- HIGH. The service additionally never lets a lower authority overwrite a higher one's verification, and a higher
-- authority that disagrees opens a stewardship case instead of overwriting, whoever holds the permission. Who holds it is
-- a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PARTY_IDENTIFIER_VERIFY', 'LEGACY', 'HIGH', 'Enforced by tenant-entity-registry-svc: record an observation of an external identifier from an authority or provider and apply verification precedence')
ON CONFLICT (action_name) DO NOTHING;
