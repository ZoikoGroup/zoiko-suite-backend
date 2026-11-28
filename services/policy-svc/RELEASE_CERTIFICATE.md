# policy-svc — Release Certificate

Certified against the evidence of the verification pass of 18 Nov 2026 only. Every figure
below was produced in that pass. What was not done is listed at the end.

| | |
|---|---|
| Service | policy-svc (Governance Platform), port 8085, mTLS peer listener 8449 |
| Specs | Doc 03 §8.1, GOV-05, ZS-SVC-V-001 PDC-05, ZS-GOV-POL-001 / ZS-GOV-INT-002, REF-07, ZS-API-001 |
| Image | `deployments-policy-svc:latest`, built 18 Nov 2026 09:44 IST from host binaries of branch `shashi-changes` (base `1d6debe6` plus this pass) |
| Migrations | 15 files (000001–000013; 000003 and 000009 each have two files, see SPEC_DEVIATIONS), all applied to the local `policy` database |
| Contract | openapi.yaml 1.0.0: 59 paths, 68 operations, 47 tagged `x-spec-operation` (kin-openapi valid, router parity test, named-operation test); asyncapi.yaml 2.0.0 |
| Identity | `POLICY_IDENTITY_VERIFY=enforce` (compose default since this pass, POL-3) |

## Tests

Host `go test`, store suite against Postgres 16 as a NOSUPERUSER NOBYPASSRLS role on a scratch
database, `REQUIRE_DB_TESTS=1`: **289 passing (265 top-level), 0 failing, 0 skipped**.

| Package | Pass | | Package | Pass |
|---|---|---|---|---|
| internal/handler | 144 | | internal/signatory | 11 |
| internal/store (real Postgres 16) | 58 | | internal/domain | 10 |
| internal/resolve | 15 | | internal/events | 5 |
| internal/consumer (new) | 14 | | internal/evidence | 4 |
| internal/authz | 13 | | internal/cache, internal/evaluator | 3 each |
| cmd/server, internal/decisionlog, internal/outbox, internal/policytype | 2 each | | internal/mtlslistener | 1 |

| Live run | Result |
|---|---|
| `scripts/audit.sh` (static, suites, identity, contract, negative paths, telemetry, database, E2E) | 48 passed, 0 failed |
| `deployments/scripts/e2e-policy-deps.py` under enforce (run twice) | 112 passed, 0 failed, 0 skipped |
| Out-of-domain caller probe (wire contract, see below) | 6 passed, 0 failed |
| Console UI E2E `tests/e2e/console/policies.spec.ts` (frontend repo; run twice) | 12 passed, 0 failed |

Mutation check: changing one GOV-05 route's `x-spec-operation` fails
`TestOpenAPI_NamedSpecOperationsAreTagged` by name.

## Controls proven live
- Identity (POL-3): bare identity headers → 401 `identity_assertion_required`; a tampered envelope →
  401; identity-context-svc's verified envelope → 200. governance-decision-log-svc's replay and
  workflow-svc's signatory reads cross the :8449 peer listener; a workload not in
  `MTLS_ALLOWED_CLIENTS` is refused.
- Callers under enforce: expense-claim-svc, payment-authorization-svc and payroll-run-svc requests,
  replayed with exactly the headers and body their client code builds, reach policy logic (200)
  when they forward the caller's envelope and are refused 401 when they do not. Each caller's
  unit tests (`TestEvaluate_RefusedCallerIsNotAnOutage` ×2, `TestApprovalPolicyFailsClosed`) prove
  it turns that 401 into a fail-closed refusal, not an outage.
- GOV-05 from the console, two people: the author's approval is refused (maker–checker) and
  recorded; a second person approves and activates; the suspender's review is refused; a second
  approver's review is recorded; versions compare; an attestation is signed and revoked.
- PDC-05: an APPROVAL_THRESHOLD version is certified from the console (two test cases, suite run)
  before it can be activated.
- If-Match: a lifecycle command with no version is 428, a stale one 412, a wildcard 400.
- Every table holding tenant data is under FORCE RLS; the runtime role `app_policy` is neither
  superuser nor BYPASSRLS; the event outbox is drained and no evaluation evidence is FAILED.

## Found and fixed in this pass
- The console could not bring any version into force: activation sent no `expected_version`
  (428) and the page had no submit-review / approve / certification path.
- Under enforce, the console's version drafts and its threshold read-back were refused 401: the
  API client rebuilt the identity from ids and dropped the signed envelope.
- The e2e harness itself sent bare identity headers; it now signs in as the demo principals.

## Not done in this pass
- The three out-of-domain callers were not run as binaries end to end: their own dependency
  chains (expense-claim: employee-master, document-vault, tax-determination, payable-open-item,
  configuration-feature-flag; payment-authorization: payee-banking-identity; payroll-run: the
  payroll chain) exceed this pass. Their policy-svc edge was proven on the wire contract above.
- Accounting-policy console (REF-07) — next task.
- EST-9 grants by role template; the PDC-05 actions are granted to the demo POLICY_FULL bundle by
  the e2e script (development only).
- Open items owned elsewhere: evaluate 404/409/501 → 200 (callers), currency on the float callers,
  P-07 (decision-log D-06), WF-6 (workflow-svc), ACTIVE tie-break sign-off (business owner),
  PDC-01 authority sources (roadmap), production signing keys and grants.
