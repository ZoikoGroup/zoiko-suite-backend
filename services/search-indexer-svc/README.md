# search-indexer-svc

ZoikoSuite's **secure search control plane** — ESR-01 through ESR-05 of
`ZS-SVC-AB-001 Enterprise Search, Indexing, Query & Secure Retrieval Control`.

One binary implements all five canonical services. The spec describes five;
this estate runs one deployable per bounded context, and search's five
concerns share one control-plane database and one engine connection, so
splitting them would mean five copies of the same contract registry with
nothing to gain. The *boundaries* are kept — each concern is its own package,
nothing in the query path writes a projection — so the split stays available
later without a redesign.

| Spec service | Package | What it owns |
|---|---|---|
| ESR-01 Source, Schema & Index Contract Registry | `internal/handler/admin.go`, `internal/store` | What may be indexed, how it may be queried, what may be exposed |
| ESR-02 Secure Indexing, Projection & Propagation | `internal/projection`, `internal/indexer`, `internal/kafka` | Events → searchable projections; tombstones; checkpoints |
| ESR-03 Query Planning, Authorization & Execution | `internal/query` | Mandatory filter compilation, complexity budgets, signed cursors |
| ESR-04 Result Hydration, Snippet, Facet & Retrieval | `internal/retrieval` | Per-result re-authorization, hydration, safe snippets and facets |
| ESR-05 Index Lifecycle, Reindex, Drift & Operations | `internal/handler/admin.go`, `internal/indexer` | Generation build/validate/activate/retire, restriction verification |

---

## What changed, and why it had to

This service used to **poll obligations-svc over HTTP** every 60 seconds and
upsert whatever came back into OpenSearch. That design could never work, and
the backend completion tracker records it as **row 65a**:

> `fetchObligations` calls `GET /v1/obligations` on obligations-svc with **no
> headers at all**. That endpoint requires *both* `X-Principal-Id` and
> `X-Tenant-Id` (401 `tenant_missing`), so **every sync cycle fails and the
> obligations search index is never populated**.

The service reported healthy throughout, because readiness was set from
whether the loop was *running*, not from whether it was *achieving* anything.

The tempting fix — forward a tenant header — does not work either. The syncer
was deliberately cross-tenant: it polled every obligation and resolved each
one's tenant afterwards, and "all tenants" is not expressible in one tenant
header. Worse, **the resolution step had the same defect one level down**: it
called `tenant-entity-registry-svc`'s `GET /v1/entities/{id}` headerless, and
that endpoint scopes its query by the caller's own tenant and answers 404
without one. Both halves were broken. Fixing either by minting a privileged
cross-tenant read would have invented exactly the platform-scope surface two
tiers of isolation work had just removed.

**The documented architecture already answers this.** Doc 03 §37 and Doc 04
§9.8/§556/§620 place search indexes as event-driven *derivative projections* —
"search is derivative, never authoritative" — consuming domain events that
already carry `tenant_id` in their envelope, and therefore needing no
cross-tenant read privilege at all.

So this service now **consumes the event backbone**, takes the trusted tenant
from the envelope, and refuses to project anything that does not carry one
(INV-02). HTTP polling is gone.

One producer-side gap had to close for that to work: **obligations-svc's event
envelope carried `legal_entity_id` but not `tenant_id`**. That is fixed in
obligations-svc (`internal/events/publisher.go` now reads the verified tenant
from the request context), not worked around here — a resolution lookup in
this service would have reintroduced the privileged read.

---

## Architecture

```
                    domain events (zoiko.*.events)
                              │  tenant_id in the envelope
                              ▼
                    ┌──────────────────┐
                    │ internal/kafka   │  one reader per registered topic
                    │  ErrorLogger set │  DLQ + partition watch
                    └────────┬─────────┘
                             ▼
                    ┌──────────────────┐
                    │ internal/        │  contract allowlist
                    │   projection     │  prohibited-field refusal
                    └────────┬─────────┘  restriction epochs
                             ▼
                    ┌──────────────────┐
     ledger first   │ internal/indexer │───► projection_ledger (Postgres)
     engine second  │                  │     compare-and-set on epoch+version
                    └────────┬─────────┘
                             ▼
                    ┌──────────────────────────────────────┐
                    │ OpenSearch: <scope>-g<generation>    │
                    │ alias <scope> ──► the ACTIVE one     │
                    │ mappings are dynamic:"strict"        │
                    └──────────────────────────────────────┘
                             ▲
      POST /v1/search        │
            ▼                │
    ┌───────────────┐        │
    │ internal/query│ compiles mandatory filters from TRUSTED CONTEXT
    └───────┬───────┘ (never from the request body)
            ▼
    ┌────────────────────┐
    │ internal/retrieval │ re-authorizes EVERY hit → authorization-svc
    └────────────────────┘ hydrates R2, escapes snippets, suppresses the rest
```

