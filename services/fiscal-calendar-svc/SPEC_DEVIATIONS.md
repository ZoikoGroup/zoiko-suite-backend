# SPEC_DEVIATIONS — fiscal-calendar-svc (REF-04)

Every place this implementation interprets, narrows, extends or departs from
the spec (REF-04, spec lines 794-836; shared contract 192-218; API patterns
1219-1248; negative path 29; DoD 1512-1531; anti-pattern "Fiscal calendar edits
old period boundaries") or from the approved design. Items are ordered roughly
by how much a reviewer should care.

## A. Not implemented / weaker than the spec

1. **The "never rewrite historical periods" check depends on REF-05, which does
   not exist yet in a form this service has been run against.** Activating a
   version that is not the first of its calendar calls
   `GET {ACCOUNTING_PERIOD_URL}/v1/calendar-usage?calendar_id=...` and expects
   `{"latest_period_end":"YYYY-MM-DD"|null,"has_posted_or_closed_periods":bool}`.
   That contract was specified by the brief; the client is tested only against an
   `httptest` stub. The default URL `http://accounting-period-svc:8174` is a
   guess at REF-05's port. Until REF-05 serves the endpoint, every non-first
   activation fails closed with 503 `DEPENDENCY_UNAVAILABLE` (intended: failing
   closed is the safe state, but it means the service cannot supersede a version
   in a real stack today).
2. **The history check is point-in-time, not transactional across services.**
   REF-05 could post a period between this service's check and the commit of
   the activation. REF-05 must independently refuse to post into a period whose
   calendar version is no longer the one in force, and must pin version ids on
   its periods; this service cannot close that window alone.
3. **The history check covers a calendar's own next version only.** It does not
   check a *different* calendar (new `code`) taking over an entity/scope from
   an older calendar. The overlap rule stops the two from overlapping in
   effective dates, but a hand-over mid-period between two calendars is not
   compared with the older calendar's posted history. Only
   `ProposeCalendarChange` on the same calendar is guarded. (The first version
   of a calendar is also not checked: it has no predecessor in its own calendar
   and the brief scoped the check to non-first versions.)
4. **`latest_period_end` semantics are assumed.** A change "touches history"
   when REF-05 reports posted/closed periods and the new version's
   `effective_from` is on or before `latest_period_end`. If REF-05 reports
   `has_posted_or_closed_periods=true` with no end date, the change is treated as
   touching history. A change effective inside a period that is open (unposted)
   but straddles the boundary is allowed without a plan; re-materialising that
   open period is REF-05's job.
5. **ORG-03 (entity) and REF-06 are not consulted.** `legal_entity_id` is taken
   from the trusted `X-Legal-Entity-Id` header and from the body (they must
   match). The service does not check that the entity exists, is active, or
   belongs to the tenant (tenant-entity-registry-svc would). Spec "Server-resolved
   context: jurisdiction defaults; existing periods/books; migration history;
   downstream close/report dependencies" is therefore only partly addressed
   (existing periods via REF-05; none of the rest).
6. **"Controller-level approval" is an authorization-svc action, not a verified
   role.** Approve and activate ask authorization-svc for
   `FISCAL_CALENDAR_APPROVE` / `FISCAL_CALENDAR_ACTIVATE`; whether that maps to
   "controller" is that service's catalogue. The actions are not seeded by this
   change; until seeded every command is denied (fail closed).
7. **Expected-version is not a "protected-field fingerprint".** Spec 3:
   "activation/approval binds protected-field fingerprint". Only the integer
   `version` is bound. Because pattern and effective dates are immutable from
   creation (see 17) a fingerprint would add little, but it is not implemented.
8. **No cancel/withdraw.** A DRAFT or APPROVED version that is never activated
   stays in that state forever (there is no REJECTED/CANCELLED version status),
   and an ACTIVE version with a future `effective_from` cannot be withdrawn: a
   correction must start after it ends or the interval overlap rule refuses it.
   Only transition *plans* have a REJECTED state.
9. **Superseding with a bounded version leaves a gap.** If the new version has an
   `effective_to`, the predecessor is still ended at the new version's
   `effective_from`; dates after `effective_to` then resolve to 404 until
   another version covers them. Nothing guesses a continuation.
10. **Valid time only.** `as_of` reconstructs by effective date. There is no
    recorded-time query ("what did the calendar say on the 3rd"). `recorded_at`
    is stored and the status history is append-only, but a superseded version's
    `effective_to` is shown as it is now, not as it was before supersession.
11. **No history table for the calendar header or plans.** The header keeps its
    latest row (version-bumped, never deleted, identity immutable); a plan keeps
    its latest row (content immutable, decision recorded once). Version
    transitions have an append-only history table.
12. **Narrow pattern vocabulary.** Two types (`CALENDAR_MONTHS`, `WEEK_PATTERN`
    summing to 52 weeks), special periods only `zero_length` and `AFTER_LAST`,
    `fiscal_year_start_day` 1-28, one 53rd-week rule pair (`ADD_TO_LAST`,
    `ADD_TO_FIRST`), week years that start on the first `week_start` weekday on
    or after the anchor date. Calendars defined as "the weekday nearest the end
    of month X" (a different 52/53 convention) are not expressible without a new
    pattern type (a code change; the data model would not change). 13-period
    calendars are expressible only as 13 periods of weeks (e.g. 13 x 4).
13. **Reads do not call authorization-svc and need only the tenant.** A caller
    in the tenant can read any entity's calendars; the entity is a filter, not a
    read permission boundary. (Matches the template; calendars are C1 reference
    data, not personal data.)
14. **Events**: `FiscalCalendarChangeProposed` is an addition to the three the
    spec lists. Approvals, plan creation and plan decisions emit no event (the
    spec lists none); they are evidenced in the tables and status history only.
15. **No `X-Expected-Version` on queries / no `If-Match`.** ETags are returned
    but conditional commands use `expected_version` only.
16. **`SOURCE_UNVERIFIED`, `REFERENCE_RETIRED` are never raised** (no REF-04 path
    produces them); `RULE_AMBIGUOUS` is raised only if the database ever held
    two in-force versions for one date, which the exclusion constraint prevents.
    All three are in the OpenAPI enum only because the shared contract lists them.

## B. Interpretations (choices where the spec is silent or ambiguous)

17. **A version's definition is immutable from creation, not only from APPROVED.**
    The brief said "never mutate once APPROVED". There is no edit command for a
    DRAFT either, and the trigger forbids changing pattern, fiscal-year start,
    `effective_from`, identity and proposer in every status. A mistake in a DRAFT
    is corrected by proposing a new version. `effective_to` is the one date that
    moves, exactly once, from NULL, when an ACTIVE version is superseded
    (end-dating).
18. **Error for a missing transition plan is a new code, `TRANSITION_PLAN_REQUIRED`
    (409).** The brief allowed INVALID_TRANSITION with a message or a new code.
    A distinct code lets REF-05 and the UI tell "needs a plan" from "illegal
    lifecycle move" without parsing text. It is an addition to the nine shared
    codes (as `NOT_FOUND` and `FORBIDDEN` already are in the sibling service).
    Callers that only know the shared nine will treat it as a 409.
19. **Overlap is `INVALID_TRANSITION`, not `DUPLICATE_CANDIDATE`.** Activating
    into an occupied interval is a refused lifecycle move.
20. **ACTIVE and SUPERSEDED both occupy their interval** (partial exclusion
    constraint on `daterange(effective_from, effective_to, '[)')` per tenant,
    entity and scope, across calendars). Resolution therefore also answers for
    dates governed by a superseded version. Parallel bases are expressed by
    different `scope` values (one calendar each); a different legal entity never
    conflicts.
21. **Predecessor supersession is automatic and narrow.** Activating version N
    ends and supersedes the same calendar's ACTIVE, open-ended version whose
    `effective_from` is earlier. Anything else overlapping is refused; there is
    no implicit trimming of a bounded version.
22. **Fiscal-year label.** `START_YEAR` (default): the calendar year of the
    anchor date. `END_YEAR`: the calendar year the fiscal year ends, i.e. anchor
    year minus 1 unless the anchor is 1 January. For week patterns the label
    follows the anchor date, not the actual (week-aligned) first day.
23. **Special periods are dated at the last day of the fiscal year**
    (`start_date = end_date`), which coincides with the last day of the final
    normal period. The "contiguous, non-overlapping, covering" invariant holds
    for NORMAL periods; special periods are zero-length overlays by definition.
    `period_no` numbers every period in order (a 13th special period is 13) and
    `period_key` is `FY<year>-P<nn>` or `FY<year>-<KEY>`.
24. **Preview is limited to APPROVED / ACTIVE / SUPERSEDED versions** (DRAFT is
    409 `INVALID_TRANSITION`), so a consumer cannot pin to a definition that may
    still be abandoned.
25. **Resolve with no match is 404 `NOT_FOUND`** (message names entity-scope and
    date); `as_of` before any version is also 404, never the nearest version.
    Draft and approved-but-not-active versions do not resolve.
26. **SoD is stricter than the spec's "affecting active books".** The proposer of
    any version cannot approve it (also a database CHECK); a plan's proposer and
    the proposer of the version it leads to cannot approve the plan (also a
    CHECK for the plan's own proposer). Activation has no SoD rule of its own.
27. **Idempotency / concurrency.** Replays return the original stored response
    (HTTP 200, `Idempotent-Replay: true`, even if the original was 201), without
    re-running any check, including the REF-05 call; the same key with a
    different request is 422 `CONTEXT_INVALID`. Failed commands store nothing.
    Activation takes a per-(tenant, entity, scope) advisory lock; the exclusion
    constraint is the backstop. Calendar `version` is bumped when a version is
    proposed and when one is activated (`expected_version` for propose-change is
    the calendar's); a version's `version` is bumped by approve, activate and
    supersede; creating a plan checks the to-version's `expected_version` but
    does not bump it.
28. **Header context.** Commands require `X-Legal-Entity-Id`, and it must equal the
    target object's entity (422 otherwise). The vendored envelope policy sets
    `LegalEntityID: Required` for all requests; in the default write-strict mode
    that is enforced on writes and only reported (header
    `X-Envelope-Contract: violated`) on reads that omit it.
29. **Event shape.** `payload.scope` is always `TENANT` (the envelope field kept
    for shape parity with the siblings); the calendar's own scope label is
    `calendar_scope` in the event data. `effective_at` is the version's
    `effective_from` as midnight UTC (the successor's start for
    `FiscalCalendarSuperseded`). Keyed by `calendar_id` for Created, `version_id`
    for the version events: no cross-object ordering.
30. **btree_gist.** The exclusion constraint needs the `btree_gist` extension. It
    is a trusted extension on PostgreSQL 13+, so the migration's `CREATE
    EXTENSION IF NOT EXISTS btree_gist` succeeded for a non-superuser
    database owner on postgres:16-alpine (verified). Where the migration role
    cannot create it, a superuser must run it once per database. The down
    migration deliberately does not drop it.
31. **`envelope/contract.go` is hand-written** (header says generated by
    `services/_contract/rollout.sh`; the script's service list was not extended,
    which is out of scope). The other vendored packages (envelope, telemetry,
    mtls, health, outbox) are copies of currency-registry-svc's with only the
    module path and service/metric names changed.
32. **Registered with the shared `zoiko_app` DB role** is the expectation, as for
    the sibling services; the parent owns docker-compose / init-db.sh. Port 8173.
33. **Readiness checks the database only.** authorization-svc and
    accounting-period-svc are not readiness dependencies: reads keep serving;
    commands fail closed on their own.
34. **Template Dockerfile bug not carried over.** The template's Dockerfile copies
    the binary to `/delegated-authority-svc` but runs `/currency-registry-svc`
    (an image built from it would not start). This service's Dockerfile copies to
    `/fiscal-calendar-svc`. The Dockerfile build itself has not been run.
35. **Telemetry domain metrics were replaced** (`fiscal_calendar_commands_total`,
    `_resolutions_total`, `_authz_decisions_total`, outbox gauges); the template's
    import/validation counters do not apply. The `Domain` struct field set
    therefore differs from the template's (relay code is unchanged).
