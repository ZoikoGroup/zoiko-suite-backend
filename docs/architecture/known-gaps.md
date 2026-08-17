# Known Architecture & Test Coverage Gaps

## Resolved: tenant-entity-registry-svc trusted an unsigned JWT for tenant isolation

Three defects that compounded, all closed 2026-08-05 and verified against
running containers (16/16 live assertions).

1. `HTTPAuthZClient.Authorize` was a TODO that logged a warning and returned
   nil, so no mutation was ever authorized. `docker-compose.yml` set
   `AUTHZ_SERVICE_URL` to this service's own address ("mock stub authz points
   back"), and `main.go` treated any URL other than the literal
   `http://authorization-svc` as production wiring — so that value selected
   the no-op client while logging "using HTTP authorization client".

2. `middleware.TenantContext` and `registry.actorFromJWT` both base64-decoded
   the JWT payload **without verifying the signature**, on the stated
   assumption that the Authorization Service had already validated it. Defect
   1 meant nothing had. `tenant_id` from that unsigned payload is what
   `PgStore.withRLS` sets as `app.tenant_id`, so row-level security — this
   service's central guarantee — was being steered by a value any caller could
   forge with base64 and no key. `compose` publishes 8081 on the host, so the
   gateway that does the verifying is bypassable.

3. Row-level security does not run at all. Confirmed against the live
   database: the service connects as the Postgres superuser
   (`pg_user.usesuper = t`), which bypasses RLS unconditionally, and the
   tables are owned by that same user with ENABLE rather than FORCE row level
   security (`pg_class.relforcerowsecurity = f`), so the owner bypasses the
   policies too. The policies are present and correctly written; they simply
   never execute. Verified exploitable before the fix:
   `GET /v1/tenants/{A}/entities` with `X-Tenant-Id: B` returned tenant A's
   entities in full.

Fixes: identity now comes from gateway-verified `X-Principal-Id` /
`X-Tenant-Id` headers (`middleware.Identity`); `actorFromJWT` and
`bearerToken` are deleted so the unsafe path cannot be reintroduced; a
mutation with no verified principal is 401; the authz client is real and
fails closed; and tenant-scoped reads assert the path tenant equals the
caller's verified tenant, returning 404 rather than 403 so a probe cannot
confirm a tenant's existence.

Carried forward: the superuser RLS bypass itself is unchanged and affects
every service in the estate that connects as `postgres`. The explicit scope
check makes this service safe regardless, which is the same belt-and-braces
posture purchase-order-svc and general-ledger-svc adopted after real CI
failures. Running the services as a non-superuser role with FORCE ROW LEVEL
SECURITY would make the policies load-bearing again and is a separate,
estate-wide change.

Also of note: `internal/store/tenant_isolation_test.go` already documented
this exact superuser trap and covers the store methods that take a tenant
filter. What it could not cover is the layer above, where the tenant comes
from the URL rather than the caller's identity — a query filtered by a
caller-supplied path parameter is not an isolation boundary however correct
its WHERE clause.


## Open: jurisdiction-rules-svc owns no compliance calendar
03-microservices.md §8.2 lists "compliance calendar logic" among this
service's holdings and jurisdiction.calendar.changed among its published
events. Neither exists: there is no calendar entity in its schema, and the
event name is deliberately absent from internal/events rather than declared
for a signal that could never fire. Filing due dates and filing_requirements
currently live in obligations-svc, so the boundary question — does the
calendar belong here, there, or split — has to be settled before the entity
is built. The other three §8.2 events (jurisdiction.rule.updated,
jurisdiction.rule.activated, legal.drift.detected) are published.

## Open: authorization scope for platform-wide reference data
Jurisdiction data has no tenant_id and no owning legal entity, but
authorization-svc's POST /v1/authorize rejects an empty legal_entity_id with
400. jurisdiction-rules-svc therefore presents a single synthetic
platform-scope entity (AUTHZ_PLATFORM_SCOPE_ID) on every decision, and role
assignments granting JURISDICTION_* / JURISDICTION_RULE_* actions must use
that same id. This is a workaround for a missing concept: authorization-svc
has no notion of a platform-scoped, non-entity resource. Any other
platform-wide service will hit the same wall.
seed-demo-rbac.ps1 does not grant these actions.

