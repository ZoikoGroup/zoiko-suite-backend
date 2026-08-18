# Known Architecture & Test Coverage Gaps

## Resolved (2026-08-17): no circuit breakers anywhere on the platform (03-microservices.md §17.7)

7 services (10 packages: accounts-payable-svc, accounts-receivable-svc,
bank-reconciliation-svc, financial-close-svc, general-ledger-svc,
purchase-request-svc, treasury-svc) had retry-with-backoff
(`retryTransport`) for their outbound HTTP calls, but retry and circuit
breaking are different mechanisms — retry keeps trying a call that keeps
failing; a breaker stops trying once it's clear the dependency is down.
No service anywhere had the latter, and no other cross-service HTTP
client on the platform had either.

Added a closed/open/half-open state machine to the same `retryTransport`
struct: 5 consecutive failures (any method) trips it open for 10s, during
which every call fails fast without touching the network; the next call
after the cooldown is let through as a probe, closing the breaker on
success or reopening it on failure. Verified with new tests per package
(breaker trips and short-circuits, a successful half-open probe recovers
it) plus a full build/vet/test sweep.

## Resolved (2026-08-17): no dead-letter routing anywhere (03-microservices.md §17.7)

audit-event-store-svc and workflow-history-svc were the only two Kafka
consumers that even acknowledged this in a TODO comment; neither, nor
any other consumer, had it. Both left a failed message uncommitted
forever, on the assumption that "the broker will re-deliver after a
restart" — not actually true, since Kafka consumer group offsets are a
single per-partition watermark: a *later* message succeeding and
committing silently carries the offset past an earlier failed one,
permanently dropping it. Until that happened, the failed message also
head-of-line-blocked every other message on the partition.

Both runners now retry a failed message a few times against the same
handler call, and if it still fails, republish it unchanged to
`<topic>.dlq` (original headers preserved, plus the failure reason,
source offset, and timestamp) and only then commit past it. A failed DLQ
publish falls back to the old uncommitted-and-retry behavior, so this
never makes failure handling worse than before.

## Resolved (2026-08-17): procurement-workflow-svc could strand a real purchase order behind a failed local write

`IssueOrder` called purchase-order-svc, then recorded the resulting
`purchase_order_id` locally; if that second, purely-local write failed,
the case stayed APPROVED with no order_id even though the order now
genuinely existed upstream. 03-microservices.md §17.8's saga-discipline
mandate has no compensating-transaction mechanism anywhere on this
platform to unwind that — and shouldn't gain one here, since
purchase-order-svc keys the order on the case's own ID as an idempotency
key, so a cancel-and-retry compensation would just race a legitimate
retry.

The local write is now retried up to 3 times with a short backoff before
giving up — a transient blip right after a real external side effect is
worth retrying locally rather than surfacing an error for something that
already succeeded elsewhere. If every retry still fails, the system stays
recoverable regardless: a caller retrying the same endpoint re-issues
against the same idempotency key and gets another chance at the local
write.

## Resolved (2026-08-17): accounts-payable-svc's RequestPayment was non-duplicating but not idempotent

