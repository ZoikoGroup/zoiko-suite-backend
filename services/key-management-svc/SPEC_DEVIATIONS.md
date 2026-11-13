# key-management-svc — spec deviations and disclosed shortcuts

Specs: ZoikoSuite Security, Privacy, Residency & Cryptographic Architecture
Standard (cited below as "the Standard"), ZS-ARCH-SVC-001 §4 (identity
envelope), platform doctrine. This file starts with the 2026-11-12 pass that
gave the service a database and a token vault.

## 1. First database — Postgres replaces the in-memory store (2026-11-12)

**What it was.** Every key registration lived in a Go map
(`store.MemoryStore`), wiped on every restart. Tracker row 82 tolerated that
while the service held key *metadata* only.

**Why that ended.** The token vault stores customers' bank account numbers
(encrypted). A vault that forgets on restart destroys those values and makes
every token held by banking-connector-svc and payee-banking-identity-svc
undetokenizable, permanently. The Standard's key-custody table says, for
platform-managed keys, "Platform must preserve redundancy and recovery"; an
in-process map has neither. Moving the vault to Postgres while leaving key
metadata in memory would have been a half-fix, so both moved.

**What changed.**
- `deployments/migrations/000001_initial_schema.{up,down}.sql`:
  `customer_keys` (one column per `domain.CustomerKey` field) and
  `tokenized_values`. Both have RLS **ENABLE + FORCE**, with a tenant
  policy on `app.tenant_id`.
- `internal/store/pg_store.go`: `PgStore` implements the unchanged
  `store.Store` interface and the new `store.Vault`. Every method runs in one
  transaction that sets `app.tenant_id` *and* filters on `tenant_id`
  explicitly, so a superuser connection (which bypasses RLS) is still scoped.
  Rotation is a single `UPDATE … key_version + 1`, so concurrent rotations
  cannot lose an increment.
- `cmd/server/main.go`: `DATABASE_URL` is required and fatal when missing
  or unreachable, the same posture as `AUTHZ_SERVICE_URL`. There is no
  fallback to memory.
- `deployments/docker-compose.phase6.yml` (this service's block, plus one
  migrations mount on `postgres`) and `deployments/init-db-phase6.sh`
  (`create_db key_management`, its migration, and `key_management` added to
  the `zoiko_app` grant loop; without that the runtime role cannot connect).
  `init-db-phase6.sh` runs only on a **fresh** `postgres_p6_data` volume, so
  an existing local volume needs recreating or the database creating by hand.

**Tenant column type: UUID, and why.** `requireTenant` accepts any non-empty
string, and this service's own handler tests use `"t1"` and `"tenant-a"`, so
"tenant IDs are always UUIDs" is **not** something this service enforces.
The UUID choice rests on what callers actually send:
- tenant-entity-registry-svc's `tenants.tenant_id` is `UUID`;
- gateway-auth-svc sets `X-Tenant-Id` from the signed token's tenant claim;
- the gateway's mTLS platform scope is `00000000-0000-0000-0000-00000000f001`.

`PgStore` refuses a non-UUID tenant (`ErrInvalidTenant`) before any SQL, so a
malformed header can never reach the `::uuid` cast in the policy. Key
`legal_entity_id` stays `TEXT`: this service never joins on it. It only
passes it to authorization-svc.

**Existing tests.** The 5 key-route test files are unchanged and still pass.
They still construct `store.NewMemoryStore()` and use non-UUID tenants
(`"t1"`), so they could not run unchanged against `PgStore` and its UUID
column. Instead, `internal/store/store_test.go` runs one conformance suite
against **both** `MemoryStore` and `PgStore`. It covers the key lifecycle,
tenant scoping on get/list/rotate/disable, the legal-entity filter, and vault
scoping and idempotency, with UUID tenants. That suite is what shows the
store the handler tests use behaves like the store production runs.
`MemoryStore` stays as that test double; `cmd/server` no longer uses it. The
comment in `tenant_scope_test.go` saying the service has "no migrations and
no Postgres" is now stale; it was left alone so that test file stays
unchanged.

## 2. Vault encryption: AES-256-GCM under one configured key, not an HSM

`internal/crypto` uses the approach document-vault-svc already established
and disclosed: AES-256-GCM, a 32-byte hex key from the environment
(`VAULT_MASTER_KEY_HEX`), a random nonce per value, and refusal to start on a
missing or wrong-length key. It is real encryption at rest. It is **not**
what the Standard ultimately asks for: KMS/HSM-held keys, rotation and
custody evidence.

Two small additions over document-vault's backend:
- **Associated data.** Each ciphertext is bound to its tenant and `token_id`.
  A ciphertext copied onto another row or tenant, by anyone with table write
  access, fails to decrypt instead of disclosing someone else's value.