## Resolved: jurisdiction-rules-svc authorized nothing
HTTPAuthZClient.Authorize was a TODO that logged a warning and returned nil,
so every admin mutation was permitted without an authorization decision. The
production-startup guard made this worse rather than better: it forced a
non-placeholder AUTHZ_SERVICE_URL in production/staging, which is exactly the
branch that selected the no-op client — the StubAuthZClient it was written to
prevent would at least have been labelled a stub. docker-compose.yml
compounded it by pointing AUTHZ_SERVICE_URL at jurisdiction-svc itself
("mock stub authz points back"). The client now calls
authorization-svc's real contract and fails closed on denial, non-200,
unreadable body, and network error; that URL is rejected as a placeholder;
and internal/authz/client_test.go asserts each failure mode denies.

Related, same service: admin routes took the actor from X-Actor-Principal-ID
— a header nothing in the platform sets, since the gateway's ForwardAuth
middleware publishes X-Principal-Id — and fell back to the literal string
"system" when absent. Every write was therefore attributed to the platform
itself. Admin routes now require a principal (401 without) and record it.

## Test coverage gap: SQL-vs-stub verification
TestFindRules_SupersededRuleReturnedForHistoricalQuery (jurisdiction-
rules-svc) verifies stubStore's Go reimplementation of date-interval
filtering, not the real SQL query in pg_store.go. No integration-test
infrastructure (testcontainers-go, docker-compose, or a CI Postgres
service container) exists anywhere in services/ or ci.yml today.
tenant-entity-registry-svc's pg_store.go previously had ZERO test
coverage of any kind (see "Resolved" section below).


Fix in progress: internal/store/pg_store_test.go (env-guarded via
TEST_DATABASE_URL, skips locally, runs in CI) + Postgres service
container added to ci.yml, scoped initially to jurisdiction-rules-svc.

Closed for jurisdiction-rules-svc (2026-08-05). The stub reimplementation
still exists — it is the handler's fake, and that is the right thing for a
handler test — but the SQL it used to stand in for is now covered directly
against Postgres: half-open interval filtering, DRAFT exclusion, superseded
rules staying visible to historical queries and disappearing from current
ones, ancestor-chain resolution including a deliberately cyclic hierarchy,
rule-pack inheritance and override precedence, overlap rejection, drift
history, pagination ordering, and malformed-UUID handling. Both store test
files now apply every file in deployments/migrations rather than naming two
of them inline, so a migration added later cannot be silently skipped by the
tests the way one was silently skipped by the running dev volume.

## Resolved: identity-context-svc had no database
identity-context-svc's principal/role/delegation store
(internal/principal/repository.go) was a pure stub — every method returned
empty results or a "not implemented" error, with no database behind it.
It has been replaced by internal/store/pg_store.go, a real pgxpool-backed
implementation, with migrations in
deployments/migrations/000001_initial_schema.up.sql and an integration
test suite (internal/store/pg_store_test.go) wired into ci.yml's
TEST_DATABASE_URL matrix alongside jurisdiction-rules-svc. The /health
endpoint's TODO for a DB ping is also now implemented.

Known limitation carried forward: PrincipalStore's methods (other than
FindByIDPSubject) do not carry tenant_id through the interface, so — unlike
tenant-entity-registry-svc — these tables have no Postgres Row-Level
Security policy; FindByIDPSubject enforces tenant scoping via an explicit
WHERE clause instead. Enabling RLS here would require widening
PrincipalStore (and its resolver call sites and test mocks) to carry
tenant_id on every method — a larger, separate change.

Also carried forward: principal_role_assignments and delegated_authorities
have no write path in this service by design — Access Control Service and
Delegated Authority Service own those objects per
docs/architecture/03-microservices.md §9.3–§9.4, and neither exists yet —
so those tables will read back empty until internal/events/consumer.go is
wired to populate them from upstream events (tracked separately as the
event-backbone gap).



