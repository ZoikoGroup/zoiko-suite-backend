-- Migration 000052 down: withdraw the templates this migration published and the
-- actions it registered. Refused while a bundle grants one of those actions.
--
-- role_template_versions is append-only (trigger from 000009). Drop the trigger
-- temporarily so this migration can withdraw its own published versions.

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('JURISDICTION_OUTBOX_EVENT_VIEW', 'JURISDICTION_OUTBOX_EVENT_REQUEUE', 'JURISDICTION_SUMMARY_VIEW',
                 'JURISDICTION_ENTITY_BACKFILL', 'JURISDICTION_OPERATION_VIEW', 'JURISDICTION_OPERATION_CANCEL');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000052 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DROP TRIGGER IF EXISTS role_template_versions_no_update ON role_template_versions;

DELETE FROM role_template_versions WHERE published_by = 'migration:000052';
DELETE FROM role_templates t
 WHERE t.template_code IN ('JURISDICTION_REGISTRY_STEWARD', 'JURISDICTION_REGISTRY_APPROVER', 'JURISDICTION_RULE_AUTHOR',
                           'JURISDICTION_RULE_REVIEWER', 'JURISDICTION_PACK_CERTIFIER', 'JURISDICTION_PACK_SIGNER',
                           'JURISDICTION_RELEASE_MANAGER', 'JURISDICTION_EXPERT_REVIEWER', 'JURISDICTION_RESOLVER_CALLER')
   AND NOT EXISTS (SELECT 1 FROM role_template_versions v WHERE v.template_code = t.template_code);

CREATE TRIGGER role_template_versions_no_update
    BEFORE UPDATE OR DELETE ON role_template_versions
    FOR EACH ROW EXECUTE FUNCTION role_template_versions_immutable();

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('JURISDICTION_OUTBOX_EVENT_VIEW', 'JURISDICTION_OUTBOX_EVENT_REQUEUE', 'JURISDICTION_SUMMARY_VIEW',
                 'JURISDICTION_ENTITY_BACKFILL', 'JURISDICTION_OPERATION_VIEW', 'JURISDICTION_OPERATION_CANCEL');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000052 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name IN ('JURISDICTION_OUTBOX_EVENT_VIEW', 'JURISDICTION_OUTBOX_EVENT_REQUEUE', 'JURISDICTION_SUMMARY_VIEW',
                       'JURISDICTION_ENTITY_BACKFILL', 'JURISDICTION_OPERATION_VIEW', 'JURISDICTION_OPERATION_CANCEL');
