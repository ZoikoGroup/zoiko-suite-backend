# search-indexer-svc — engineering context

The decisions behind the code, and the reasoning that is not obvious from
reading it. Written for whoever picks this up next.

---

## 1. Why this service was rewritten rather than repaired

The old implementation polled `obligations-svc` over HTTP every 60 seconds.
The backend completion tracker records it as **row 65a**, and the entry is
worth reading in full because it also explains why the obvious fix was
rejected:

> `fetchObligations` calls `GET /v1/obligations` on obligations-svc with **no
> headers at all**. That endpoint requires *both* `X-Principal-Id` and
> `X-Tenant-Id` (401 `tenant_missing`), so **every sync cycle fails and the
> obligations search index is never populated**. The tempting fix — forward a
> tenant — does not work: this syncer is *deliberately* cross-tenant […] and
> you cannot express "all tenants" in one tenant header.

Two things made this worse than the row states.

**The tenant resolution had the same defect one level down.** `resolveTenantID`
called `tenant-entity-registry-svc`'s `GET /v1/entities/{id}` headerless. That
endpoint reads the tenant from the request context and every store query
filters `AND tenant_id = $n` from it, so a headerless call resolves to no
tenant and returns nothing — mapped to 404. Both halves of the sync were
broken, and each masked the other: fixing the fetch would have produced a
resolver that 404'd on every record.

**Readiness was measured from the wrong thing.** The old `health.SetReady` was
driven by whether the sync *loop* was running, not whether it was *achieving*
anything. So the service reported healthy for as long as it had been deployed,
while indexing exactly zero documents.

The fix is the documented architecture, not a header. Doc 03 §37 and Doc 04
§9.8/§556/§620 place search indexes as event-driven derivative projections —
"search is derivative, never authoritative" — consuming events that already
carry `tenant_id` in their envelope and therefore needing no cross-tenant read
privilege at all. That is what this service now does.

### The producer-side gap that had to close

`obligations-svc`'s event envelope carried `legal_entity_id` but **not**
`tenant_id`. Its own comment described this as "correctly omitted, never
fabricated", which was true of the *domain object* — `domain.Obligation` has no
tenant field — and wrong about the *envelope*: the handler had already refused
every request carrying no verified tenant (`requireTenant`), so the value was
sitting in the request context at the moment of publication.

That is fixed in obligations-svc, in one place: `emit` reads
`middleware.TenantFromContext(ctx)`. It was **not** worked around here. A
tenant lookup in this service would have reintroduced exactly the privileged
cross-tenant read that row 65a refused to invent.

---

## 2. Why five spec services are one binary

ZS-SVC-AB-001 describes ESR-01 through ESR-05 as five canonical services. This
estate runs one deployable per bounded context, and search's five concerns
share one control-plane database and one engine connection — so five
deployables would mean five copies of the same contract registry, five
connection pools to the same cluster, and a network hop between the query
planner and the contract it plans against.

The **boundaries** are kept, so the split stays available later without a
redesign:

- `internal/query` never writes a projection and imports no store write path.
- `internal/projection` knows nothing about HTTP.
- `internal/retrieval` receives a compiled plan and cannot compile one.
- The control plane and tenant plane are separate files with separate
  authorization models (`handler/admin.go` vs `handler/handler.go`).

---

## 3. The decisions that would be wrong if reversed

### 3.1 Ledger first, engine second

`indexer.Apply` writes the control-plane ledger **before** the search index.
The ledger's `WHERE` clause is the concurrency control — it is what refuses a
stale replay and what refuses a restriction resurrection.

Writing the engine first would mean a stale event had already changed what is
searchable before anything checked whether it should, and the ledger's refusal
would be a record of a decision that had not been enforced.

The cost is a window where the ledger says indexed and the engine has not been
written. That window is recoverable — the next event for the same source_ref,
or a generation rebuild, closes it — and it fails **safe**: a document that
should be visible briefly is not. The other order fails unsafe: a document that
should not be visible is, and the ledger claims otherwise.

The checkpoint sweep detects the window by comparing both counts, which is the
only reason keeping a separate ledger is worth its cost.

### 3.2 Restriction epoch is compared before source version, and strictly

```sql
WHERE EXCLUDED.restriction_epoch > projection_ledger.restriction_epoch
   OR (EXCLUDED.restriction_epoch = projection_ledger.restriction_epoch
       AND EXCLUDED.source_version > projection_ledger.source_version)
```

