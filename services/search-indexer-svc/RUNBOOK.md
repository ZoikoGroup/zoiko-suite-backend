# search-indexer-svc — Operational Runbook

ZS-SVC-AB-001 §13.3 names eight mandatory runbooks. Each has a section here.

The thing to hold on to while reading any of them: **this service owns no
business truth**. §8.3 calls its indexes "disposable projections, not the sole
backup", and INV-01 says an index is never the authoritative source. So the
recovery action for almost every failure is *rebuild from source*, and the
question that actually matters during an incident is not "can we restore the
index" but **"is anything discoverable that should not be"**.

That asymmetry is why freshness and restriction lag are separate metrics, and
why the two have separate runbooks. A stale index is an inconvenience. An
unpropagated restriction is a disclosure.

---

## 0. First five minutes

```bash
curl -s localhost:8096/readyz | jq            # which dependency is down
curl -s localhost:8096/metrics | grep search_indexer_
docker logs search-indexer-svc --tail 100
```

`/readyz` names each dependency separately:

```json
{"status":"ready","checks":{
  "bootstrap":"ok","postgres":"ok","opensearch":"ok","alias_consistency":"ok"}}
```

| Check | Critical | If it fails |
|---|---|---|
| `bootstrap` | yes | The projector registry never loaded — §5 |
| `postgres` | yes | Control plane unreachable — §6 |
| `opensearch` | yes | Engine unreachable — §4 |
| `alias_consistency` | **no** | Drift: §3. Reported, not fatal — see below |

Alias drift is deliberately non-critical. Drift means the alias and the
control plane disagree about *which* generation is serving; the service is
still answering, and taking the instance out of rotation would replace a
correctness incident with an availability one. The probe is how you find out,
not how you respond.

---

## 1. Authorization service outage / INDETERMINATE spike

**Symptom.** `search_indexer_retrieval_decisions_total{outcome="indeterminate"}`
climbing; searches answering **206** with `ESR-008`; users report results
"disappearing".

**What is happening, and why it is correct.** Every R1/R2 result is
re-authorized against authorization-svc before its content is returned
(INV-05). When no decision can be obtained, the result is **suppressed** —
INV-07 and NP-04 both require failing closed. The service is working exactly
as specified; it is authorization-svc that is down.

```bash
curl -s localhost:8089/readyz
curl -s localhost:8096/metrics | grep 'authz_decisions_total.*unavailable'
```

**Do not** work around this by disabling re-authorization or pointing
`AUTHZ_SERVICE_URL` at the permit-all stub. The stub refuses to build outside
`ENV=local` precisely so that this cannot be done under pressure — a search
plane serving unauthorized results is a worse incident than a search plane
serving none.

**Resolve** by restoring authorization-svc. Suppression stops within
`decisionCacheTTL` (2s) of it answering again.

**Communicate** that results were *incomplete*, not *wrong*: every affected
response carried `completeness_state: PARTIAL` and a suppression count, so
callers were told.

---

## 2. Restriction / deletion propagation backlog or failed verification

**This is the one that is a disclosure, not an inconvenience.**

**Symptom.** `search_indexer_restriction_backlog{scope=...}` above zero and not
draining; `esr.restriction.failed` events; log line
`RESTRICTION NOT PROPAGATED — content is still discoverable`.

```bash
# Per-tenant view, through the API.
curl -s 'localhost:8096/v1/restrictions?scope=<scope>' -H 'X-Tenant-Id: ...' ... | jq

# Estate-wide, for an operator.
docker exec zoiko-postgres psql -U postgres -d search_indexer -c "
  SELECT scope_name, state, count(*),
         max(now() - effective_at) AS oldest
  FROM restriction_tombstones
  WHERE state <> 'VERIFIED'
  GROUP BY 1,2 ORDER BY oldest DESC;"
```

**Read the state correctly.** §2.2 is explicit that `APPLIED` is not
`VERIFIED`:

| State | Means |
|---|---|
| `PENDING` | Recorded; the index write has not been attempted |
| `APPLIED` | The index write succeeded — **but nothing has checked** |
| `VERIFIED` | Invisibility was independently **proven** by retrieval test |
| `FAILED` | Either the write failed, or verification found it still visible |

A `FAILED` tombstone whose reason is *"content is still discoverable after
propagation"* means **the content is discoverable right now**. Treat as a
disclosure incident.

**Immediate containment** — §8.2 permits blocking or degrading the affected
scope rather than continuing known over-disclosure. The safe lever is to
retire the serving generation's alias so the scope answers ESR-011 rather than
returning the record:

