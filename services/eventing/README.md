# eventing

Shared event envelope and transactional outbox for every ZoikoSuite producer,
implementing ZS-EVENT-001 (Enterprise Domain Event Catalogue & Event Contract
Standard) §4 (envelope), §5.3 (partition key), §6 (outbox), §7 (at-least-once)
and §11 (retry, quarantine).

It replaces the hand-written `internal/outbox` + event-struct copies that 24
services carried, which had drifted apart (event id in the body vs only in a
header, `occurred_at` vs `emitted_at`, relays without `SKIP LOCKED` or an
attempt limit) and none of which matched the standard. It also fixes a
problem none of those copies addressed: kafka-go's `RequiredAcks` defaults to
fire-and-forget, so an outbox could mark an event published that the broker
never stored.

| Package    | What it gives you |
|------------|-------------------|
| `envelope` | `envelope.New(Spec)` — validated CloudEvents-aligned envelope, UUIDv7 id, `payloadhash`, opaque partition key. Emits legacy field names too until consumers migrate. |
| `outbox`   | `outbox.Enqueue(ctx, tx, env)` inside your business transaction; `outbox.Relay` to deliver; `SchemaSQL`; `VerifySchema`; `ReadStats`; `Requeue`. |
| `kafkaout` | kafka-go adapter: `RequireAll` acks, key-hash balancing, broker `Probe`, permanent-error classification. |

## Adopting it in a service

1. **go.mod**: `require zoiko.io/eventing v0.0.0` and
   `replace zoiko.io/eventing => ../eventing` (same pattern as `search-client`).
   The service's Dockerfile must build from `services/` so `../eventing` is
   in the build context.