## Resolved: tenant-entity-registry-svc had zero test coverage
internal/store/pg_store.go had no tests of any kind — including no
verification that its Postgres Row-Level Security policies actually
isolate tenants, despite that being this service's central architectural
guarantee. Added internal/store/pg_store_test.go (env-guarded via
TEST_DATABASE_URL, same convention as jurisdiction-rules-svc and
identity-context-svc), covering CreateTenant/GetTenantByID,
CreateEntity/GetEntityByID, and — the important one —
TestPgStore_RLS_TenantIsolation, which creates two tenants each with
their own entity and proves a query scoped to tenant A cannot see
tenant B's data. Wired into ci.yml's TEST_DATABASE_URL matrix alongside
the other three services.

Follow-up (separate, not yet filed): the tests above cover Tenant and
LegalEntity only. EntityHierarchy, EntityJurisdictionAssignment,
DataResidencyPolicy, and TaxIdentityBundle still have no integration
test coverage.


## Resolved: vendor-due-diligence-svc could conclude a check without its evidence
internal/handler/handler.go recorded the screening outcome and the evidence
supporting it in two separate transactions, and the evidence write's failure
was logged and swallowed — then the response returned the evidence record
anyway. A caller could therefore be handed a COMPLETED/CLEAR check together
with the evidence for it while the store held the check and no evidence at
all, and a later GET returned the clean outcome with an empty evidence list.
For a service whose stated purpose is due-diligence evidence, that is an
unevidenced compliance pass which reads exactly like an evidenced one.

Replaced AddEvidence + CompleteCheck with a single store.ConcludeCheck: one
transaction, guarded on the check still being STARTED so a second conclusion
cannot overwrite a terminal one (the unguarded UPDATE allowed FLAGGED to be
replaced with CLEAR). Proved with internal/store/pg_store_test.go —
TestConcludeCheck_EvidenceFailureRollsBackTheConclusion forces an unusable
evidence row and asserts the check stays STARTED, and
TestConcludeCheck_ConcurrentConclusionsExactlyOneWins races eight
conclusions and asserts exactly one succeeds with exactly one evidence row.
The service previously had no store tests, which is why this survived: the
handler stub is a map and cannot fail one write while the other succeeds.

## Resolved: CLEAR was indistinguishable from a real sanctions clearance
The only screening this service performs is an exact, case-insensitive match
against a hardcoded list of two vendor names. There is no sanctions or
watchlist feed anywhere on this platform to call — external-data-feed-svc
carries MARKET_DATA, CREDIT_SCORE, COMPANY_INFO, FX_RATE and ESG_DATA only —
so the stub stands in for an integration that does not exist rather than
shortcutting one that does. Because the match is exact, "Acme Sanctioned
Holdings Ltd" screens CLEAR while "Acme Sanctioned Holdings" is flagged.

Nothing on the wire said so. risk_outcome was CLEAR and screening_basis was
free-text prose, which is not a contract, so any consumer reading CLEAR as
"this vendor is clear" would report an effectively unscreened counterparty as
a screened one that passed. Added vendor_dd_checks.screening_source
(migration 000002), written STUB_DENYLIST, returned on the read API AND
published on vendor.dd.completed — putting it on the read API alone would
have fixed the defect for whoever looks at the console and left it in place
for every automated consumer, which is the more dangerous half.

## Resolved: FAILED was unwritable and vendor.dd.failed unemittable
FAILED was a status no code path could set and vendor.dd.failed was an event
declared in 03-microservices.md §12.10 with nothing able to publish it. A
check whose conclusion failed was abandoned in STARTED, where it stayed
forever — indistinguishable in the register from any other row, with no route
to retry it and nothing downstream told a screening had been attempted and
lost. Added store.MarkFailed and the handler failure path, and migration
000002 adds a CHECK constraint making a partially-applied conclusion
unrepresentable (an outcome requires a conclusion, and a conclusion requires
its timestamp).

Note on that constraint: `risk_outcome IN ('CLEAR','FLAGGED')` alone did NOT
enforce it. A CHECK rejects a row only when its expression evaluates to
FALSE, and `NULL IN (...)` evaluates to NULL — so the COMPLETED branch went
NULL, the disjunction went `FALSE OR FALSE OR NULL` = NULL, and a COMPLETED
check carrying no outcome at all was accepted: precisely the state the
constraint existed to forbid. It needs an explicit IS NOT NULL beside the IN.