```bash
# Verify the record really is visible before acting.
curl -s "localhost:9200/<physical-index>/_doc/<tenant>:<source_type>:<source_id>" | jq '._source.tombstoned'

# If it is not tombstoned, re-apply the restriction. This is idempotent by
# source_event_id, so use a NEW one to force a fresh epoch.
curl -X POST localhost:8096/v1/restrictions -H 'Content-Type: application/json' \
  -H "X-Tenant-Id: $T" -H "X-Principal-Id: $P" -H 'X-Source-Channel: api' \
  -H 'X-Purpose-Context: INCIDENT_CONTAINMENT' -H "X-Request-Id: $R" \
  -H "Idempotency-Key: $R" -H "X-Correlation-ID: $R" \
  -d '{"scope":"...","source_type":"...","source_id":"...",
       "reason":"INCIDENT_REAPPLY","source_event_id":"incident-<ticket>"}'
```

**NP-60: you may not mark it accepted.** There is no API that moves a `FAILED`
tombstone to `VERIFIED` without the verifier proving invisibility, and adding
one would be a defect. "Accepted risk cannot relabel unsafe visibility as
PASS; the exception remains explicit and governed."

**Root causes seen so far**
* the scope has no ACTIVE generation, so there is nothing to verify against —
  recorded as `FAILED` with that reason, which is correct: "nothing to check"
  is not "we checked";
* OpenSearch unreachable during the sweep — resolves on its own once §4 is
  fixed;
* a restriction arrived with an epoch not newer than one already applied
  (ESR-013) and was refused — check whether the caller is replaying an old
  event.

---

## 3. Index generation corruption or accidental alias switch

**Symptom.** `/readyz` reports `alias_consistency` with a message like
*"scope X: alias serves Y but the control plane records Z as ACTIVE"*.

**This did not come from this service.** The control plane cannot produce that
disagreement on its own: a partial unique index allows exactly one ACTIVE
generation per scope, and activation writes the engine alias first and the
control-plane state second. Drift therefore means something changed the alias
from outside — a manual `_aliases` call, a restored snapshot, or a second
deployment pointed at the same cluster.

§8.3: **freeze automated cutover and open an incident.**

```bash
curl -s "localhost:9200/_alias/<scope>" | jq          # what is actually serving
curl -s localhost:8096/v1/index-generations?scope=<scope> ... | jq  # what we think
```

**Do not** "fix" it by editing the alias to match the control plane without
first establishing which generation is correct. NP-43 is explicit that when
the active generation is corrupt but the older one is stale on permissions,
you do not blindly roll back — you evaluate safety, and rebuild or block.

**Recovery.** Build a fresh generation from the PUBLISHED contract and
activate it through the API, which makes both halves agree again:

```
POST /v1/index-generations              {"scope": "<scope>"}
POST /v1/index-generations/{id}/state   {"state": "VALIDATING"}
POST /v1/index-generations/{id}/state   {"state": "READY"}    # validation runs here
POST /v1/index-generations/{id}/state   {"state": "ACTIVE"}
```

---

## 4. Engine partial outage or corrupt generation

**Symptom.** Searches answering **206** with `completeness_state: PARTIAL` and
a detail like *"2 of 5 shards failed"*; `search_indexer_engine_errors_total`
climbing.

The 206 is the specified behaviour (NP-19, INV-24): a 200 with failed shards
is a partial answer wearing a success status, and presenting it as complete is
the silent over-claim the whole completeness model exists to prevent.

```bash
curl -s localhost:9200/_cluster/health?pretty
curl -s localhost:9200/_cat/indices?v | grep <scope>
```

**Recovery is a rebuild, not a restore.** NP-45: an index backup is not an
authoritative backup. Rebuild the generation from source events (replay the
consumer group from the earliest offset for the affected topics) and activate
the new generation.

```bash
# Reset the consumer group to replay every event for one topic.
docker exec zoiko-kafka /opt/kafka/bin/kafka-consumer-groups.sh \
  --bootstrap-server localhost:9094 --group search-indexer-svc \
  --topic <topic> --reset-offsets --to-earliest --execute
docker restart search-indexer-svc
```

Replay is safe: every projection write is idempotent by
`tenant + source_ref + source_version`, and the restriction-epoch comparison
means a replayed create cannot resurrect a tombstoned document (NP-11).

---

## 5. Source-to-index checkpoint stall or partial partition loss

**Symptom.** `search_indexer_index_lag_seconds` rising; checkpoints reporting
`LAGGING` or `STALE`; `GET /v1/checkpoints` showing a ledger/engine mismatch.

```bash
curl -s localhost:8096/v1/checkpoints ... | jq
docker exec zoiko-kafka /opt/kafka/bin/kafka-consumer-groups.sh \
  --bootstrap-server localhost:9094 --group search-indexer-svc --describe
```

**Check the consumer is actually alive.** The failure mode that costs the most
time here is a consumer that *looks* idle: kafka-go discards reader errors when
`ErrorLogger` is nil, so a reader that can never join its group sits in
`FetchMessage` forever, logs nothing, and the process reports ready. This
service sets `ErrorLogger`, so group-coordination failures appear in the log as
`kafka: ...` warnings — **if the log is silent and lag is rising, the problem
is upstream, not the consumer.**