- **A format-version byte.** A later move to per-tenant or KMS-wrapped keys
  can then tell old ciphertexts apart.

Known limits:
- one key for all tenants;
- no rotation or re-encryption path;
- no key id per row;
- losing or changing the key makes every stored token undetokenizable;
- the compose value is a throwaway local-dev key.

**S4 is refused.** The vault accepts `S2_CONFIDENTIAL` and `S3_RESTRICTED`
only. The Standard requires "HSM/external key control … dual control" for S4
Crown-Jewel data, which a single software key does not meet. S0 and S1 need
no tokenization.

## 3. TOKEN_DETOKENIZE is a v1 of SEC-T15, not a maker-checker workflow

SEC-T15: "detokenization requires additional scoped approval and evidence."
This pass implements:
- a distinct `TOKEN_DETOKENIZE` action, checked at authorization-svc,
  fail-closed;
- the check is made at the legal entity **stored with the token**, never one
  the caller supplies;
- holding `TOKEN_CREATE` (or any key privilege) does not imply it.

It does **not** implement:
- **per-request approval**, a second person approving each detokenization
  (maker-checker / dual control);
- **an approval reference** carried on the request
  (`X-Approval-Reference`) and verified;
- **time-boxed or purpose-bound grants.** `X-Purpose-Context` is required
  by the envelope but not evaluated.

A principal holding the grant can detokenize any token in its tenant and
legal entity, as often as it likes; each attempt is recorded (section 5). A
support agent asking for raw bank details (SEC-T15's scenario) is covered
only to the extent that support roles are not granted `TOKEN_DETOKENIZE`.
This is a lighter control than the Standard describes, disclosed here rather
than presented as complete.

## 4. Tokenize: a material write, so authorized and idempotent

The original brief listed tokenize with tenant and principal checks only.
Two additions follow platform doctrine:
- **`TOKEN_CREATE` authorization** at `X-Legal-Entity-Id`, because material
  writes are authorized via authorization-svc. Prerequisite: callers'
  principals need this grant before `TOKENIZATION_ENFORCED` is turned on.
- **Idempotency** on `Idempotency-Key`, unique per tenant (INV-08):
  - same key and same request → the same token back;
  - same key and a different request → 409.

  The comparison decrypts the stored row; it does not hash the value. A
  plain hash of an account number is brute-forceable from the small space of
  valid numbers.

These need two `tokenized_values` columns beyond the brief's list:
- `legal_entity_id`, the scope detokenize authorizes against;
- `idempotency_key`.

`tokenized_values` is append-only: a trigger refuses `UPDATE` and `DELETE`.
Repointing a token at a different value would silently change every stored
copy of it. **Erasure** of a tokenized value (privacy requests) therefore
has no path yet and must become a governed operation, not a `DELETE`.

## 5. Detokenization evidence (SEC-INV-15) rides on SIEM, best-effort

Every detokenize attempt that reaches an identified tenant and principal
emits `token.detokenized` at HIGH, with its outcome:
- `succeeded`;
- `refused:not_available_to_tenant`;
- `refused:not_authorized`;
- `refused:authorization_unavailable`;
- `refused:invalid_request`;
- `failed:*`.

Tokenize emits `token.created` (MEDIUM). This reuses the service's existing
SIEM mechanism, as for `key.rotated` / `key.disabled`. No value is ever in an
event or log line.

That mechanism is a monitoring side channel, so as an audit trail it is
weaker than the Standard's "evidence":
- **Opt-in per tenant.** A tenant without an ACTIVE SIEM exporter records
  nothing in SIEM.
- **Lossy.** Events go through a bounded queue and are dropped (counted and
  logged) when it is full.
- **Recorded under the caller's tenant only.** A cross-tenant attempt is
  recorded there and not under the token owner's tenant: under RLS the vault
  cannot see whose token it was.

**Mitigation in this pass.** Each attempt is also written to this service's
own structured log (`event_type=token.detokenized`). That record exists
whether or not SIEM is configured, but it is not tamper-evident.

**Follow-up.** A durable, append-only evidence record (evidence-manifest-svc
or an outbox event), written in the same transaction as the decision.

## 6. Contract with the two existing clients

The server matches the proposed contract in banking-connector-svc's and
payee-banking-identity-svc's `internal/tokenization/client.go`:
- paths;
- JSON bodies;
- `X-Tenant-Id` / `X-Principal-Id` / `X-Correlation-ID`;
- `S3_RESTRICTED`;
- 200 on success;
- 403 as the only "denied".

**Differences, and where each was fixed:**
1. **The §4 envelope.** This service's envelope middleware (default
   write-strict) refuses a POST that lacks `X-Request-Id`,
   `X-Source-Channel`, `Idempotency-Key`, `X-Legal-Entity-Id` and
   `X-Purpose-Context`. The proposal sent none of them, so every call would
   have been a 400 before reaching the vault. **Fixed in both clients**,
   which now send the full envelope. Exempting these routes from the envelope
   would have meant exempting a write.