### Two planes, two authorization models

| Plane | Routes | Authorization |
|---|---|---|
| **Tenant** | `/v1/search`, `/v1/retrieve`, `/v1/restrictions`, `/v1/search-exports`, `/v1/search-evidence`, `/v1/scopes` | Scoped by the caller's verified tenant; every result re-authorized per record |
| **Control** | `/v1/search-sources`, `/v1/index-contracts`, `/v1/index-generations`, `/v1/checkpoints` | **Platform-scoped** — a contract describes a shape shared by every tenant |

§9.1 asks for exactly this split: "administrative engine endpoints are isolated
from tenant/application traffic and require privileged operational access."

---

## API

Full request/response shapes are in `openapi.yaml`; events are in
`asyncapi.yaml`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/search` | Execute a governed search |
| `POST` | `/v1/retrieve` | Re-authorize and hydrate named refs (max 50) |
| `GET` | `/v1/scopes` | Search surfaces and their field capabilities |
| `POST` | `/v1/restrictions` | Apply a priority visibility removal |
| `GET` | `/v1/restrictions` | Propagation state for this tenant |
| `POST` | `/v1/search-exports` | Separately-authorized export boundary |
| `GET` | `/v1/search-evidence` | This tenant's search evidence |
| `POST` | `/v1/search-sources` | Register a source (platform) |
| `GET` | `/v1/search-sources` | List registered sources |
| `POST` | `/v1/index-contracts` | Draft a contract version (platform) |
| `GET` | `/v1/index-contracts` | List contracts |
| `GET` | `/v1/index-contracts/{id}` | One contract with its fields |
| `POST` | `/v1/index-contracts/{id}/state` | DRAFT → CERTIFIED → PUBLISHED |
| `POST` | `/v1/index-generations` | Plan and build a generation (platform) |
| `GET` | `/v1/index-generations` | List generations |
| `POST` | `/v1/index-generations/{id}/state` | BUILDING → VALIDATING → READY → ACTIVE |
| `GET` | `/v1/checkpoints` | Index freshness and population |

### Reason codes

Every refusal carries a `reason_code` from §11.3 and its canonical `reason`:

```json
{
  "error": "search_refused",
  "detail": "field \"api_secret\" is not registered as returnable on this scope",
  "reason_code": "ESR-006",
  "reason": "FIELD_NOT_RETURNABLE"
}
```

`ESR-001` … `ESR-020` are all implemented in `internal/domain/types.go`.

### Onboarding a domain

Four calls, no redeploy:

```bash
# 1. Register the source and its event contract.
POST /v1/search-sources
{ "owner_service": "obligations-svc", "source_type": "obligation",
  "event_topic": "zoiko.obligations.events",
  "event_types": ["obligation.created", "obligation.updated"],
  "restriction_event_types": ["obligation.deleted"],
  "sensitivity_ceiling": "FINANCIAL" }

# 2. Draft the field contract. Always created DRAFT — §4.2's publication
#    gates are a human workflow this service cannot perform.
POST /v1/index-contracts
{ "source_type": "obligation", "scope_name": "obligation",
  "retrieval_class": "R1", "authz_action": "OBLIGATION_READ",
  "fields": [ {"name": "obligation_code", "type": "TEXT",
               "searchable": true, "returnable": true, "snippet_allowed": true} ] }

# 3. Certify, then publish.
POST /v1/index-contracts/{id}/state  { "state": "CERTIFIED" }
POST /v1/index-contracts/{id}/state  { "state": "PUBLISHED" }

