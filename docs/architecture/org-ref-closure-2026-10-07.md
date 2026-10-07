# ORG / REF baseline (ZS-SVC-I-001) — closure note, 2026-10-07

Status: **work on this document is stopped, not completed.** It was picked up by
mistake; the next document takes over. This note records exactly what exists so
nobody mistakes the ORG/REF baseline for finished. It supersedes the status
columns of `org-ref-wave0-audit-2026-10-07.md` (the audit's findings stand).

## What was done

| Area | Service | Result |
|---|---|---|
| ORG-10 security | `payee-banking-identity-svc` | Events no longer carry the full account number or payee/institution names (safe projection, tenant on every event). Committed. |
| ORG-10 consumer | `payment-authorization-svc` | Request-time ORG-10 failure returns 503 and persists nothing; unpinned authorizations are re-checked at approve/consume. Committed. |
| REF-02 | `currency-registry-svc` (new) | Built, tested, run on PostgreSQL 16. NOT CERTIFIED. |
| REF-04 | `fiscal-calendar-svc` (new) | Built, tested, run on PostgreSQL 16. NOT CERTIFIED. |
| REF-05 | `accounting-period-svc` (new) | Built, tested, run on PostgreSQL 16. NOT CERTIFIED. Posting gate answers "not allowed" for a missing period. |
| REF-05 cutover, phase 1 | `financial-close-svc` | Provenance endpoint + optional mirror. Off by default. |
| REF-05 cutover, phase 2 | `general-ledger-svc` | Shadow gate (compare only). Off by default; `enforce` rejected at startup. |
| Tooling | `tools/period-backfill`, demo authz seed, compose flags | Committed. |

The new services were chosen as separate deployments. The repo's own convention
maps several doc IDs onto one existing service (e.g. ORG-02+03 in
`tenant-entity-registry-svc`); that choice was not explicitly offered when made
and was left as built rather than reworked.

## What was NOT done

- **Not built:** ORG-04, ORG-08, ORG-09, REF-03, REF-06, REF-07, REF-08, REF-09, REF-10.
- **Existing services audited only, not uplifted:** ORG-01 (~8%), ORG-05 (~3–17%),
  ORG-06 (~62–69%), ORG-07 (~10%), REF-01 (~12%), ORG-10 remainder (~54%: no
  outbox, no `expected_version`/idempotency, account stored in plaintext, routes
  not `/v1/payee-destinations/{id}:verb`).
- **REF-05 cutover phases 3–5 not started:** soak, enforcement flip (needs
  separate sign-off), and removal of the legacy period state. Until the flip,
  `financial-close-svc` and `general-ledger-svc` still treat an unregistered
  period as OPEN (the original fail-open is NOT yet closed in production paths).

## Verified vs unverified

Verified (ran): build/vet/tests for every touched service; the new services'
Postgres suites and down migrations on PostgreSQL 16 as a non-superuser role;
the real `init-db.sh` applying all 429 migrations; the authz seed against a real
`authorization-svc`; the backfill tool end to end against real
`fiscal-calendar-svc`, `accounting-period-svc` and `financial-close-svc`
(calendar create/approve/activate with separate principals, 12 periods
materialised, idempotent re-run, REF-05 rejects an uncovered date). That run
found and fixed two defects in the backfill tool.

**Never verified end to end:** locking a period with the mirror on (the
`financial-close-svc` -> REF-05 callback and the soft-close/hard-close SoD
handling), and the GL shadow comparison under real posting. These were only
tested against stand-ins. Do not rely on them without running them.

## Known risks to carry forward

1. Strict envelope mode (`ZS_ENVELOPE_ENFORCEMENT=strict`) would refuse REF-05's
   verification callback; the envelope file is generated.
2. Mirrored soft-close is attributed to the workload, hard-close to the human,
   so one click yields both steps (SoD is mechanical).
3. REF-05 soft-close exception is asserted by the caller and unverified.
4. `CreateReversalPosting` in `general-ledger-svc` stores reversals with no
   posting date (existing defect, not caused by this work).
5. Authz actions are seeded in the demo seed only; no production mechanism was found.
6. Events published by `payee-banking-identity-svc` before the ORG-10 fix carried
   full account numbers; whoever read that topic may hold them.
7. `tools/schemacheck/schemacheck.exe` is a tracked binary (pre-existing).

## Where the artefacts are

Service READMEs / `SPEC_DEVIATIONS.md` / `RELEASE_CERTIFICATE.md` in each new
service; `SPEC_DEVIATIONS_REF05.md` in `financial-close-svc`;
`tools/period-backfill/README.md`; `org-ref-wave0-audit-2026-10-07.md`.