A restriction takes the event's emission time as its epoch; an ordinary update
takes **0**. That asymmetry is the whole of NP-11 and NP-48: a replayed create
arriving after a deletion carries epoch 0, the stored epoch is positive, and
the compare-and-set refuses it — *however high its source version*. Visibility
cannot be resurrected by anything that is not itself a restriction decision.

Doing it in the `WHERE` clause rather than as read-then-write is what makes it
safe under concurrency.

### 3.3 A tombstone keeps the document and drops the content

The obvious implementation of "remove from search" is `DELETE`. It is wrong
here: a deleted document has no epoch to compare a late replay against, so the
replay simply re-creates it. The tombstone keeps the governance lineage —
tenant, source ref, epoch — and empties `Fields`.

`DeleteProjection` exists for the cases where a tombstone would be pointless (a
generation being rebuilt, a source type retired) and is never used on the
restriction path.

### 3.4 The trusted context never comes from the request body

`query.Request` has no `tenant_id`, `actor` or `purpose` field at all. NP-01
says "reject/ignore client tenant; trusted context wins", and giving the body
nowhere to put one makes it structural rather than validated.

The handler's decoder additionally sets `DisallowUnknownFields`, so a caller
that *tries* is **refused** rather than silently ignored. That is the
difference that matters: a caller whose tenant was quietly dropped would read
an empty result as "that tenant has no data".

### 3.5 Mandatory filters are a different field from user filters

`searchclient.ExecutionPlan` has `MandatoryFilters`, `MandatoryMustNot` and
`UserFilters` as separate slices, ANDed together at compile time. There is no
field on that type capable of expressing a negation of the mandatory set — so
NP-03 ("user negates mandatory ACL filter") fails at the type level, not at a
validation step somebody could later move or forget.

### 3.6 Snippets are escaped before markers are substituted, never after

The engine is asked to wrap matches in private-use sentinels (`U+E000` /
`U+E001`) rather than `<em>`. The fragment that comes back is therefore
entirely untrusted text containing two known runes. `safeSnippets` escapes the
whole fragment first, then swaps the sentinels for `<mark>`.

The ordering **is** the control. Doing it the other way round would escape the
tags we just added and leave stored markup alone — NP-24 in one step.

### 3.7 R0 cannot carry anything above INTERNAL