## Resolved: a blank vendor name was screened rather than refused
`vendor_name: "   "` passed the `!= ""` check, then screenVendorName trimmed
it to "" , matched nothing, and the check concluded COMPLETED/CLEAR — a clean
due-diligence result for a vendor with no name. Inputs are now trimmed before
the emptiness test. Screening also collapses internal whitespace, so a stray
double space cannot defeat the only screening there is.

## Resolved: a malformed authorization scope read as an outage
authorization-svc stores legal_entity_id in a uuid column and answers 503
`store_unavailable` for a non-UUID one — its own instance of the platform-wide
habit of reporting a driver error as an outage. From a calling service that
503 is indistinguishable from authorization-svc genuinely being down, so a
caller who mistyped a legal_entity_id filter was told the authorization plane
had failed. Callers must therefore validate the scope themselves before
asking: handler.validScope answers 400 `invalid_scope`.

Worth noting for other services: it is specifically the value used as the
AUTHORIZATION SCOPE that must be a UUID, not necessarily the column. In this
service legal_entity_id and counterparty_id are VARCHAR(255) (only check_id
is uuid), so a malformed counterparty filter is a valid comparison that
matches nothing and must NOT be reported as invalid.

Still open for this service: the screening itself. Replace
stubSanctionsDenylist with a real sanctions/watchlist integration when one
exists on the platform; screening_source is the field that will distinguish
its results from the stub's, and every historical row stays honest about
having been screened by the stub.

## Open: authorization-svc role creation reports a duplicate as an outage
Re-POSTing an existing role answers 503 `store_unavailable` rather than 200 or
409. Recorded in seed-demo-rbac.ps1, which tolerates it and relies on its
end-of-run verification instead.

## Open: several databases in deployments/init-db.sh do not exist on an
## existing postgres volume
init-db.sh only runs on first volume initialisation, so a database added to it
later is absent from any volume created before that. vendor_due_diligence and
counterparty_management both had to be created and migrated by hand.
counterparty-management-svc reported `healthy` throughout — its readiness
probe does not detect that its own database is missing, so a healthy container
is not evidence its schema exists.

## Resolved: board-resolutions-svc interpolated a request header into SQL
`setRLS` built its statement as
`fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)`, and tenantID is the
raw `X-Tenant-Id` header. `X-Tenant-Id: x'; ALTER TABLE board_resolutions
DISABLE ROW LEVEL SECURITY; --` ran as written — on the statement whose entire
job is enforcing tenant isolation. Now `set_config('app.tenant_id', $1, true)`,
so no string is assembled at all.

## Resolved: row-level security applied to none of board-resolutions-svc's queries
Worth reading even if you never touch this service, because the shape is
platform-wide. 000001 did `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` and wrote
a tenant isolation policy. Postgres exempts a table's OWNER from row-level
security unless the table is also declared FORCE ROW LEVEL SECURITY, and these
services connect as the owner — so the policy never applied to a single query
the service made. Combined with reads that carried no `tenant_id` predicate of
their own (`WHERE resolution_id = $1`, nothing more), one tenant could read
another's board minutes and resolutions by id.

Both halves are fixed: every statement now carries an explicit tenant
predicate, and 000002 adds FORCE plus an explicit `WITH CHECK`. Check any other
service whose store relies on the policy alone — the schema reads as if the
control is present, and it is not.

## Resolved: notification-svc authorized a list read only if you asked it to
`CheckAllowed` ran only when `legal_entity_id` was supplied. Omitting it — the
easier request to make — returned every notification in the tenant, across every
legal entity, subjects and bodies included, to any principal holding no grant at
all. A read is authorized by who is asking, never by which query parameters they
happened to send. Now: supplying the entity requires NOTIFICATION_VIEW on it;
omitting it means your own inbox, with the recipient filter forced to the caller.

## Resolved: attribution taken from the request body defeated segregation of duties
board-resolutions-svc wrote `created_by` and `passed_by` from the request body.
The SoD check compares a resolution's `created_by` against the principal passing
it, so a drafter could file their resolution under another name and then pass
their own work — the two strings no longer matched, and the one control the
doctrine rests on allowed it. Both are now the authenticated principal, and a
body field naming anyone else answers 400 rather than being silently rewritten.

Grep for other services taking an actor id from a body: any of them whose SoD
or approval check compares two stored actor fields has the same hole.

