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