03-microservices.md §3.7 requires every state-changing API to be
idempotent. This endpoint had no client-supplied idempotency key (unlike
invoice creation's `correlation_id`) and relied solely on the
status-machine guard on `invoice_id`: a retry against an invoice already
PAYMENT_REQUESTED correctly never published a duplicate event, but it
also never succeeded — it returned 422, which is non-duplicating, not
idempotent. A client retrying a timed-out-but-actually-successful call
had no way to get back the success it already caused.

Now recognizes the retry from the invoice's own state: requesting payment
on an invoice already PAYMENT_REQUESTED returns 200 with the current
invoice and does not publish again, whether it was already in that state
before the call or a concurrent request won the same atomic transition
first. Any other status is still a genuine invalid transition and still
422s.

## Resolved (2026-08-17): every service connected to Postgres as the superuser, which unconditionally bypasses Row-Level Security

This was the single most-cited gap against the original architecture spec
(Doc 01 §11.2, "Row-level authorization enforced at the data access layer"
as a stated minimum; §17.1 "least privilege" as a core security principle;
§18.3 lists the tenant/entity model among "Non-Negotiable Foundations").
55 services define real `CREATE POLICY` tenant-isolation policies — every
one of them was running with that guarantee silently disabled, because a
Postgres superuser bypasses RLS regardless of how correct the policy text
is. This was previously "carried forward" as an accepted, unfixed risk in
this file (see the tenant-entity-registry-svc entry below) on the basis
that it was "a separate, estate-wide change" — it has now been made.

Fix: migrations still run as the superuser (DDL/extensions need that), but
every service now connects at runtime as a new, non-superuser,
non-owner role, `zoiko_app` (`NOSUPERUSER NOCREATEDB NOCREATEROLE
NOBYPASSRLS`). A non-owner, non-superuser role is automatically subject to
`ENABLE ROW LEVEL SECURITY` policies with no further schema change needed
— `FORCE ROW LEVEL SECURITY` was not required because `zoiko_app` was
deliberately never made the table owner. Applied to `deployments/init-db.sh`
and `init-db-phase5/6/7.sh` (so it's automatic for a fresh volume) and to
`deployments/docker-compose*.yml` (`DB_USER`/`DB_PASSWORD` and the
`DATABASE_URL`-style services), across all 63 databases in the main stack
plus the phase5/6/7 stacks.

Live-verified against a running instance, not just reviewed: with
`app.tenant_id` set to tenant A, a query against `tenants` as the
superuser returned **both** tenant A's and tenant B's rows (reproducing
the exact bug); the identical query as `zoiko_app` returned **only**
tenant A's row. Also verified `zoiko_app` retains full INSERT/UPDATE/DELETE
for its own tenant's rows (a write scoped to the wrong `tenant_id` is
correctly rejected by the policy's `WITH CHECK`, not merely its `USING`
clause), and that `tenant-entity-registry-svc` and `general-ledger-svc`
both boot and connect cleanly under the new credentials with no
application-level changes required.

Not in scope for this pass: the ~30 services with tenant-scoped tables
but no `CREATE POLICY` defined at all still have no RLS to make load-
bearing — they rely solely on the application-level `WHERE tenant_id = ...`
filtering that was always the case. Adding real RLS policies to those is a
separate, larger effort (each needs its own policy design, not just a
credential change).

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

Formerly carried forward, now resolved: the superuser RLS bypass itself
affected every service in the estate that connected as `postgres`. See
"Resolved (2026-08-17)" at the top of this file — every service now
connects at runtime as a non-superuser `zoiko_app` role, making this
service's own RLS policies load-bearing again, not just the explicit
application-level scope check described above (which remains in place as
defense-in-depth).

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

## Resolved: secret-vault-integration-svc's 000002 migration was never applied by init-db.sh
`000002_add_data_classification.up.sql` existed in the repo but init-db.sh
only ever ran `000001_initial_schema.up.sql` for this service — the same
class of gap as the "several databases... do not exist" entry below, but for
a migration rather than a whole database. Found live: `CreateSecretPolicy`
failed with `column "data_classification" of relation "secret_policies" does
not exist`, reported to the caller as a generic `store_unavailable` (yet
another instance of the platform-wide "driver error reported as an outage"
pattern already documented elsewhere in this file). Fixed by adding the
missing line to init-db.sh and applying the migration to the running
container.

## Open: secret-vault-integration-svc's broker never returns usable secret material
Live-verified the full policy → version → activate → put-material → broker
flow end-to-end for the first time anywhere on this platform. It works
exactly as designed through every step — except the design itself is the
gap: `vault.Backend.Get` (internal/vault/backend.go) deliberately returns an
opaque lease token, never the raw secret value ("this service brokers
access to secrets, it does not become a second copy of them"), and no other
endpoint anywhere in the service exposes the underlying material either.

That means there is currently no functional path — for a service or a
human — to get a secret's actual value back out of this vault once it has
been stored. The broker mechanism is fully real for *authorization and
audit* (who requested what, when, whether they were on the
allowed_workload_ids list, an immutable GRANTED/DENIED trail) but cannot
yet serve its apparent purpose of letting a consuming service bootstrap a
real credential (e.g. its own DB password) at startup.

An initial attempt to wire general-ledger-svc's DB password through this
path was built, live-tested against the real broker response, found to
return `"local-lease:aGqKvDINXRJ8370wYH66VVI6Aau0LIKQ"` instead of the
material that had actually been PUT, and reverted rather than shipped —
setting DB.Password to that value would have broken the DB connection the
moment SECRET_VAULT_SERVICE_URL was ever set to a non-empty value. This is
upstream of what a "wire the existing pieces together" pass can fix: it
needs a real design decision in secret-vault-integration-svc itself (e.g. a
genuine material-exchange path scoped to the exact allowed workload, or the
vault performing operations on the caller's behalf rather than ever handing
back a value) before any consuming service can be wired to it for real.

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

## Resolved: delegated-authority-svc let a caller delegate someone else's authority to themselves
Closed 18 Aug 2026, with migration 000002 and a caller/delegator binding in the
handler. Found by reading the create path, not by any test -- the service's own
test suite passed throughout, because every test named a delegator the caller
was entitled to name.

CreateDelegation ran two authorization checks and neither was the one that
mattered. It confirmed the CALLER held DELEGATION_CREATE on the legal entity,
then confirmed the DELEGATOR held the action_type being delegated -- the
platform's documented invariant, "delegated authority must never exceed the
delegator's own authority." What it never asked was whether the caller had any
relationship to the delegator at all. delegator_principal_id was a field in the
request body, and it was believed.

So any principal holding DELEGATION_CREATE could post a body naming a colleague
as delegator and themselves as delegate, and receive a 201. Both checks pass:
they may create delegations, and the colleague really does hold the action. The
invariant nobody had written down is that a principal may only give away
authority that is theirs to give. The result was a general-purpose privilege
escalation reachable through the documented happy path -- take any authority
held by anyone on an entity you can create delegations on.

The service's own doc comment made this harder to see rather than easier. It
said the delegator's authority was checked "rather than trusted from the request
body," which is true of the AUTHORITY and false of the IDENTITY, and reads as
though the body had been dealt with.

Closed in three parts:
  - The delegator must be the caller, unless the caller holds the new
    DELEGATION_ADMINISTER action on the entity. A separate action rather than a
    wider reading of DELEGATION_CREATE, because the two are different powers:
    one hands away authority you hold, the other moves authority between other
    people.
  - Even WITH DELEGATION_ADMINISTER, a delegation created for another principal
    may not name its creator as the delegate. Administering delegations between
    other people is legitimate; being the beneficiary of one you created is the
    same escalation by a longer route.
  - delegator and delegate must differ, enforced in the handler and as a NOT
    VALID CHECK.

DELEGATION_ADMINISTER is deliberately NOT in the demo RBAC bundle. Granting it
alongside DELEGATION_CREATE would restore the escalation for the demo principal
and make the fix untestable from the console.

## Resolved: delegated-authority-svc returned the whole tenant's register to an unauthorized reader
Closed in the same pass. Identical in shape to the notification-svc read gap,
and worth grepping for a third time.

ListDelegations authorized DELEGATION_VIEW only when the caller supplied a
legal_entity_id. Omitting it -- the shorter, easier request -- skipped
authorization entirely and returned every delegation in the tenant to a
principal holding no grant at all: who may act for whom, on what action, until
when. On this particular register that map IS the security model, so the
unauthenticated view was strictly more valuable to an attacker than any single
delegation.

A read is now scoped one of two ways and there is no third: with a legal entity
the caller needs DELEGATION_VIEW on it; without one the answer is restricted to
delegations the caller is personally party to. Asking after another principal's
delegations without an entity scope is 403 rather than silently narrowed,
because "you may not ask" and "there are none" are different answers and only
one of them is reassuring.

## Resolved: delegated-authority-svc had never run on this machine
Found while bringing the service up to verify the above, 18 Aug 2026.

The service is in docker-compose.yml and its database is in init-db.sh, and it
still crash-looped on startup with `database "delegated_authority" does not
exist`. init-db.sh runs ONLY when the Postgres volume is initialised empty, and
this database was added to the script after the development volume was created,
so the script had never created it here.

This is the fourth instance of the "works only on this machine" shape and the
first in the opposite direction -- not state applied by hand that was never
scripted, but state scripted that was never applied. It also explains why the
missing RBAC bundle below went unnoticed: the service was never up to return
the 403s.

Worth running against every service that is in compose but has never been
observed running, not just the ones with a console page.

## Resolved: delegated-authority-svc had no RBAC bundle at all
Closed in the same pass by adding DELEGATION_FULL (DELEGATION_CREATE,
DELEGATION_VIEW, DELEGATION_REVOKE) to seed-demo-rbac.ps1.

Nothing had ever granted a DELEGATION_* action, so every one of the service's
four routes would have answered 403 to every principal. The service has enforced
authorization since it was written; the grants simply did not exist. Same shape
as jurisdiction-rules-svc, whose admin surface had been 403 for twelve days for
the same reason -- the third time this has been found, and the reason
seed-demo-rbac.ps1 now reports missing actions before it starts.

## Resolved: delegated-authority-svc's row-level security applied to no query
Closed by migration 000002. 000001 ran ENABLE ROW LEVEL SECURITY and wrote a
tenant isolation policy; Postgres exempts a table's owner from row-level
security unless the table is FORCE, and these services connect as the owner, so
the policy had never applied to a single statement.

Isolation did not rest on it -- pg_store.go carries an explicit tenant_id
predicate on every statement, including the expiry sweep -- but a policy that
silently does nothing reads as a control that is present, and on a register of
who may act for whom that is the control an auditor would point at. 000002 adds
FORCE, an explicit WITH CHECK, and NOT VALID invariants for the status
vocabulary and the terminal-state evidence (a REVOKED row must carry who
revoked it and when; an EXPIRED row must carry expired_at).

## Open: nothing consumes delegation grants
Noticed while wiring the console, 18 Aug 2026. Not a defect introduced by that
pass, and not closed by it.

delegated-authority-svc records delegations and publishes authority.delegated /
authority.revoked / authority.expired. No service reads the register when making
an authorization decision: authorization-svc does not call it, and a grep for
consumers finds only identity-context-svc, which has a DELEGATED_AUTHORITY_URL
in its config and an InvalidationReasonDelegationRevoked enum value -- an
intention, not a call site.

So a delegation is currently a governed, audited RECORD that one principal may
act for another, and not a mechanism that lets them. Every escalation described
above is therefore a write-integrity and audit-integrity problem today rather
than a live privilege bypass -- which is the reason to close it now, before
something starts honouring these rows and turns a forged record into real
authority retroactively.

## Resolved: document-vault-svc authorized nothing, on any route
Closed 18 Aug 2026. Found by reading the service, not by any test — its own
suite of 25 tests passed throughout, because not one of them asked whether a
caller was allowed to do what it was doing.

There was no authorization of any kind: no authz client, no AUTHZ_SERVICE_URL,
no check. Every route answered anything that could reach the port, including
GET /v1/documents/{id}/content, which returns the document bytes — on a service
whose own schema classifies its contents PUBLIC / INTERNAL / CONFIDENTIAL /
RESTRICTED, and which financial-close-svc already uses to store close evidence.

Five actions gate it now, and the splits are deliberate:
  - DOCUMENT_CREATE, DOCUMENT_VERSION_CREATE — filing and amending.
  - DOCUMENT_READ vs DOCUMENT_DOWNLOAD — knowing a document exists and reading
    its bytes are different disclosures. The access log has recorded them as
    different access types (METADATA / DOWNLOAD) since the schema was written;
    authorization now agrees with the audit trail.
  - DOCUMENT_ACCESS_LOG_READ — the log says who read what and when. It is the
    record an investigator consults, and it should not fall out of ordinary read
    access to the document.

## Resolved: document-vault-svc's tenant filter switched itself off when omitted
Closed in the same pass, and this is the more dangerous half of the above.

The store's predicate was `($2::uuid IS NULL OR tenant_id = $2::uuid)`, with the
tenant supplied by a helper that returned nil when the request carried no
X-Tenant-Id. A NULL there makes the first disjunct TRUE for every row, so the
filter did not merely fail to narrow — it disabled itself. Combined with the
absence of authorization, a request with NO headers at all could read and
download any document belonging to any tenant.

It was documented rather than hidden, which is what makes it worth recording.
FindDocumentByID's doc comment read: "An empty tenant in context (e.g. a
not-yet-migrated caller) falls back to unscoped lookup — tightening that further
requires making the header mandatory platform-wide." That is an accurate
description of a cross-tenant read on a vault holding RESTRICTED content,
written as a compatibility note.

A missing tenant is now 401. The store's helper returns an error rather than an
empty string, so "no tenant" can no longer mean "all tenants" anywhere
downstream, and migration 000002 adds the row-level security that had never
existed on any of the three tables — not even the ENABLE-without-FORCE the rest
of the estate had to be corrected for.

## Resolved: document-vault-svc recorded readers as "unknown", and let them pick a name
Closed in the same pass.

actorFromHeader was three defects in nine lines. It read X-Actor-Principal-ID
FIRST — a header nothing in this platform sets and anything may send — taking
precedence over the X-Principal-Id the gateway verifies, so a caller could
attribute their own download to a colleague. Failing both, it returned the
literal string "unknown".

That last one matters most, because the access log is not a convenience: it is
the append-only record that answers "who downloaded this RESTRICTED document".
An unidentified caller was not refused; it was RECORDED, and the log could
answer "unknown" while reading as though it had answered.

The forgeable header is ignored, an unidentified caller is 401 before any access
is recorded, and 000002 adds NOT VALID CHECK constraints refusing '' and
'unknown' as a principal on all three tables. NOT VALID because existing rows
are the true record of what the service did, and a migration must not rewrite an
audit trail to make a constraint pass.

## Resolved: document-vault-svc had no way to list documents
Closed by adding GET /v1/documents. Six routes existed and every one of them
required a document_id the caller already had, so the vault could be written to
and read from but never browsed. That is why it had no console page: there was
nothing to put on one.

legal_entity_id is required on the new route rather than optional, because this
service authorizes per legal entity — a register spanning every entity in the
tenant would have no single scope to authorize against, and defaulting to "all
entities the tenant owns" is exactly how the unscoped reads elsewhere in this
platform came about.

## Open: a register read is not recorded in the access log
Noticed while writing the live suite for the pass above, and deliberately not
changed.

000001's comment states the doctrine: "Every read of a document's metadata or
bytes appends a row here". GET /{id} and /{id}/content honour it. The new
GET /v1/documents does not — it returns metadata for many documents and records
nothing, so the log understates who has seen what.

Recording one row per document per list call would make the log unreadable for
its actual purpose, and a "LIST" access type covering a result set is a schema
decision rather than a bug fix, so it is left open. Worth settling before anyone
relies on the access log being a complete account of metadata disclosure — today
it is a complete account of DOWNLOADS and of single-document reads only.

## Open: retention_policy is a label, not an engine
Pre-existing, recorded while wiring the console. documents.retention_policy is a
named string this service stores and nothing enforces: no purge is scheduled by
it, and no delete is blocked by it — there is no delete route at all.
ErrRetentionActive is defined in the domain and never returned. A document
marked for a seven-year hold is not held by anything in this vault; the label
records an intention that some other system would have to honour.