2. **Migration**: add a numbered migration whose body is `outbox.SchemaSQL`
   verbatim. Leave the old outbox table in place: rows it still holds are
   evidence (§6.1: cleanup "must not delete evidence required for
   reconciliation before delivery is proven").
3. **Startup**: call `outbox.VerifySchema(ctx, pool)` next to the DB ping;
   build `kafkaout.New(...)` and `outbox.NewRelay(pool, writer,
   outbox.DefaultConfig(), log)`; `go relay.Run(ctx)`.
4. **Producers**: in the same transaction as the state change,
   `env, err := envelope.New(envelope.Spec{...})` then
   `outbox.Enqueue(ctx, tx, env)`. A validation error is a bug in the caller.
5. **Retire** the service's old relay and `internal/outbox` package. Let any
   rows left in the old table drain first (stop new writes to it, keep the
   old relay running until its backlog is empty, then remove it).

## Expand / contract on field names

All current consumers parse `event_id`, `event_type`, `emitted_at`,
`tenant_id`, ... By default the envelope carries these legacy names alongside
the canonical ones, so adopting this module breaks no consumer. When every
consumer of a topic reads the canonical names, set
`Spec.OmitLegacyFields = true` for that producer.

## Operating it

- `relay.ReadStats` → `Backlog`, `Quarantined`, `OldestBacklogAge`. Alert on
  backlog age that only grows, and on any quarantined event.
- A broker outage does **not** spend events' retry budgets: the relay probes
  the broker after a failed write and, if it is down, releases the batch
  uncounted and backs off.
- `relay.Requeue(ctx, eventID)` returns a quarantined event to the queue
  after the cause is fixed. The event id is kept: it is a redelivery of the
  same fact.

## Tests

`TEST_DATABASE_URL=postgres://... go test ./...` runs the outbox suite against
real Postgres (each test uses its own schema). `TEST_KAFKA_BROKERS=host:port`
additionally runs the real-broker adapter test.

## Event catalogue (ZS-EVENT-001 §19)

`catalogue` embeds the 196 canonical event types with owner, aggregate and
consumers. `catalogue.Lookup(type)` resolves one; `catalogue.Lint(entry)` is the
§21 semantic lint (shape, domain, past tense, no generic CRUD verbs, owner and
consumers present). Add new event types to `catalogue/catalogue.json`; the
tests fail on any entry that does not pass lint.

## Replay and quarantine cases (ZS-EVENT-001 sections 10 and 11.1)

- **Replay is rejected by default.** Set `Message.ReplayRunID` from the replay
  channel reader (never from the payload). `Inbox.Process` then returns
  `ErrReplayRejected` before touching the database, unless the consumer called
  `AllowReplay()`. Only rebuildable projections (search, analytics, read models)
  may opt in; payment, filing, e-signature and communication consumers must not.
  An opted-in replay is deduplicated in its own namespace per run, so it never
  changes live receipts.
- **Quarantine cases.** Append `inbox.CaseSQL` to the consumer's migration.
  `OpenCase` gives a quarantined receipt an owner, priority, failure class and
  SLA; `ResolveCase` needs a disposition, a resolver and evidence; the table
  constraint refuses a half-resolved case. Alert on `UnownedQuarantined > 0`.

## Platform controls (ZS-EVENT-001 section 14)

- **`policy.ProducerACL`**: exclusive ownership of event types or families
  (`com.zoikosuite.accounting.journal.*`). `CheckSpec(spec)` before
  `envelope.New` refuses a second producer (E-02, EV-T23). `NewProducerACL`
  rejects grants in which two services overlap; `strict` also requires the type
  to be in the catalogue and owned.
- **`policy.Residency`**: explicit, directional `TransferRule`s with a purpose.
  Cross-region delivery is denied by default, an unstated region or an unknown
  classification fails closed (treated as restricted).
  `Inbox.SetDeliveryPolicy(policy, consumerRegion, audit)` applies it before any
  write and passes the denial `Decision` to `audit` as control evidence (EV-T10).
- **`Inbox.SetObserver`**: one `Observation` per delivery (outcome, replay,
  quarantined, denied) for Prometheus or any metrics sink; it carries no payload.

## AsyncAPI contracts (ZS-EVENT-001 sections 12.1 and 21)

`asyncapi/<domain>.asyncapi.json` are AsyncAPI 3.1.0 documents generated from the
catalogue, one per domain (21): a channel per aggregate stream
(`zoikosuite.<domain>.<aggregate>.v1`), a message per event type, a `send`
operation naming the owning producer, and the shared CloudEvents-aligned envelope
schema. Do not edit them; after changing `catalogue.json` run
`go generate ./catalogue` in `services/eventing`. `TestAsyncAPIFilesAreCurrent`
fails on drift. The per-event `data` payload is an open object until its schema is
registered in schema-registry-svc (referenced by the envelope's `dataschema`).

## Secret gate (ZS-EVENT-001 E-15, section 13, EV-T09)

`envelope.New` refuses an event whose `data` holds secret material and names the
JSON paths, never the values (`envelope.ScanSecrets` runs the same check on its
own). It flags values that are secret wherever they sit (JWT, PEM private key,
bearer token, AWS key id, a URL with an embedded password, a Luhn-valid card
number) and a few credential key names with a non-empty value (`password`,
`client_secret`, `api_key`, `access_token`, `refresh_token`, `private_key`,
`cvv`, `card_number`, ...). Keys match exactly, so `secret_ref` and `token_type`
pass. If a payload needs to refer to a credential, send an opaque reference.

## Drafting payload schemas from real events (`schemainfer`)

Hand-deriving a schema per event type does not scale, and a schema invented
without looking at real payloads is worse than none. `schemainfer` infers an
**observed** contract from payloads a producer actually emitted: a field is
`required` only if present in every sample (and only with at least 2 samples), a
type is stated only where all samples agreed, `format: date-time` only if every
string parsed as RFC 3339. The output stays in the subset schema-registry-svc
compares, so it can be registered as is. Review it: the samples decide how strict
it is.

1. Capture: run the producer's integration suite against a throwaway Postgres
   with a trigger on `eventing_outbox` that copies `event_type` and `payload`
   into a side table (a DDL event trigger re-attaches it each time a test
   recreates the table).
2. Export `{"type": ..., "data": ...}` lines and run
   `go run ./schemainfer/cmd/schemainfer -in samples.jsonl -out <svc>/contracts/events -prefix <type prefix> -service <svc>`.
3. Keep the schemas honest in the service's own tests: validate what the suite
   emits against them (see general-ledger-svc `assertEventsMatchSchemas`, hooked
   into the kernel test environment so every kernel integration test checks it).
