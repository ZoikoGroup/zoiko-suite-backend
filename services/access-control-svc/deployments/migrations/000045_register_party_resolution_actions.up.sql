-- Migration 000045: register the MDM-05 party resolution actions (ZS-SVC-I-001 s4.7, ZS-SVC-U-001 s18-s19),
-- enforced by tenant-entity-registry-svc.
-- A merge decides which records are one party: the absorbed ids stop being live, identifiers and roles move and every
-- domain re-points its references. CRITICAL, and never by the principal who opened the case. A split corrects one and
-- a case decision settles a duplicate, a fact or an identifier dispute: HIGH, and never by the opener (a split never
-- by the merger). tenant-entity-registry-svc enforces the separation whoever holds the permission. Who holds these is
-- a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PARTY_MERGE',             'LEGACY', 'CRITICAL', 'Enforced by tenant-entity-registry-svc: merge duplicate parties into a survivor; never by the principal who opened the case'),
    ('PARTY_SPLIT',             'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: correct a merge by splitting a party out of it; never by the principal who merged'),
    ('PARTY_RESOLUTION_DECIDE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: decide a party stewardship case that is not a merge; never by the principal who opened it')
ON CONFLICT (action_name) DO NOTHING;
