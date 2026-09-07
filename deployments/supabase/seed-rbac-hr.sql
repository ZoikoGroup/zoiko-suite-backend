-- =============================================================================
-- RBAC seed for the HR domain  —  DATA, NOT A MIGRATION
-- =============================================================================
--
-- Companion to seed-rbac.sql, which grants the actions the login-path services
-- send. None of the HR domain's actions are in it, and authorization-svc fails
-- closed, so every gated request to employee-master, leave-absence, compensation
-- and payroll-run answers DENIED without the rows below.
--
-- Added as a SECOND permission bundle on the same role rather than by editing
-- the first. authorization-svc's evaluation query joins every active bundle for a
-- role and unions their actions:
--
--     JOIN permission_bundles pb ON pb.role_id = r.role_id AND pb.active_flag
--
-- so a second bundle is additive, and the servicectl bundle keeps its own
-- provenance — its comment explains precisely which service sends each of its
-- actions, and that stays true.
--
-- Reuses seed-rbac.sql's role, tenant and principal, so run that FIRST:
--   role      a0000000-0000-4000-8000-000000000001  SERVICECTL_TEST_ADMIN
--   tenant    11111111-1111-1111-1111-111111111111
--   principal 33333333-3333-3333-3333-333333333333  (assignment is tenant-wide)
--
-- ── Why app.tenant_id is set first ───────────────────────────────────────────
--
-- authorization_svc.roles has RLS enabled and FORCED, and 0028 gave
-- permission_bundles a policy too — scoped through the parent role. Both reads
-- of `roles` below therefore need a tenant installed, or the INSERT is refused
-- because the role it references resolves to nothing.
--
-- Setting it also means this file is correct when run as a NOBYPASSRLS role, not
-- only as the SQL editor's postgres (which carries BYPASSRLS and would ignore
-- every policy here).
--
-- set_config(..., false) rather than SET LOCAL, for the reason seed-rbac.sql
-- gives: the SQL editor may already be inside a transaction.
-- =============================================================================

SELECT set_config('app.tenant_id', '11111111-1111-1111-1111-111111111111', false);

-- ── The HR domain bundle ─────────────────────────────────────────────────────
--
-- Every action below is one the services actually send, collected from the
-- action constants in each service's handler rather than guessed:
--
--   employee-master-svc   internal/handler/handler.go
--   leave-absence-svc     internal/handler/handler.go, holidays.go
--   compensation-svc      internal/handler/handler.go, components.go
--   payroll-run-svc       internal/handler/handler.go
--
-- The VIEW actions are included because several read paths authorize too: a
-- listing scoped to a legal entity checks *_VIEW before it reads, and omitting
-- them would leave reads denied while writes worked, which is a confusing
-- half-grant.
--
-- This is a TEST grant: one role holding every action across four services is
-- the opposite of segregation of duties. It exists to prove the spine end to
-- end. Real roles should be narrower, and payroll in particular should not sit
-- in the same bundle as the compensation grants that feed it — PAYROLL_RUN_
-- FINALIZE and WAGE_REVISE in one role is exactly what an SoD rule is for.

INSERT INTO authorization_svc.permission_bundles
    (permission_bundle_id, role_id, bundle_code, permitted_actions, active_flag)
VALUES
    ('b0000000-0000-4000-8000-000000000002',
     'a0000000-0000-4000-8000-000000000001',
     'HR_DOMAIN_BUNDLE',
     '[
        "EMPLOYEE_CREATE",
        "EMPLOYEE_UPDATE",
        "EMPLOYEE_UPDATE_STATUS",
        "EMPLOYEE_VIEW",

        "LEAVE_TYPE_CREATE",
        "LEAVE_TYPE_VIEW",
        "LEAVE_BALANCE_UPDATE",
        "LEAVE_BALANCE_VIEW",
        "LEAVE_REQUEST_SUBMIT",
        "LEAVE_REQUEST_VIEW",
        "LEAVE_REQUEST_APPROVE",
        "LEAVE_REQUEST_REJECT",
        "HOLIDAY_CREATE",
        "HOLIDAY_VIEW",
        "HOLIDAY_DEACTIVATE",

        "COMPENSATION_CREATE",
        "COMPENSATION_VIEW",
        "WAGE_REVISE",
        "BONUS_GRANT",
        "BONUS_APPROVE",

        "PAYROLL_RUN_CREATE",
        "PAYROLL_RUN_VIEW",
        "PAYROLL_RUN_CALCULATE",
        "PAYROLL_RUN_FINALIZE"
      ]'::jsonb,
     TRUE)
ON CONFLICT (role_id, bundle_code) DO UPDATE
    SET permitted_actions = EXCLUDED.permitted_actions,
        active_flag       = TRUE;

-- =============================================================================
-- VERIFICATION — reproduces authorization-svc's own evaluation query
-- =============================================================================
-- Same joins and predicates as its PgStore.FindGrantedActions. If this returns
-- the HR actions the service will grant them; if it returns nothing, the seed did
-- not take and every gated HR request will answer DENIED.
--
-- app.platform_scope stands in for the escape hatch withPlatformScope sets on
-- this exact read.

SELECT set_config('app.platform_scope', 'true', false);

SELECT r.role_code,
       pb.bundle_code,
       jsonb_array_length(pb.permitted_actions) AS action_count
  FROM authorization_svc.principal_role_assignments pra
  JOIN authorization_svc.roles r
       ON r.role_id = pra.role_id AND r.active_flag
  JOIN authorization_svc.permission_bundles pb
       ON pb.role_id = r.role_id AND pb.active_flag
 WHERE pra.principal_id = '33333333-3333-3333-3333-333333333333'
   AND pra.effective_from <= NOW()
   AND (pra.effective_to IS NULL OR pra.effective_to > NOW())
 ORDER BY pb.bundle_code;

-- And the union the service actually evaluates against, so a missing action is
-- visible here rather than as a 403 from a service.
SELECT count(DISTINCT action) AS distinct_actions_granted
  FROM authorization_svc.principal_role_assignments pra
  JOIN authorization_svc.roles r
       ON r.role_id = pra.role_id AND r.active_flag
  JOIN authorization_svc.permission_bundles pb
       ON pb.role_id = r.role_id AND pb.active_flag
 CROSS JOIN LATERAL jsonb_array_elements_text(pb.permitted_actions) AS action
 WHERE pra.principal_id = '33333333-3333-3333-3333-333333333333'
   AND pra.effective_from <= NOW()
   AND (pra.effective_to IS NULL OR pra.effective_to > NOW());