# 4. Build a generation, validate it, activate it.
POST /v1/index-generations                  { "scope": "obligation" }
POST /v1/index-generations/{id}/state       { "state": "VALIDATING" }
POST /v1/index-generations/{id}/state       { "state": "READY" }   # runs real validation
POST /v1/index-generations/{id}/state       { "state": "ACTIVE" }  # atomic alias swap
```

The topic is subscribed within 30 seconds of step 1; indexing begins at step 4.

---

## The invariants, and where they live

The spec's INV-01..INV-30 and NP-01..NP-60 are not decoration. Each is
enforced somewhere specific, and each has a test that fails if it is removed.

| Invariant | Where | Test |
|---|---|---|
| INV-02 trusted tenant on every projection | `projection.Project` refuses `ErrNoTrustedTenant` | `TestProject_RefusesEventWithNoTrustedTenant` |
| INV-04 filters cannot be negated by syntax | `searchclient.ExecutionPlan` has no field that expresses it | `TestCompile_UserFiltersCanOnlyNarrow` |
| INV-05/06 index hit is not permission | `retrieval.Execute` re-authorizes each hit | `TestExecute_ReauthorizesEveryR1Hit` |
| INV-07 unknown authorization fails closed | `authz` returns `ErrUnavailable`; retrieval suppresses | `TestExecute_UnavailableAuthorizerSuppressesAndDegrades` |
| INV-08 only registered fields are indexed | allowlist in `Project`, `dynamic:"strict"` mapping | `TestProject_UnregisteredFieldsAreNotIndexed` |
| INV-09 secrets never enter the index | `ErrProhibitedFieldPresent` + two DB CHECKs | `TestProject_RefusesPayloadCarryingProhibitedField` |
| INV-13 snippets follow field policy | `snippet_requires_returnable` CHECK + planner | `TestSchema_RefusesSnippetWithoutReturnable` |
| INV-17 query text is never stored | evidence holds a SHA-256 digest | `TestSearch_EvidenceNeverStoresQueryText` |
| INV-18 restriction priority + verification | `VerifyRestrictions` sweep, independent of the writer | `TestVerifyRestrictions_VerifiesInvisibleAndFailsVisible` |
| INV-21/24 no unvalidated activation, no silent partial | READY gate runs real validation; 206 on PARTIAL | `TestSearch_PartialAnswerIs206` |
| INV-29 export is separately authorized | `SEARCH_EXPORT` action, own reason, own evidence | `TestExport_RequiresItsOwnAuthorization` |
| NP-11/48 no visibility resurrection | epoch-first compare-and-set in SQL | `TestUpsertProjectionRecord_RestrictionEpochWins` |
| NP-24 stored markup cannot execute | escape-then-substitute in `safeSnippets` | `TestExecute_SnippetsEscapeStoredMarkupAndKeepOnlyOurMarks` |
| NP-42 alias drift is detectable | partial unique index + `/readyz` check | `TestIndexGenerations_OnlyOneActivePerScope` |
| NP-51 dynamic mapping is off | `dynamic:"strict"` | `TestApply_StrictMappingRejectionIsQuarantinedNotRetried` |
| NP-57 cursors are tamper-evident | HMAC-SHA256, tenant+scope+plan bound | `TestCursor_TamperedPayloadFailsIntegrity` |

---

## Environment

| Variable | Default | Notes |
|---|---|---|
| `ENV` | `local` | Gates the authz stub and the cursor-key default |
| `PORT` | `8096` | |
| `DB_HOST` / `DB_PORT` / `DB_NAME` / `DB_USER` / `DB_PASSWORD` / `DB_SSLMODE` | `localhost` / `5432` / `search_indexer` / `postgres` / `postgres` / `disable` | Control plane |
| `KAFKA_BROKERS` | `localhost:9092` | Comma-separated |
| `KAFKA_GROUP_ID` | `search-indexer-svc` | |
| `KAFKA_BOOTSTRAP_TOPICS` | *(empty)* | Only seeds the set before any source is registered |
| `OPENSEARCH_ADDRESSES` | `http://localhost:9200` | Comma-separated |
| `OPENSEARCH_USERNAME` / `OPENSEARCH_PASSWORD` | *(empty)* | Required in staging/prod |
| `AUTHZ_SERVICE_URL` | *(empty)* | **Refuses to start outside `local` if unset** |
| `AUTHZ_PLATFORM_SCOPE_ID` | *(empty)* | Control-plane writes 500 without it, rather than silently degrading to tenant scope |
| `CURSOR_SIGNING_KEY_HEX` | *(none outside local)* | ≥32 bytes, hex. **No default outside `local`** — NP-57 |
| `ZS_ENVELOPE_ENFORCEMENT` | `write-strict` | Set to `strict` in compose: ESR-001 must refuse reads too |
| `RESTRICTION_VERIFY_INTERVAL` | `30s` | §8.2 propagation verification sweep |
| `CHECKPOINT_INTERVAL` | `60s` | §5.3 freshness and population accounting |
| `SOURCE_HYDRATION_TIMEOUT` | `3s` | R2 hydration bound; a timeout SUPPRESSES |
| `MAX_RESULT_WINDOW` | `100` | ESR-015 |
| `MAX_COMPLEXITY_SCORE` | `100` | ESR-004 |
| `FACET_MIN_COUNT` | `2` | §7.3 / NP-08. Refuses to start below 2 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | Tracing is optional; its failure is not fatal |

