# SPEC_DEVIATIONS — accounting-period-svc (REF-05)

Every place this implementation interprets, narrows, extends or departs from
the spec (REF-05, spec lines 837-880; shared contract 192-218; cross-domain
control chain 1105-1145; API patterns 1219-1248; negative paths 30-31; DoD
1512-1531) or from the approved design. Ordered roughly by how much a reviewer
should care.

## A. Not implemented / weaker than the spec

1. **The ACC-14 verification endpoint does not exist yet.** The service verifies
   provenance by calling `GET {FINANCIAL_CLOSE_URL}/v1/close/workflow-refs/{ref}`
   on financial-close-svc. That endpoint is built in the later GL / close
   cutover step. Until then financial-close-svc answers 404, every state command
   is rejected with `SOURCE_UNVERIFIED` (fail closed), and **no period can leave
   OPEN in a real deployment**. That is deliberate, but it means this service
   cannot be run end to end today. The wire contract it expects is in
   `internal/clients/provenance.go` and the RUNBOOK; it was verified only
   against `httptest` servers written from the same contract, never against a
   real ACC-14 implementation.
2. **Soft-close exception is asserted, not verified.** For `SOFT_CLOSED`
   periods the gate allows posting when the caller sends
   `soft_close_exception=true`. The service has no way to check that the caller
   is entitled to that; it trusts the calling service (the GL). Anyone who can
   reach the gate can set the flag. A real control needs the exception to be
   backed by an authorization-svc decision or an ACC-14 artefact the gate can
   verify. Hard-closed and reclosed periods ignore the flag.
3. **"Posted" in `calendar-usage` is approximated by "not OPEN".** This service
   cannot see postings. `has_posted_or_closed_periods` is true when any
   materialised period of the calendar (version) is in a state other than OPEN.
   An OPEN period that already holds postings is reported as unused. REF-04
   therefore cannot rely on this alone to block a calendar change after postings
   exist in an open period; the GL would have to report usage too.
4. **Period commands do not use a protected-field fingerprint bound at
   approval time.** `decision_fingerprint` is computed here (sha256 over period,
   command, from-state, workflow ref, snapshot ref and reopen window) and
   enforced unique per period; it is not compared with a fingerprint issued by
   ACC-14. The provenance check compares workflow ref, period key, legal entity,
   command and control snapshot ref instead.
5. **Control event is best effort.** `PeriodCommandRejected` is written in its
   own transaction after the refusal (the command's transaction rolls back).
   If that write fails, the command is still refused and the failure is only
   logged. It is emitted only when the period exists, and only for missing /
   unverified evidence, ACC-14 unavailable, and SoD refusals; not for authz
   denials (authorization-svc has its own decision log), version conflicts or
   illegal transitions. Not exactly-once: each refused attempt emits one.
6. **No sweeper for expired reopen windows (by design).** An expired
   `REOPEN_AUTHORIZED` is judged at read time (gate and compat read); the stored
   state stays `REOPEN_AUTHORIZED` until someone runs `reclose`. No event marks
   the expiry, so a consumer that only listens to events will not learn that the
   window ended.
7. **No REF-06 / book validation.** `book_scope` and `module_scope` are opaque
   strings; there is no check that a book exists or belongs to the entity (spec
   cross-domain chain "Legal Entity -> Accounting Book"). `legal_entity_id` is
   not checked against org-structure either; materialisation trusts the
   calendar service's answer for the entity.
8. **No bitemporal / as-of query.** `GET /v1/accounting-periods/{id}` returns
   the current state. State history is stored and readable, but there is no
   `?as_of=` / `?recorded_at=` reconstruction.
9. **Postgres behaviour** was verified on PostgreSQL 16 for the paths the 6 store
   tests exercise (see RELEASE_CERTIFICATE.md); no load, failure-injection or
   multi-replica test was run. `-race` was not run (no cgo on the build machine).
10. **`DUPLICATE_CANDIDATE` and `REFERENCE_RETIRED` are never raised** (no REF-05
    path produces them); they exist in the code vocabulary and OpenAPI enum only
    because the shared contract lists them.
11. **No mTLS to fiscal-calendar-svc / financial-close-svc.** The template wires
    mTLS only for authorization-svc (behind `AUTHZ_MTLS_ENABLED`); the two new
    upstream calls are plain HTTP with `X-Tenant-Id` / `X-Workload-Id` headers.
    The mTLS rollout for these paths is not done.
12. **Not a readiness dependency: ACC-14, fiscal-calendar-svc, authorization-svc.**
    `/readyz` checks Postgres only. The gate (the C0/C1 path) calls none of them.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

13. **Soft-close also requires ACC-14 evidence.** The spec says "close/reopen"
    commands need ACC-14 provenance; `RequestSoftClose` is named a *request*. All
    four state commands (`request-soft-close`, `hard-close`, `authorize-reopen`,
    `reclose`) require `acc14_workflow_ref` and `control_snapshot_ref`. This is
    stricter than the minimum and is the reason nothing moves before the
    cutover endpoint exists. Dropping the requirement for soft-close is a
    one-line change in `internal/service/commands.go`.
14. **Missing refs = `CONTEXT_INVALID`; refs that fail verification =
    `SOURCE_UNVERIFIED`; ACC-14 unreachable / 5xx / 429 = `DEPENDENCY_UNAVAILABLE`.**
    The handler deliberately does not pre-validate the refs so the service can
    record the control event.
15. **Check order matters** and is documented in `Service.Command`: refs
    present, reopen window, `expected_version`, lifecycle, ACC-14 verification
    (outside any row lock), then the write transaction re-checks version and
    lifecycle under `FOR UPDATE`, then SoD and workflow-decision uniqueness.
16. **SoD rule.** `hard-close` and `authorize-reopen` must be requested by an
    actor other than the actor of the **latest** `SOFT_CLOSE` history entry.
    `reclose` has no SoD rule (spec names hard close and reopen). Only the
    soft-close requester is compared; nothing stops the same person hard-closing
    and later reclosing.
17. **A workflow decision applies to a period once.** Unique index on
    `(period_id, decision_fingerprint)`; the service refuses a repeat with
    `CONTEXT_INVALID`. The from-state is part of the fingerprint, so the same
    refs may legitimately be reused across different states.
18. **Reopen window.** `expires_at` is mandatory, must be in the future and at
    most `REOPEN_MAX_WINDOW` (default 72h, inclusive) ahead. The reopen scope
    may only narrow the period's: a period with a non-empty book/module scope
    must be reopened with exactly that scope; an entity-wide period may be
    reopened for any book/module or for all (`""`). `reclose` clears the window
    on the period row; the history row that opened it keeps it.
19. **Gate scope matching.** A period with an empty `book_scope` / `module_scope`
    covers every request; a scoped period covers only a request presenting that
    exact value. A request that omits `book_scope` therefore never matches a
    book-specific period. For `REOPEN_AUTHORIZED` the request's scope must be
    covered by the reopen scope (empty reopen scope = all); a request presenting
    no scope against a scoped reopen is out of scope.
20. **Gate precedence.** Several matching periods with the same (state, outcome)
    resolve to the most specific scope, then NORMAL before SPECIAL, then lowest
    `period_id`. Different outcomes are `RULE_AMBIGUOUS`: there is deliberately
    no precedence such as "book-specific beats entity-wide", because that would
    let an OPEN book period override a HARD_CLOSED entity period. A `kind` query
    parameter narrows NORMAL/SPECIAL overlaps (a SPECIAL period may overlap the
    last NORMAL one). A second calendar version materialised over a closed
    period produces `RULE_AMBIGUOUS` on the overlapping dates; that fails closed
    and needs an operator decision. REF-04 is expected to prevent such a change
    through `calendar-usage`.
21. **Gate response shape.** The response carries both `posting_allowed` (bool)
    and `posting_mode` (`ALLOWED | RESTRICTED | BLOCKED`); `RESTRICTED` is the
    "restricted posting" model for soft-close and reopen. `version` is the
    period's row version; `state_version` equals it today (only state changes
    bump the version). `purpose=read` is accepted and answered identically.