**A ledger/engine population mismatch** downgrades freshness to `LAGGING` and
logs `checkpoint: population mismatch between ledger and index`. The ledger
counted independently of the index, which is what makes NP-17 ("checkpoint
falsely advances past missing events") detectable at all. A persistent
mismatch means documents were written to the ledger but not the engine — the
window the ledger-first write order deliberately leaves open. It closes on the
next event for the same record, or on a generation rebuild.

---

## 6. Control-plane database unavailable

**Symptom.** `/readyz` reports `postgres` failing; the container leaves
rotation.

Searches cannot run: evidence cannot be recorded, and the planner cannot read
the published contract. That is correct — a search plane that could not record
what it served would be answering without an audit trail.

```bash
docker exec zoiko-postgres pg_isready
docker exec zoiko-postgres psql -U postgres -d search_indexer -c "\dt"
```

If the schema is missing (a fresh volume skips `docker-entrypoint-initdb.d`
when PGDATA already exists), apply it by hand:

```bash
for f in services/search-indexer-svc/deployments/migrations/*.up.sql; do
  docker exec -i zoiko-postgres psql -v ON_ERROR_STOP=1 -U postgres -d search_indexer < "$f"
done
```

---

## 7. Query abuse, enumeration, or an expensive-query incident

**Symptom.** `search_indexer_query_rejections_total{reason_code=...}` climbing
for one scope; a burst of `esr.security_filter.denied` events.

The event carries an **HMAC-keyed actor hash**, not a principal id — §11.2
specifies a hash for this event so Security can correlate repeated behaviour
without the abuse stream becoming a second copy of who searched for what.

```bash
curl -s localhost:8096/metrics | grep query_rejections_total
```

| Code | What the caller was doing |
|---|---|
| `ESR-003` | Sending engine query syntax — wildcards, regex, `_index` |
| `ESR-004` | Over the complexity budget, or a query below the 2-character minimum (NP-54 enumeration) |
| `ESR-005`/`ESR-006` | Probing for fields that are not registered searchable/returnable |
| `ESR-015` | Asking for an unbounded result window, or walking a cursor past its page limit |

To identify the actor, join the hash against `search_evidence` — which is
tenant-scoped and behind authorization, which is the point of keeping the two
separate.

**Tightening levers**, in increasing order of disruption:
`MAX_COMPLEXITY_SCORE` ↓, `MAX_RESULT_WINDOW` ↓, then retire the scope's
generation to take the surface offline entirely.

---

## 8. Emergency: disable a search scope

There is no "disable search" flag, deliberately — a flag is a thing that can be
left on. The lever is the generation lifecycle, which is audited and
reversible.

```bash
# 1. Find the ACTIVE generation.
curl -s 'localhost:8096/v1/index-generations?scope=<scope>' ... | jq '.generations[] | select(.validation_state=="ACTIVE")'

# 2. Build and activate a replacement, OR remove the alias directly in an
#    emergency. Removing the alias makes the scope answer ESR-011
#    INDEX_GENERATION_NOT_ACTIVE — an explicit refusal, never an empty
#    result set, so no caller concludes the data does not exist.
curl -X POST localhost:9200/_aliases -H 'Content-Type: application/json' \
  -d '{"actions":[{"remove":{"index":"<physical>","alias":"<scope>"}}]}'
```

Removing the alias outside the API creates deliberate drift, which `/readyz`
will report — that is the intended signal that an emergency action was taken
and the control plane needs reconciling afterwards.

**The index is not deleted.** INV-20: removing something from search is not
record deletion and cannot satisfy a DRC/PRV deletion obligation on its own.

---

## Appendix: reading the metrics

| Metric | Healthy | Investigate |
|---|---|---|
| `search_indexer_index_lag_seconds` p95 | within the scope's freshness class | rising → §5 |
| `search_indexer_restriction_lag_seconds` p95 | seconds | **any sustained rise → §2** |
| `search_indexer_restriction_backlog` | 0 | non-zero and not draining → §2 |
| `retrieval_decisions_total{outcome="indeterminate"}` | ~0 | any → §1 |
| `retrieval_decisions_total{outcome="suppress"}` | low and steady | a spike means access changed, or an attack |
| `searches_total{completeness="PARTIAL"}` | ~0 | any → §1 or §4 |
| `query_rejections_total` | low | a spike on one scope → §7 |
| `messages_consumed_total{outcome="quarantined"}` | 0 | any → a producer is outside its published contract |
| `messages_consumed_total{outcome="dead_lettered"}` | 0 | any → check the DLQ topic |
| `engine_errors_total` | 0 | any → §4 |

A note on `quarantined`: it is never transient. It means a source emitted a
field its contract does not register, a value that does not match its declared
type, a payload carrying a `SECRET_PROHIBITED` field, or an event with no
trusted tenant. The fix is a new contract version or a producer change — never
a retry. The message is in `<topic>.dlq` with an `X-DLQ-Reason` header saying
which.