---

## Metrics

§13.1 asks for seven signal families. The one that shapes the design is the
**separation of freshness from restriction lag** — the spec is explicit that
"revocation is not hidden inside a generic eventual-consistency SLA", so these
are two histograms rather than one with a label. A label would let an alert be
written against the aggregate, and the aggregate is dominated by ordinary
indexing.

| Metric | Labels | §13.1 family |
|---|---|---|
| `search_indexer_index_lag_seconds` | `scope` | Freshness |
| `search_indexer_restriction_lag_seconds` | — | **Restriction lag** (over-disclosure window) |
| `search_indexer_restriction_backlog` | `scope` | Restriction safety (gauge) |
| `search_indexer_retrieval_decisions_total` | `scope`, `outcome` | Authorization outcomes |
| `search_indexer_searches_total` | `scope`, `completeness` | Completeness |
| `search_indexer_zero_result_searches_total` | `scope` | Search quality |
| `search_indexer_query_rejections_total` | `scope`, `reason_code` | Security abuse |
| `search_indexer_messages_consumed_total` | `topic`, `outcome` | Completeness |
| `search_indexer_projections_total` | `scope`, `outcome` | Completeness |
| `search_indexer_generation_transitions_total` | `scope`, `state` | Reindex certification |
| `search_indexer_engine_errors_total` | `operation` | Engine health |

---

## Testing

```bash
go build ./... && go vet ./...
go test ./...                     # unit: projection, query, retrieval, handler, indexer, events, config, authz

# Store integration tests need a real Postgres 16. They SKIP without
# TEST_DATABASE_URL — set REQUIRE_DB_TESTS=1 to make that skip a failure.
docker exec zoiko-postgres psql -U postgres -c "CREATE DATABASE si_test_scratch;"
TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/si_test_scratch?sslmode=disable" \
  REQUIRE_DB_TESTS=1 go test -count=1 ./internal/store/

# Everything, end to end, against the running stack.
./scripts/audit.sh
```

A note on the store suite: it connects as a **non-superuser** for the RLS
tests. A superuser bypasses row-level security unconditionally — `FORCE ROW
LEVEL SECURITY` forces it for the table *owner*, not for a superuser — so the
same assertions run as `postgres` pass whether the policy is correct, broken,
or absent. An earlier draft did exactly that and reported a policy that was
never evaluated as working.

---

## Known limits and open decisions

These are deliberate, not oversights. Each maps to a controlled open decision
in §16 that is not this service's to close.

- **No semantic/vector search yet** (§10, OD-10/OD-11). Wave 7. The projection
  and restriction model already carries the lineage vectors would need, so
  adding them is an additional index family rather than a redesign.
- **`/v1/search-exports` records authorization and population; it does not
  stream data** (OD-13). The route exists to make INV-29's boundary explicit
  and enforced. Wiring a bulk writer needs the export-maximum and
  async-threshold decision, so it answers 202 with the recorded authorization
  rather than inventing a limit.
- **No R2 hydrator is wired** (OD-05). No scope is registered R2 yet. A scope
  configured R2 with no hydrator answers ESR-014 rather than silently
  downgrading to index content — a downgrade would be a freshness lie nothing
  downstream could detect.
- **Numeric SLOs are not set** (OD-03/OD-04). The metrics that would be
  alerted on exist and are separated correctly; the thresholds are a capacity
  decision.
- **Single OpenSearch cluster** (OD-01/OD-02). Residency is enforced logically
  via the `residency_region` mandatory filter. RESTRICTED-tier data pinned to a
  region-specific cluster remains the GTRM follow-up `search-client`'s README
  already flags.