22. **`PERIOD_NOT_FOUND` is a new error code** (404) beyond the shared set. It
    also covers `status-by-key` for an unknown key. `NOT_FOUND` (404) is for an
    unknown period id; `FORBIDDEN` (403) for authorization-svc denial.
23. **Compat read, `status-by-key`.** Maps OPEN / in-window REOPEN_AUTHORIZED ->
    `OPEN`, SOFT_CLOSED / RECLOSED / expired reopen -> `CLOSED`, HARD_CLOSED ->
    `LOCKED`. When several periods share the key (different book scopes or
    calendar versions) it returns the **most restrictive** status, because the
    legacy shape has no book. It accepts `period_name` as an alias. The legacy
    contract's `period_name` is free text in financial-close-svc; this service's
    `period_key` is the REF-04 key (e.g. `FY2026-P03`). Whether they are the same
    strings in practice is unverified (open question for the cutover).
24. **Materialisation.** Needs a pinned `calendar_version_id` or resolves one via
    REF-04 at `resolve_date` (default `YYYY-01-01` of the fiscal year, a guess
    that is checked: the resolved `calendar_id` and the preview's `fiscal_year`
    must match the request). Validation: non-empty, at most 60 periods, unique
    keys and numbers, valid dates, NORMAL periods contiguous (SPECIAL periods
    may overlap). Existing periods are never changed; drift is reported in
    `boundary_drift`. `module_scope` is always empty at materialisation. A
    materialisation emits one `PeriodOpened` per created period (cap 60 makes
    that always possible in one transaction). `expected_version` does not apply
    to materialise (no object yet).
25. **Extra authz action.** `PERIOD_MATERIALIZE` (not in the spec) guards
    materialisation; `PERIOD_STATE_COMMAND` guards the four state commands. Both
    must be seeded in authorization-svc; until they are, all commands are denied.
    They are checked against the period's `legal_entity_id` (the body's for
    materialise). Reads are not authorized (service-to-service, tenant scoped).
26. **Envelope policy.** `LegalEntityID` is `NotRequired` in the vendored
    envelope policy: the entity travels in the body / query / the period, not as
    `X-Legal-Entity-Id`. `BookID` is `NotRequired` (REF-06 does not exist). The
    contract file was written by hand, not by `_contract/rollout.sh`.
27. **Dates** are `YYYY-MM-DD` strings in Go and `DATE` in Postgres; instants are
    UTC. No floating point (guarded by `doctrine_test.go`).
28. **History records the materialisation** as a `MATERIALIZE` entry
    (`from_state` empty, `to_state` OPEN) so the history is complete from birth;
    transition entries must carry both ACC-14 refs and a fingerprint (CHECK).
29. **Database guards are stricter than the service.** A trigger refuses any
    state change that is not one lifecycle step with `version + 1`, in addition
    to the immutability of identity/scope/boundaries and no DELETE/TRUNCATE. A
    manual fix-up script therefore cannot reopen a period either; it would have
    to go through the same transitions.
30. **No git commits, compose entry or init-db registration were made** (out of
    scope for this build; the parent registers deployment). The Dockerfile and
    migrations were not built/run under docker compose.
