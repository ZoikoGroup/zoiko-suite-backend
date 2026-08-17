# Known Architecture & Test Coverage Gaps

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

## Open: several databases in deployments/init-db.sh do not exist on an
## existing postgres volume
init-db.sh only runs on first volume initialisation, so a database added to it
later is absent from any volume created before that. vendor_due_diligence and
counterparty_management both had to be created and migrated by hand.
counterparty-management-svc reported `healthy` throughout — its readiness
probe does not detect that its own database is missing, so a healthy container
is not evidence its schema exists.