## Resolved: an evidence gate that failed open on anything it did not recognise
board-resolutions-svc's evidencereq client was `if outcome == "MISSING" {
refuse }; allow`. Everything else passed — including the empty string, which is
what a renamed field or a changed envelope produces, and indistinguishable from
SATISFIED under a deny-list. Now an allow-list of SATISFIED /
NO_REQUIREMENTS_DEFINED; anything else is an unusable answer and the pass is
refused. Both outbound clients in this service now have tests that drive a real
`httptest` server with the dependency's literal response shape — the gap that
let financial-close-svc ship three of these at once.

## Open: notification-svc holds rows from before the channel fix
The delivery adapter used to report an unrecognised channel as a delivery
failure, so a caller's typo produced a stored FAILED record and a
`notification.failed` event — evidence of an attempt no provider ever saw. The
dev database has one (`channel: 'PIGEON'`). Migration 000002 adds its CHECK
constraints `NOT VALID` deliberately: new writes are constrained, existing rows
are preserved. A register that quietly edits its own history is worth less than
one with an embarrassing row in it. Run `ALTER TABLE ... VALIDATE CONSTRAINT`
once the backlog has been dealt with by someone who can decide what to do
with it.

## Resolved: schema-registry-svc worked only on one developer's machine
Two independent instances of the same shape, both invisible locally and both
fatal anywhere else.

**The grant.** Registration is gated on SCHEMA_PUBLISH, and
`seed-demo-rbac.ps1` never granted it. It worked on the development machine
because a hand-made bundle (`SRB` on role `SR_1255`) had been left in that
Postgres volume by some earlier ad-hoc seeding. On a fresh volume, in CI, or on
anyone else's machine, every registration was 403 and the console's form was
dead while the page rendered perfectly. A `SCHEMA_FULL` bundle is seeded now.

**The migration.** `deployments/init-db.sh` applied only `000001`. Every SELECT
this service makes names `compatibility_mode` and `owning_service`, which
`000002` adds — so on a volume initialised from that script the column did not
exist and every read and write failed. Applied by hand locally, so again
invisible. Both `000002` and `000003` are listed now.

Worth generalising: a feature that depends on state nobody scripted is
indistinguishable from a working feature until someone else runs it. Grep
`init-db.sh` against each service's migrations directory, and each service's
authorized actions against the seed's bundles.

## Resolved: the seed's own idempotency probe could not see a missing scope
`seed-demo-rbac.ps1` decides whether it has work to do by probing every action
-- but only on the LEGAL ENTITY. SCHEMA_PUBLISH was already granted there by the
stray bundle above, so the probe found nothing missing, skipped every bundle,
and never granted the PLATFORM scope that schema-registry-svc actually
authorizes against for an entity-less registration. It then reported success.

This is the same failure the script's header already describes one level down
(the old single-action early return), recurring one level up in the scope
dimension. The probe now covers both scopes.

## Resolved: json.Valid is not schema validation
schema-registry-svc accepted any well-formed JSON as an event contract, under an
error message that already claimed json_schema "must be a valid JSON object".
`123`, `"a string"`, `null`, `[]` and `{}` all passed.

The consequence is worse than untidiness: a first version stored as `123` can
never be evolved. The next registration runs the compatibility check, the stored
baseline fails to parse into a shape, and every future version of that event
answers 400 forever -- the registry accepted a value that permanently bricked
the contract it was recording. Validation is now one shared parse used by both
the write path and the compatibility checker (they each had their own copy of
the shape struct, which is how the two could ever disagree), with migration
000003 as the schema-level backstop.

## Resolved: the console reported a breaking schema change as a version race
`lib/api/client.ts` folds an error body into one human string from its
`error`/`field`/`message`/`detail` keys. schema-registry-svc answers a breaking
change with `{error, violations: [...]}`, and `violations` -- the strings naming
the exact field that broke -- were dropped before any caller saw them.

The console distinguishes this service's two different 409s by whether
violations are present, so with them gone EVERY breaking change was reported as
the other 409: "another registration claimed this version, re-read and
resubmit". The reader retried, got the same 409, and was told the same thing,
forever. `ApiError` now carries the parsed `body` alongside the folded message,
and callers needing a structured member read it directly rather than scraping it
back out of prose -- the same conclusion financial-close-svc's structured 422
reached.

Check any other service whose error body carries structured findings: the
folding is right for the great majority and destructive for exactly these.

## Resolved: jurisdiction-rules-svc had no grants, so its whole admin surface was dead
Recorded as an open gap when this service was hardened on 5 Aug -- "RBAC seeding
must grant JURISDICTION_* / JURISDICTION_RULE_* against that same [platform] id;
seed-demo-rbac.ps1 does not" -- and it stayed open. All five admin routes
(jurisdiction create/deactivate, rule create/transition/record-drift) were 403
for every principal, so nothing could register a jurisdiction, record a rule, or
mark legal drift through any client.

Unlike schema-registry-svc's identical gap, there was not even a stray hand-made
grant making it appear to work locally. A `JURISDICTION_FULL` bundle is seeded
now, on the platform scope -- jurisdictions are platform-wide reference data with
no legal entity of their own, and the service requires AUTHZ_PLATFORM_SCOPE_ID at
startup for exactly that reason.

That is now three services found with the same shape in one pass
(schema-registry, jurisdiction-rules, and schema-registry's migrations). The two
greps worth running against every remaining service: `init-db.sh` against each
service's `deployments/migrations/` directory, and each service's authorized
action strings against `seed-demo-rbac.ps1`'s bundles. Note that the action
strings are DERIVED for this service -- upper(resource + "_" + action) in
internal/authz -- so they have to be read off the handler's resource/action
pairs, not guessed from a naming convention.

## Clarified: deactivating a jurisdiction makes it stop resolving
Worth writing down because the console asserted the opposite and a live
assertion caught it.

`POST /v1/admin/jurisdictions/{id}/deactivate` clears active_flag and end-dates
the row -- no hard delete, per platform doctrine. But `GET /v1/jurisdictions/{id}`
is an ACTIVE-ONLY lookup and answers 404 afterwards. That is deliberate: the
service's own ErrJurisdictionNotFound is documented as "returned when the
jurisdiction_id does not exist OR IS INACTIVE. Callers (e.g.
tenant-entity-registry-svc) must reject the assignment fail-closed."

So the list endpoint and the lookup endpoint disagree on purpose: a deactivated
jurisdiction is still visible in `GET /v1/jurisdictions` -- which is what lets a
register explain a historical record -- while being unbindable and
unvalidatable everywhere else. Deactivation is therefore consequential rather
than cosmetic, and it affects records ALREADY bound to that jurisdiction, not
just new ones. The console now says so in both places it mentions deactivation;
a stale comment in lib/api/jurisdictions.ts had claimed an obligation "CAN be
bound to an inactive jurisdiction", which is backwards.

## Resolved: a full 17-service audit, and the three shapes it found
Run 17 Aug across every finished service. Recorded because the SHAPES recur and
the checks are cheap enough to repeat.

**1. Migrations in the repo that init-db.sh never applied.** Two more, after
schema-registry-svc's: secret-vault-integration-svc's `000002_add_data_
classification` and bank-reconciliation-svc's `000003_add_gl_cash_account_code`.
Both columns existed on the development volume because someone had applied them
by hand, so both services worked here and would have failed on a fresh volume.
The second is not cosmetic — without gl_cash_account_code, matching compares
magnitudes and a statement line of -500.00 reconciles cleanly against a journal
that moved 500.00 the other way.

**2. Migrations init-db.sh DOES list that this volume never ran.** A different
failure with the same symptom: init-db.sh only executes when the Postgres data
directory is empty, so anything added after the volume was created has silently
never run. governance-decision-log-svc was answering 503 `store_unavailable` on
every decision read — `column "workflow_instance_id" does not exist` — because
000004 had never been applied here. Nine migrations across four services were
missing on this volume. Reconciling is safe to re-run: apply every up migration
without ON_ERROR_STOP and let the already-applied ones fail on duplicate objects.

**3. A stale image is not visible in any test result.** Every service's image
was compared against its source. The heuristic to use is narrower than it first
appears: comparing image-created against last-commit time OVER-reports, because
an image built from a working-tree file before that file was committed is
content-identical and cache-hits to the same image. The authoritative check is a
cached `docker compose build` exiting 0 — if the context changed, it rebuilds.
Timestamps are a smoke alarm, not the answer.

Also confirmed clean in the same pass: every authorized action string across the
17 is granted by a seed bundle, every up migration has a down (financial-close-svc
was missing one), and all 17 are present in the console's service registry.

## Resolved: obligations-svc had no authorization and no tenant dimension
Two structural gaps in one service, both shipped deliberately and both stale by
the time they were found.

**No authorization at all.** Its config carried the comment "No AuthZServiceURL
field: admin writes do not call Authorization Service yet -- it doesn't exist.
Deliberate, documented deferral matching policy-svc's and
governance-decision-log-svc's precedent." Both halves had gone stale:
authorization-svc is live on :8089, fifteen other services call it, and both
services cited as precedent had since been wired to it. What the deferral left
behind was an OPEN WRITE SURFACE on a statutory compliance register -- anything
able to reach the port could raise an obligation, close one, or file against
one. Three actions now gate it (OBLIGATION_CREATE, OBLIGATION_STATUS_UPDATE,
FILING_REQUIREMENT_CREATE), one per route rather than a blanket write
permission, because closing an obligation and raising one are different
authorities.

**No tenant dimension.** Not a missing filter -- a missing concept. There was no
tenant_id column, so every read returned every tenant's obligations. The
sharpest edge was the dedup key: obligation_code carried a GLOBAL unique index
and creation is idempotent on it (`ON CONFLICT DO NOTHING`, then look up and
return the existing row). A second tenant registering an ordinary code --
"VAT-Q1-2026" -- did not create their obligation. They were handed the FIRST
tenant's, with that tenant's legal entity, due date and source reference, as a
200. One tenant's compliance register answering with another's record, through
the documented happy path. Migration 000002 adds tenant_id to both tables, FORCE
row-level security, and re-scopes the unique index to (tenant_id,
obligation_code).

Also closed: created_by_principal_id was written from the request body (the
record of who raised a statutory obligation, self-declared); the status
transition read and wrote in two statements with no lock, so two concurrent
closes could both pass the state-machine check and the second overwrote
closed_at; the register was unbounded; no body size cap or unknown-field
rejection; Kafka BatchTimeout at the 1s default.

## Open: obligations-svc does not verify the legal entity belongs to the caller's tenant
Found by the live suite for the pass above, and deliberately NOT closed in it.

Authorization is scoped to the obligation's legal_entity_id, and tenancy is
scoped by X-Tenant-Id, but nothing checks that the two agree. A principal
holding OBLIGATION_CREATE on legal entity X can therefore write an obligation
referencing X into ANY tenant, simply by changing the header -- the row lands in
the caller's own tenant and is invisible to the entity's real tenant, so this
is a write-side integrity gap rather than a read leak.

The service already validates jurisdiction_id against jurisdiction-rules-svc and
fails closed; the same treatment for legal_entity_id against
tenant-entity-registry-svc is the fix, and it is a new outbound dependency
rather than a one-line check. Worth checking whether the other entity-scoped
services have the same hole -- the pattern of "authorize on the entity, scope
rows by the tenant, never reconcile the two" is not specific to this service.

## Open: applicability_decisions has no tenant dimension
Noticed while merging origin/main, which added the table and its store
independently of the tenant-scoping pass above. Not introduced by that merge and
not changed by it -- redesigning another branch's table is not a merge's job.

applicability_decisions carries no tenant_id, so it is not covered by the
row-level security that migration 000003 installs on obligations and
filing_requirements, and internal/store/applicability_store.go names no tenant in
any of its three statements. The table is reachable only through an
obligation_id, which IS tenant-scoped -- but the applicability queries do not
join back to obligations, so an obligation_id belonging to another tenant
returns that tenant's applicability decisions, including the facts_used payload
and who decided.

Note the store never touches the obligations table itself, so FORCE ROW LEVEL
SECURITY does not break it: the foreign-key check runs as the table owner with
row security off. The feature works; it is the isolation that is missing.

The fix mirrors the obligations pass: add tenant_id, backfill from the parent
obligation, FORCE RLS with a WITH CHECK policy, and route the queries through
withTenantTx.