2. **Legal entity required.** It is the authorization scope. **Fixed in both
   clients**: `Scope.LegalEntityID`, taken from the connection or the
   destination proposal.
3. **Principal required on tokenize.** The client sent it optionally; the
   vault requires it. **Fixed in both clients.**
4. **Unknown token = 403, not 404.** A token that does not exist is refused
   exactly like another tenant's token. Under RLS the vault cannot tell them
   apart, and a 403/404 split would reveal that some other tenant holds the
   token. **No client change needed:** the clients already map only 403 to
   `ErrDetokenizationDenied`.
5. **Non-UUID tenants are refused (400).** Production tenants are UUIDs. The
   clients fail closed on the 400.

The token is the row's random UUID (36 characters): it fits
banking-connector-svc's `VARCHAR(128)` and never equals the value.

## 7. Key-route store errors — fixed 2026-11-13

**Before (left as-is in the 2026-11-12 pass):**
- `GetKey`, `RotateKey` and `DisableKey` answered **any** store error with
  404, so a database outage read as "key not found";
- `ListKeys` discarded the store error and answered `200` with an empty list.

That was harmless while the store was an in-process map that could not fail.
With Postgres it made an outage look like "no such key", during incident
response, the moment someone is trying to disable a key.

**Now** all four go through `writeKeyStoreErr`:
- `store.ErrKeyNotFound` → 404;
- a non-UUID tenant (`store.ErrInvalidTenant`) → 400;
- **anything else → 503 `service_unavailable`, retryable, logged with the
  operation.**

`MemoryStore` returns the same `ErrKeyNotFound` sentinel as `PgStore`, so
the mapping is identical on both. `openapi.yaml` documents the 503.

**Tests** (`internal/handler/store_errors_test.go`):
- a failing store gives 503 on all four routes;
- a genuine miss is still 404, and a genuinely empty list is still 200
  with `count: 0`;
- against **real Postgres**: the same routes answer 404/200 with the pool
  open, and 503 once the pool is closed;
- a non-UUID tenant on Postgres is 400.

**Breaks caught:**
- `ListKeys` discarding its error again;
- the not-found / store-failure distinction removed.

`RegisterKey` still answers a store failure with 500 ("failed to register
key"). It never claimed "not found", so it was outside this fix.

## 8. Verification (2026-11-12)

**Test convention.** This service's tests had no database and no build tag,
so the Postgres tests follow the sibling `TEST_DATABASE_URL` convention, as
in anomaly-detection-svc. They **skip** without it. With it,
`internal/testpg` gives each test package its own migrated database, reached
through a `NOSUPERUSER NOBYPASSRLS` role.

**Plain `go test ./... -count=1`.** Exit 0. 71 PASS and 14 SKIP: the
Postgres halves of the store and handler tests.

**With `TEST_DATABASE_URL` (embedded Postgres 16).** Exit 0. 87 PASS,
0 SKIP, 0 FAIL.

**Coverage.**
- **Round trip:** exact value back (IBAN, padded, unicode, 1024 bytes); no
  value in any SIEM message; `Cache-Control: no-store`.
- **Cross tenant:** 403, identical to an unknown token; recorded as
  `token.detokenized` under the caller's tenant; authorization not
  consulted.
- **Missing `TOKEN_DETOKENIZE`:** 403 when denied, 503 when authorization
  is unavailable; no value returned; recorded; checked at the token's
  stored legal entity, not the header.
- **Tokenize:** `TOKEN_CREATE` checked; S4, S1, unknown, wrong-case and
  empty classifications refused; idempotent replay and 409.
- **Envelope:** the clients' header set passes write-strict; the three
  proposal headers alone are refused.
- **Store conformance:** `MemoryStore` and `PgStore` both.
- **Postgres-only:** restart durability, forced RLS (including a
  cross-tenant insert refused by policy), non-UUID tenant refused, the
  immutability trigger, and concurrent same-key tokenize creating exactly
  one row.

**Deliberate breaks, each caught.**
- memory vault ignores the tenant;
- `TOKEN_DETOKENIZE` check removed;
- authorizing the caller's legal entity instead of the row's;
- the cross-tenant attempt not recorded;
- unknown token answered differently (existence oracle);
- a replay minting a new token;
- associated data dropped from GCM.

**RLS layering.** Removing `PgStore`'s explicit tenant filter alone did
**not** fail the cross-tenant test: RLS still refused it. Removing the
filter *and* opening the RLS policy did fail it.