R0 skips re-authorization by design (§7.1: "no source hydration required if
restriction epoch is current"). A contract that put personal, HR, financial or
legal content behind R0 would return indexed content with no current-
authorization check at all — INV-05 defeated by *configuration* rather than by
a bug, which is far harder to notice. The contract validator refuses it.

### 3.8 The cursor is signed, not encrypted

The payload is readable, so an attacker can see exactly what to change — and
changing it invalidates the token, which is the property that matters (NP-57).
Encryption would hide the contents and leave them malleable, which is strictly
worse: it would look opaque while remaining forgeable.

The cursor is bound to the tenant, the scope **and the plan digest**, so a
cursor from a different filter set cannot be replayed onto this one (NP-56).
Outside `ENV=local` the signing key has no default: a fixed fallback would be
public the moment this repository is read, and a cursor signed with a public
key is not signed.

### 3.9 Total is reported inexact rather than adjusted downward

Once anything is suppressed, `total_is_exact` goes false and the engine's count
is reported unchanged. Subtracting the suppressions would tell the caller
exactly how many results it was not allowed to see — which is the existence
disclosure §7.3 and NP-40 are about.

### 3.10 Facet minimum-cell is enforced twice

`min_doc_count` is set on the request *and* re-applied when decoding. Belt and
braces, because an engine upgrade that changed `min_doc_count` semantics would
otherwise turn a security rule off silently, and a facet count of one can
reveal a single privileged record (NP-08).

---

## 4. Things that look like bugs and are not

**`/readyz` reports `alias_consistency` but does not fail on it.** Drift means
the alias and the control plane disagree about *which* generation serves. The
service is still answering; removing the instance from rotation would replace a
correctness incident with an availability one. The probe is how you find out.

**A quarantined message is committed.** A contract violation does not become
valid on a retry, and blocking the partition on it would stop every other
tenant's indexing behind one bad document. It is also copied to the DLQ, which
is the evidence a source owner needs.

**`RestrictionFailed` is recorded when a scope has no active generation.**
"There is nothing to check" is not the same claim as "we checked and it is
gone", and NP-60 forbids relabelling an unproven state as a pass.

**A `POST /v1/search` requires an `Idempotency-Key`.** The envelope middleware
runs in strict mode and classifies any non-GET as a material write. §11.1 does
specify "Request ID + canonical query digest" as this route's idempotency, so
requiring one is consistent — and it costs a caller one header.

**Tracing failures are logged, not fatal.** An OTLP collector outage must not
take search down with it. The authorization client is the opposite: it refuses
to build the permit-all stub outside `ENV=local`, because a search plane
serving unauthorized results is worse than one that will not start.

---

## 5. Defects found by the tests during this build

Recorded because they are the class of thing that recurs, not because they are
still open — all four are fixed.

1. **Nil Go slices became SQL NULL.** `event_types`, `restriction_event_types`,
   `partition_set` and `reason_codes` are `NOT NULL DEFAULT '{}'`, and an
   explicit NULL does not trigger a default. Registering a source with no
   restriction events — an ordinary registration — failed with a 500, and every
   *successful* search failed to record its evidence while the search itself
   still returned results. The symptom would have been an evidence table
   containing only refusals. Normalised in the store, where the column lives.

2. **`field_id UUID PRIMARY KEY` on `search_field_definitions`.** The domain
   uses the field *name* as its identity everywhere — the projection writes it
   under that name, the planner looks it up by that name — and the insert
   silently passed the name into the UUID column. The surrogate key was a
   second identity nothing used; it is gone, and the primary key is
   `(contract_id, field_name)`.

3. **The RLS test connected as a superuser.** A superuser bypasses row-level
   security unconditionally — `FORCE ROW LEVEL SECURITY` forces it for the
   table *owner*, not for a superuser — so the assertions passed whether the
   policy was correct, broken, or absent. The test now creates an unprivileged
   role and connects as it, and additionally asserts that the correct tenant
   *does* see its row, so "passes because nothing is enforced" is no longer a
   way to pass.

4. **`strings.Contains(query, "script")`.** The forbidden-operator check
   matched substrings, so `description`, `transcript`, `prescription` and
   `subscription` were all refused as attempted engine scripts — four ordinary
   things to search a business corpus for, with a refusal the caller could
   neither understand nor work around. Engine identifiers now match on word
   boundaries; punctuation operators still match as substrings, which is
   correct for them.

---

## 6. What is deliberately not built

Each maps to a §16 controlled open decision that is not this service's to
close. None is a stub pretending to work.

| Not built | Open decision | Why the boundary is still enforced |
|---|---|---|
| Semantic / vector search | OD-10, OD-11 | Wave 7. The projection and restriction model already carries the lineage vectors need, so it is an additional index family rather than a redesign. |
| Bulk export delivery | OD-13 | `/v1/search-exports` authorizes, records the population and transfers nothing. The route exists so INV-29's boundary is enforced rather than implied; inventing a size limit would be making OD-13's decision. |
| R2 source hydration | OD-05 | No scope is registered R2. A scope configured R2 with no hydrator answers ESR-014 — a loud refusal — rather than silently downgrading to index content, which would be a freshness lie nothing downstream could detect. |
| Numeric SLOs | OD-03, OD-04 | The metrics that would be alerted on exist and are correctly separated; the thresholds are a capacity decision. |
| Region-pinned clusters | OD-01, OD-02 | Residency is enforced logically via the `residency_region` mandatory filter. Physical pinning for RESTRICTED-tier data remains the GTRM follow-up `search-client`'s README flags. |

---

## 7. Where to start reading

1. `internal/domain/types.go` — the entities, the state machines and the
   ESR-001..020 reason codes. Everything else refers to these.
2. `internal/projection/projector.go` — the allowlist, and why a projection is
   built from the contract rather than filtered from the payload.
3. `internal/query/planner.go` — the refusals. This package is mostly
   refusals, in §1.3's decision order.
4. `internal/retrieval/retrieval.go` — the two-stage retrieval, and why an
   index hit is not a permission.
5. `deployments/migrations/000001_initial_schema.up.sql` — the tenancy split,
   and the two partial unique indexes that make illegal states unwritable.
