# Spec deviations — banking-connector-svc (BNK-02, BNK-03, BNK-04)

Governing document: `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`
(ZS-SVC-E-001 v1.0), §7 tables 14–17 (BNK-02), 18–21 (BNK-03) and 22–25
(BNK-04); §2 invariants 2, 3, 4, 5, 6, 7 and 8; §4 shared context,
idempotency, errors, external-state, sensitive-data and evidence contract; §5
entities `BankConnection`, `BankConnectionCredentialRef`, `BankStatement`,
`BankStatementSource`, `CanonicalBankTransaction` and
`BankNormalizationVersion`; §6 events; and the §12 negative paths named in
tables 17, 21 and 25. The copy in `docs/architecture/original_doc/` is
byte-identical to the one in `docs/architecture/`.
First survey of this service against the spec (2026-10-22).

**Citation check.** Every BNK-02/03/04 line cited in the review request was
compared against the docx text. The purpose, authoritative ownership,
required/server-resolved inputs, commands, queries, state model, permissions,
segregation-of-duties rule, events, dependencies, failure/degradation
semantics, evidence and the minimum negative-path certification for all three
profiles match the text in tables 14–25. Where the register below says “spec
says”, it is quoting those tables, not an inference from the service.

## Overall

The service has three capabilities layered onto one existing PostgreSQL
service:

- **BNK-02** has a real consent/lifecycle state machine, a token-reference
  field that is never serialized, a vault revoke for revoke and rotation, a
  region-policy table, and all nine explicit commands at some level.
- **BNK-03** has immutable statement lines, a terminal-state trigger, a
  duplicate key, balance validation, quarantine and a narrow reprocess reset.
  It does **not** have the raw source payload/reference, a real import
  watermark, statement-period or parser/schema evidence, or a declared
  completeness population.
- **BNK-04** has immutable canonical versions, `superseded_by` lineage,
  unknown-code routing to a mapping exception, a maker-checker approval, and
  raw date/amount/currency now derived from the immutable statement line. It
  does **not** have a real mapping/schema version snapshot, raw-lineage or
  normalization-version queries, or the spec's canonical quarantined state.

The largest open risks are **A10/A11** (raw source and watermark absent),
**A19/A21** (mapping version and normalization authority), **A1** (read
authorization), and the cross-service BNK-05 watermark mismatch in section C
(C1). The BNK-05 principal mismatch (C2) was closed on 2026-10-24. None of those is a small local patch; they need a source-evidence
table, a versioned mapping registry, and a cross-service auth decision.

Rough coverage after this pass: **BNK-02 ~65%, BNK-03 ~40%, BNK-04 ~55%.**
The percentages are a judgment, not a test count.

Status key: **Done** = implemented as named; **Partial** = route exists but a
listed gap remains; **Missing** = no route or no callable path.

## Command and query coverage

### BNK-02 — Bank Connection

| Spec command / query | Route or callable | Status |
|---|---|---|
| CreateConnectionRequest | `POST /v1/banking/connections/request` (`InitiateConnection`) | Done; account/entity/currency binding added this pass. Retry with the same `correlation_id` does not compare the new payload to the original (A9). |
| CompleteConnectionAuthorization | `POST /v1/banking/connections/{id}/complete-authorization` | Partial; scope-subset and future-expiry guards added. Token lease is not verified against the vault, and no authorization-transaction/provider-response evidence is stored (A5). |
| ActivateConnection | `POST /v1/banking/connections/{id}/activate` | Done; expiry and region guards added at the handler. The store still permits a direct caller to activate without an expiry check (A6). |
| RefreshConnection | `POST /v1/banking/connections/{id}/refresh` | Partial; future-expiry guard added. The old lease is overwritten without a vault revoke (A6). |
| ReconsentConnection | `POST /v1/banking/connections/{id}/reconsent` (`TriggerReconsent`) | Partial; the state transition exists, but there is no new requested-scope input and no independent approval (A4). |
| SuspendConnection | `POST /v1/banking/connections/{id}/suspend` | Done; suspension is now reversible and no longer revokes the vault lease (B2). Health is not changed (A7). |
| RevokeConnection | `POST /v1/banking/connections/{id}/revoke` | Partial; vault revoke precedes the DB write, but there is no retry/outbox and an empty lease is a no-op (A6). |
| ReconnectProvider | `POST /v1/banking/connections/{id}/reconnect` | Done; expired credentials are rejected for DEGRADED/SUSPENDED (A6). RECONSENT_REQUIRED re-enters AUTHORIZING. |
| RotateConnectionCredential | `POST /v1/banking/connections/{id}/rotate-credential` | Partial; old lease is revoked before the DB update, with no cross-system atomicity (A5/A6). |
| GetConnection | `GET /v1/banking/connections/{id}` and `/accounts/{id}` | Done; full projection added. No read authorization (A1). |
| GetConnectionHealth | Missing | — (A2). |
| GetConnectionScopes | Missing | —; `consent_scope` is embedded in GetConnection (A2). |
| GetConsentHistory | `GET /v1/banking/connections/{id}/events` | Partial; requires MANAGE, not read/support, and stores only type/detail/actor, not scopes or provider evidence (A2/A5). |
| ListConnections | `GET /v1/banking/connections` and `/accounts` | Done; full projection added. No read authorization (A1). |
| GetAvailableActions | Missing | — (A2). |

### BNK-03 — Statement Ingestion

| Spec command / query | Route or callable | Status |
|---|---|---|
| IngestStatement | `POST /v1/banking/statements/ingest` | Partial; immutable lines and stable `(connection_id, content_hash)` replay exist. No raw payload/reference; hash is caller-supplied; no watermark/period/parser evidence (A10/A11). |
| ValidateStatement | `POST /v1/banking/statements/{id}/validate` | Partial; balance-only. Does not compare account, currency, period, declared count or control totals (A12). |
| ReprocessQuarantinedStatement | `POST /v1/banking/statements/{id}/reprocess` | Partial; migration 008 allows only `QUARANTINED → RECEIVED` with an empty reason. It does not create a new parser/mapping version or new evidence (A15/B4). |
| SupersedeStatementMapping | Missing | — (A14). |
| CloseStatementImport | Missing | — (A14). |
| QuarantineStatement | `POST /v1/banking/statements/{id}/quarantine` | Partial; status/reason only, no authz or event (A15/A17). |
| GetStatement | Missing | —; `ListStatements` is the nearest read (A14). |
| GetStatementRawReference | Missing | —; no raw payload/reference exists (A10). |
| ListStatements | `GET /v1/banking/connections/{id}/statements` | Done after this pass for status/hash/source/batch/quarantine fields. No read authz (A17). |
| GetImportWatermark | Missing | — (A11/A14). |
| GetIngestionEvidence | Missing | — (A11/A14). |
| ListQuarantinedStatements | Missing | — (A14). |
| (legacy) | `POST /v1/banking/statements` | Not a spec command; creates a header-only `ACCEPTED` row with no lines, hash, watermark or raw source, and ignores `raw_content` (A18). |

### BNK-04 — Transaction Normalization

| Spec command / query | Route or callable | Status |
|---|---|---|
| NormalizeTransaction | `POST /v1/banking/transactions/normalize` | Partial; mapping lookup and raw-line values are real. No statement-acceptance gate and no legal-entity authz (A21/A23). |
| ReNormalizeTransaction | `POST /v1/banking/transactions/{id}/re-normalize` | Partial; supersedes the prior row and preserves it, but the category is arbitrary caller input and the “mapping version” is only a per-line counter (A19/A21). |
| QuarantineTransaction | `POST /v1/banking/transactions/quarantine` | Partial; creates an OPEN mapping exception. It can be called after a canonical row exists and does not create a canonical `QUARANTINED` version (A22/A23). |
| SupersedeNormalization | Missing as a named command | —; `re-normalize` performs a supersession, but there is no mapping-correction/re-performance command (A22). |
| ApproveMappingException | `POST /v1/banking/mapping-exceptions/{id}/approve` | Partial; maker-checker is real. Category/counterparty are arbitrary caller input and the new row gets `mapping_version = 1`, not a mapping-registry version (A19/A21). |
| GetBankTransaction | `GET /v1/banking/canonical-transactions/{id}` | Done, but requires `X-Principal-Id`; BNK-05's client sends only tenant (section C). No read authz beyond the principal check (A21). |
| GetRawLineage | Missing | — (A20). |
| GetNormalizationVersion | Missing | — (A20). |
| ListTransactions | Missing | — (A20). |
| ListNormalizationExceptions | Missing | — (A20). |

## State, events and permissions

| Area | Implemented | Spec requirement / gap |
|---|---|---|
| BNK-02 state | `REQUESTED`, `AUTHORIZING`, `ACTIVE`, `SUSPENDED`, `RECONSENT_REQUIRED`, `REVOKED`; legacy `CONNECTED`/`DISCONNECTED`; health status separate. | Table 16: `Requested → Authorizing → Active → Degraded/Suspended → ReconsentRequired → Revoked`; token/credential versions separate. `DEGRADED` status is defined but no command sets it, and health can remain `HEALTHY` while `SUSPENDED` (A7). |
| BNK-03 state | `RECEIVED → VALIDATING → ACCEPTED/QUARANTINED`; `REJECTED` defined but never produced; migration 008 allows `QUARANTINED → RECEIVED`. | Table 20: Received → Validating → Accepted/Quarantined/Rejected; accepted raw evidence immutable; mapping/parser correction produces a superseding processed version. No superseding processed version and no rejected path (A15). |
| BNK-04 state | Canonical `NORMALIZED`/`SUPERSEDED`; quarantine is a separate `mapping_exceptions` row. | Table 24: RawCaptured → Normalized/Quarantined; canonical versions immutable; mapping correction creates a superseding version. No canonical `QUARANTINED` state, no mapping-correction command (A22). |
| BNK-02 events | Outbox: `bank-connection.requested`; legacy `bank-connection.created`. Internal `bank_connection_events` for the rest. | Table 16: Requested, Activated, Degraded, ReconsentRequired, Revoked, CredentialRotated. Five of six spec events never reach the outbox; Degraded is never emitted because no status path sets it (A3). |
| BNK-03 events | Outbox: `bank-statement.ingested`. | Table 20: Received, Accepted, Quarantined, Rejected, Superseded. Only one of five is emitted; none for quarantine/reprocess (A16). |
| BNK-04 events | Outbox: `banking.transaction.normalized`, `banking.transaction.renormalized`, `banking.mapping_exception.raised`, `banking.mapping_exception.approved`. | Table 24: Normalized, Quarantined, NormalizationSuperseded. Normalized exists; the other two spec facts are represented by implementation-specific events or not emitted (A22). |
| BNK-02 permissions | `BANKING_CONNECTION_CREATE`, `_MANAGE`, `_REVOKE`. | Table 16: `bank.connection.read/manage`, `authorize`, `revoke`, `support`. No read, authorize or support action; none of the strings is registered in the access-control taxonomy (A1/A4). |
| BNK-03 permissions | `BANKING_STATEMENT_INGEST` is checked. `BANKING_STATEMENT_VALIDATE` is defined but never checked. | Table 20: `statement.read`, `statement.ingest`, `statement.quarantine.manage`, `statement.raw.read`. Validate/accept/quarantine/reprocess check only a principal; ListStatements checks nothing (A17). |
| BNK-04 permissions | `BANKING_MAPPING_EXCEPTION_APPROVE` and `BANKING_MAPPING_MANAGE` are checked platform-wide. `BANKING_TRANSACTION_NORMALIZE` is defined but never checked. | Table 24: `bank.transaction.read`, `bank.transaction.normalize`, `mapping.exception.approve`. Normalize/re-normalize/quarantine check only a principal; read checks only principal; none registered in taxonomy (A21). |

## A. Deviations and gaps not fixed in this pass

### BNK-02 — Bank Connection

- **A1. Read authorization is absent on BNK-02 reads.** `GetConnection` and
  `ListConnections` are tenant-scoped only; they never call
  `CheckAllowed`, and `BankConnection.TokenLeaseRef` is `json:"-"`, so no raw
  token is exposed, but account ids, provider refs, scopes, health, region
  and expiry are visible to any principal that can reach the service.
  `GetConsentHistory` requires `BANKING_CONNECTION_MANAGE` rather than a
  read/support action, so support cannot read it and an ordinary reader
  cannot use the query at all. Table 16 names `bank.connection.read/manage`,
  `bank.connection.authorize`, `bank.connection.revoke` and
  `bank.connection.support`; only manage/revoke/create exist, none is
  registered in the access-control taxonomy, and there is no read or support
  distinction. This is the same gap BNK-05 A10 records for reconciliation
  reads.
- **A2. Three BNK-02 queries are missing and the fourth is thin.**
  `GetConnectionHealth`, `GetConnectionScopes` and `GetAvailableActions` have
  no route (table 15). Health and scopes are embedded in `GetConnection`,
  which is not the same contract: there is no freshness/health transition
  view and no action map for a caller that must decide whether to activate,
  reconnect or revoke. `GetConsentHistory` exists as
  `/connections/{id}/events`, but it returns only
  `event_id/connection_id/tenant_id/event_type/detail/actor/created_at`;
  detail is empty for most transitions and there is no scope grant, consent
  version, authorization transaction or provider response (A5).
- **A3. BNK-02 events are mostly internal audit rows, not outbox facts.**
  `InitiateConnection` emits `bank-connection.requested`; the legacy
  `CreateConnection` emits `bank-connection.created`. Complete, activate,
  refresh, reconsent, suspend, revoke, reconnect and rotate only insert into
  `bank_connection_events`, which is append-only but is not the §6 outbox and
  is not published to consumers. Table 16 requires six events
  (`BankConnectionRequested`, `Activated`, `Degraded`, `ReconsentRequired`,
  `Revoked`, `CredentialRotated`). Five are absent from the outbox, and
  `BankConnectionDegraded` has no state transition that would produce it.
  A consumer such as BNK-08 cannot learn that consent was revoked or a
  credential rotated except by polling.
- **A4. Scope expansion has a subset check but no independent approval.**
  The handler now rejects a granted scope outside the connection's recorded
  scope set (`domain.ScopeSubset`), which closes the “Scope expands without
  reauthorization” negative path at the first authorization. But the spec's
  SoD line also says “Connection administrator cannot self-approve elevated
  scope expansion where policy requires” (table 16). There is no elevated
  scope/approval policy, no second principal, and no
  `bank.connection.authorize` action. A principal who can initiate can also
  complete the authorization; the check proves the grant is a subset of what
  was requested, not that a second human approved a sensitive scope.
  `consent_scope` is also overwritten with the granted set, so a scope
  requested at initiation but not granted is lost; the second reconsent cycle
  is therefore conservative (B3). Persisting `requested_scope` separately
  and adding an approval step are the next design decisions.
- **A5. Credential references have no version chain or authorization
  evidence.** The spec's authoritative ownership includes “token/secret
  reference; authorization method; consent expiry; connection health;
  reconnect/revocation history” (table 15) and §5 names a separate
  `BankConnectionCredentialRef`. This service stores a single mutable
  `token_lease_ref` and `token_expires_at` on `bank_connections`; Complete,
  Refresh and Rotate overwrite them. There is no credential-version row, no
  authorization transaction id, no provider response, no certificate/key
  reference, and no proof that a caller-supplied lease belongs to this tenant
  or connection. The vault client can revoke a lease by id, but it cannot
  validate a new one. This fails invariant 3/4 (“tokens/credentials ...
  referenced from business records”; “consent/scopes/expiry/revocation are
  versioned and enforceable”) and table 17's evidence line. It also makes
  “revocation must propagate and be verifiable” weaker than the spec.
- **A6. Credential lifecycle has non-atomic and unverified edges.**
  `RefreshConnection` replaces `token_lease_ref` without revoking the old
  lease; the old lease can remain live until expiry and is no longer
  reachable by the service. `RotateConnectionCredential` revokes the old
  lease in the vault and then writes the DB; if the DB write fails, the
  connection points at a revoked lease. `RevokeConnection` does the vault
  call before the DB write and returns 503 on vault failure with no durable
  retry/outbox; that is fail-closed for the API but not a verifiable
  propagation record. The store-level `ActivateConnection` only requires a
  non-empty lease, not a future expiry; the handler now checks expiry, but a
  direct/in-process caller can bypass it. These are table 16/17
  failure-and-evidence gaps, not small route fixes.
- **A7. Region enforcement is opt-in, provider approval is not modeled, and
  `DEGRADED` is unreachable.** `bank_region_policies` allows a region per
  legal entity, but a legal entity with zero rows is unrestricted; the
  service logs a warning and proceeds. Table 16 says “provider-region
  conflict blocks activation” and table 15 lists provider approval and
  region/residency as server-resolved context. There is no provider approval
  record and no default/deny-by-absence region policy. The status constant
  `DEGRADED` exists, but no command transitions to it; `TriggerReconsent`
  goes straight to `RECONSENT_REQUIRED` and sets health `DEGRADED`.
  `SuspendConnection` changes status but leaves health unchanged. The state
  model therefore has states the UI can name but the service cannot enter,
  and a suspended connection can still report `HEALTHY`.
- **A8. Legacy connection routes bypass the BNK-02 lifecycle.** `POST
  /v1/banking/connections` (and the `/accounts` alias) still creates a
  `CONNECTED` row with only the original nine columns: no `bank_account_id`,
  no BNK-01 verification, no requested/consent scope, no token reference,
  no expiry, no region policy, no correlation id. It is authorized with
  `BANKING_CONNECTION_CREATE`, but it does not satisfy table 15's required
  inputs or table 16's state model. Treasury-svc's `ListConnectionOptions`
  filters on `bank_account_id`, so these legacy rows are invisible to BNK-01
  even after the projection fix. This is a public compatibility route, so it
  was documented rather than removed; the safe next step is to delegate it
  to `InitiateConnection` or return a deprecation error.
- **A9. Idempotent initiate replay does not bind the request payload.**
  `InitiateConnection` deduplicates on `(tenant_id, correlation_id)` and
  returns the original row, but it does not compare bank account, legal
  entity, bank name, provider ref or requested scopes. A retry with a
  different payload silently returns the old connection; a retry after the
  account is no longer verifiable fails before reaching the idempotency
  lookup. The shared idempotency contract (table 7) says duplicate calls
  return the original stable result; it does not authorize a changed request to be
  treated as the original. A stored request fingerprint or a 409 on
  mismatch is needed.

### BNK-03 — Statement Ingestion

- **A10. No raw source/reference is stored; `content_hash` is caller-supplied.**
  BNK-03's authoritative ownership is `BankStatement; raw source
  reference/hash; provider/file source ID; statement period; opening/closing
  balances; transaction population; import watermark;
  ingestion/rejection/quarantine evidence` (table 19). The schema has a
  statement header and immutable lines, but no `BankStatementSource`, no raw
  payload, no raw file reference, no declared period, and no computed hash.
  `content_hash` is whatever the caller sends; it is not SHA-256 of the
  bytes, and the legacy `IngestStatementRequest.RawContent` field is accepted
  and discarded. This fails invariant 5 (“Accepted bank statements/raw
  provider evidence are immutable or verifiably referenced by cryptographic
  hash”) and the §5 `BankStatementSource` entity. The negative-path
  certification “Duplicate statement imported twice” and “Statement for
  wrong account accepted” cannot be proven from stored evidence alone
  (A11/A12).
- **A11. No import watermark, period, parser/schema version or ingestion
  evidence query.** `GetImportWatermark`, `GetStatement`,
  `GetStatementRawReference`, `GetIngestionEvidence` and
  `ListQuarantinedStatements` are absent (table 19). There is no
  `import_watermark` column, no statement-period start/end, no parser/schema
  version, no file security-scan result, and no control-total/row-count
  evidence beyond `transaction_count = len(lines)`. Invariant 6 (“Statement
  ingestion is idempotent and maintains a source/import watermark and
  completeness controls”) is not met. BNK-05's A12/B7 note that its
  `source_watermark` is a local placeholder and that BNK-03 should own the
  real watermark is correct: this service does not own one yet.
- **A12. Completeness is not checked, only the balance equation.**
  `ValidateStatement` reads `opening_balance`, `closing_balance` and
  `SUM(lines.amount)`, and quarantines on a mismatch. It does not compare a
  declared transaction count, account, currency, statement period or control
  totals; the API does not accept a declared population separate from the
  lines it receives. A truncated file that happens to balance is accepted as
  complete. `IngestStatement` also accepts an empty format, zero statement
  date, zero line dates, zero/negative amounts, empty currencies and mixed
  currencies; `transaction_count` is simply the number of supplied lines.
  Table 18 requires “reproducible account/period/balance/transaction
  completeness”; table 20 says “partial import cannot masquerade as
  complete”; table 21 lists “Partial file marked complete”. None of those is
  enforced. The store's `connection_id` foreign key and the HTTP
  `GetConnectionByID` check stop a random connection id, but there is no
  check that the connection's BNK-01 account is still active or that line
  currencies match the account.
- **A13. Duplicate/replay semantics depend on a caller-controlled hash.**
  The unique index is `(connection_id, content_hash) WHERE content_hash <> ''`.
  A re-upload with the same hash returns the original rows; a re-upload of
  the same bytes with a different hash creates a second statement; a
  re-upload with the same hash but different lines returns the original
  lines without a conflict. An empty hash bypasses dedupe entirely. The
  legacy `POST /v1/banking/statements` inserts `content_hash = ''` and status
  `ACCEPTED` with no lines, so it is a duplicate/completeness bypass as well
  as a lifecycle bypass (A18). Table 20 says “Duplicate/replayed source is
  idempotently rejected/replayed to stable result”; table 21's negative path
  is “Duplicate statement imported twice”. Stable *by caller hash* is not
  the same as duplicate detection over source bytes.
- **A14. BNK-03 commands and queries are missing.**
  `SupersedeStatementMapping` and `CloseStatementImport` have no route;
  `GetStatement`, `GetStatementRawReference`, `GetImportWatermark`,
  `GetIngestionEvidence` and `ListQuarantinedStatements` have no route
  (table 19). `ListStatements` is a connection-scoped legacy route, not a
  statement-by-id read, and it returns no raw reference. This is a
  query/command coverage gap, not a naming difference.
- **A15. The `REJECTED` state is never produced and reprocess is a loop.**
  `StatementRejected` and the state model exist, but no command or handler
  sets `REJECTED`. `ReprocessQuarantinedStatement` resets the same immutable
  lines to `RECEIVED`; it cannot add a corrected line population, change a
  parser version, or supersede a prior processed statement. A balance
  mismatch caused by the original lines will simply quarantine again. Table
  20 requires “mapping/parser correction produces superseding processed
  version”; the service has no such version. The reprocess reset also emits
  no event and records no actor/reason/evidence (table 21).
- **A16. BNK-03 events are one of five.** Only `bank-statement.ingested`
  reaches the outbox. `BankStatementReceived`, `BankStatementAccepted`,
  `BankStatementQuarantined`, `BankStatementRejected` and
  `BankStatementSuperseded` are absent (table 20). Quarantine and reprocess
  produce no outbox event at all, so a consumer cannot observe the state that
  matters for close/reconciliation.
- **A17. Statement control actions have no legal-entity authorization.**
  Validate, accept, quarantine and reprocess call `requirePrincipal` only;
  they do not resolve the statement's connection/legal entity or check
  `BANKING_STATEMENT_VALIDATE`. The constant is defined but never used.
  `ListStatements` also has no read authorization and returns the raw-source
  hash, source/import-batch ids and creator principal to any tenant
  principal. Table 20 names `statement.read`, `statement.ingest`,
  `statement.quarantine.manage` and `statement.raw.read`; only ingest is
  checked. This is the same pattern as BNK-05's A10/A12 and is a real
  control gap, not a deployment preference.
- **A18. The legacy statement route is a lifecycle bypass.** `POST
  /v1/banking/statements` requires ingest authz and a verifiable connection,
  then calls `RecordStatement`, which inserts status `ACCEPTED` with
  `transaction_count` from the caller, no lines, no hash, no source id, no
  batch id and no raw content. `raw_content` in the request is ignored. A
  caller can therefore create an accepted, immutable header with zero
  evidence. This violates table 19's inputs and table 20's accepted-evidence
  rule. Like the legacy connection route, removing or delegating it is a
  deployment/contract decision; it is not fixed here.

### BNK-04 — Transaction Normalization

- **A19. Mapping version is not a mapping/schema version.** Table 23 makes
  “mapping version; supersession chain” authoritative; table 24 says
  “mapping correction creates superseding version”; table 25 requires
  “canonical version, mapping/version, timezone/sign decisions”. The
  service's `mapping_version` is an integer incremented on each
  re-normalization of a line (default 1), not an identifier/version of the
  mapping dictionary entry or parser/schema. `bank_transaction_mappings` has
  no version column, no update path, no immutability trigger and no
  `BankNormalizationVersion` table. `ApproveMappingException` inserts
  `mapping_version = 1`; `ReNormalizeTransaction` inserts `prior + 1` with a
  caller-supplied category. A later reader cannot reproduce which mapping
  produced a canonical row. This fails invariant 7 and the negative path
  “Mapping version not retained”.
- **A20. Raw-lineage, normalization-version and list queries are missing.**
  `GetRawLineage`, `GetNormalizationVersion`, `ListTransactions` and
  `ListNormalizationExceptions` have no route (table 23). Only
  `GetBankTransaction` exists. The canonical response has
  `statement_line_id` and `superseded_by`, but no raw hash, no mapping
  registry version, no exception list and no way to enumerate a tenant's
  transactions. BNK-05 cannot call a lineage/version query because there is
  none.
- **A21. Normalization has no legal-entity authorization or independent
  mapping control.** Normalize, re-normalize and quarantine only require a
  principal; `BANKING_TRANSACTION_NORMALIZE` is defined but never checked.
  The handler binds the tenant from the header, but no legal entity, account
  or statement ownership is checked. A principal with no banking grant can
  normalize any line in the tenant. `CreateTransactionMapping` and
  `ApproveMappingException` are platform-scoped (`legal_entity_id = ""`);
  there is no check that the mapping author is different from the approver of
  a material mapping change, and `ReNormalizeTransaction` accepts any
  category string with no approval. Table 24's SoD line says “Mapping author
  cannot self-approve material mapping changes affecting financial
  interpretation where policy requires”; the exception path has
  maker-checker, but the mapping/re-normalization path does not.
- **A22. No canonical `QUARANTINED` state and no normalization-superseded
  event.** Table 24's state is `RawCaptured → Normalized / Quarantined`;
  canonical versions are immutable; mapping correction creates a superseding
  version. In this service an unknown code creates a `mapping_exceptions`
  row, not a canonical row with status `QUARANTINED`; there is no
  `SupersedeNormalization` command; and the outbox emits
  `banking.transaction.renormalized`, not
  `BankTransactionNormalizationSuperseded`. `BankTransactionQuarantined` is
  not emitted at all. A consumer that maps spec event names will not see the
  spec facts.
- **A23. State/precondition bugs around normalization and exceptions.** A
  second `NormalizeTransaction` on an already-NORMALIZED line hits the
  active-line unique index and surfaces as a generic 500, not a stable
  original result or `ErrInvalidTransactionTransition`. `QuarantineTransaction`
  inserts an OPEN exception without checking whether a canonical row already
  exists; approving that exception then inserts a second active canonical
  row and fails on the unique index (again a generic 500). Neither
  normalization nor approval checks that the owning statement is
  `ACCEPTED`; a line from a `RECEIVED` or `QUARANTINED` statement can be
  normalized. These are table 20/24 failure-semantics and state-model gaps,
  and they are reachable through the public routes.
- **A24. Canonical evidence is better than before but still incomplete.**
  This pass fixed the worst case: canonical `transaction_date`, `amount` and
  `currency` are now read from the immutable `bank_statement_lines` row, so
  a caller can no longer assert a different amount/date/currency. Caller
  assertions are optional and a mismatch returns 422. But `counterparty`
  remains caller-supplied enrichment, and the canonical row stores no raw
  line hash, no mapping-entry id/version, no timezone/sign decision and no
  exception evidence (table 25). The raw line itself is immutable, which is
  the right base; the lineage above it is not reproducible.
- **A25. Re-normalization has no downstream re-performance signal.**
  Table 24 says mapping correction “may trigger reconciliation
  re-performance”; table 23 makes the supersession chain authoritative.
  `ReNormalizeTransaction` updates the old row to `SUPERSEDED` and inserts
  the new one, but emits only `banking.transaction.renormalized`. There is no
  event for BNK-05, no `superseded_at`/reason, and no record that an existing
  reconciliation match used the prior canonical version. A correction can
  leave a certified BNK-05 match pointing at the old fact with no signal to
  re-run.
- **A26. Money is `float64` in the domain/API.** The database stores
  `NUMERIC(18,4)`, but `StatementLine`, `CanonicalTransaction`,
  `BankStatement`, the request types and BNK-05's client all use `float64`.
  The balance check uses a half-cent epsilon. This is deterministic enough
  for the current tests, but it is not exact decimal arithmetic, and the
  spec's “control totals”/“deterministic/replayable” NFRs (table 21/25) are
  better served by integer minor units or a decimal type. This is a
  cross-cutting decision, not a one-file patch.

## B. Bounded decisions and what this pass changed

- **B1. BNK-02 projection and account binding.** `GetConnectionByID` and
  `ListConnections` now select `bnk02Columns` and use `scanConnection`, so
  `bank_account_id`, `provider_ref`, `consent_scope`, `token_lease_ref`,
  token expiry, health and region round-trip. `InitiateConnection` now
  verifies the BNK-01 account through `clients.BankAccountClient`, rejects a
  cross-entity account with 422, rejects an empty currency with 424, and
  stores the verified account currency instead of hardcoded USD. This also
  fixes treasury-svc's `ListConnectionOptions`, which previously filtered on
  a `bank_account_id` that the partial projection never returned.
- **B2. BNK-02 scope/expiry/suspend guards.** `domain.ScopeSubset` rejects a
  granted scope outside the recorded set on complete-authorization;
  `requireNewCredentialExpiry` rejects missing/past expiry on complete,
  refresh and rotate; `requireUsableCredential` rejects expired activation
  and reconnect from DEGRADED/SUSPENDED; `SuspendConnection` no longer calls
  the vault, because suspension is reversible and `ReconnectProvider` needs
  the lease.
- **B3. Scope check limitation.** `consent_scope` is seeded from requested
  scopes at initiation and then overwritten with the granted set. There is no
  separate `requested_scope` column, so after a partial grant a second
  reconsent cycle cannot re-add a scope that was originally requested but
  never granted unless it is still in the recorded set. The check is
  deliberately conservative; fixing it properly means persisting
  `requested_scope` (or a scope-version table) and deciding whether
  reconsent may widen the original request. This is called out at the
  handler and in A4.
- **B4. BNK-03 reprocess trigger exception.** Migration 008 replaces
  `reject_terminal_statement_mutation` with a rule that permits only
  `QUARANTINED → RECEIVED` with an empty `quarantine_reason` and every other
  header column unchanged. `ACCEPTED` and `REJECTED` remain terminal. The
  down migration restores migration 004 exactly.
- **B5. BNK-03 projection and batch insert.** `statementColumns` and
  `scanStatement` now include `content_hash`, `source_id`, `import_batch_id`,
  `status`, `quarantine_reason` and `created_by_principal_id`, and
  `ListStatements` returns them. `IngestStatement` replaces the per-line
  `INSERT ... RETURNING` loop with one set-based
  `INSERT ... SELECT FROM unnest(...)`, removing the N+1 network round trip.
  The replay path reuses the same statement and line population.
- **B6. BNK-04 raw values.** Normalize, re-normalize and approve now load
  the immutable statement line and always insert its date/amount/currency.
  `TransactionDate`/`Amount` became optional pointers; `Currency` is an
  optional assertion. A mismatch returns `ErrTransactionEvidenceMismatch`
  and HTTP 422. Tests cover both normalize and approval, including a caller
  who lies and a caller who omits the fields.
- **B7. Legacy routes remain.** The `/v1/banking/connections` and
  `/v1/banking/statements` compatibility paths were not removed or silently
  changed. They are documented in A8/A18 because removing a public route is
  a deployment/contract decision, and the BNK-03 legacy body has no lines to
  forward.
- **B8. Test harness can use a caller-supplied disposable database.** The
  embedded-Postgres `TestMain` now honors an existing `TEST_DATABASE_URL`
  instead of always starting its own instance. The full integration suite
  was run against a fresh `postgres:16-alpine` container; see section D.
- **B9. No event-taxonomy or access-control taxonomy changes.** The action
  strings are still service-local. The outbox event names are still
  implementation names (`banking.transaction.normalized`) rather than the
  spec's CamelCase facts. Changing either is a cross-service contract
  change, not a local fix.

## C. Cross-service contradictions

### C1. BNK-05 assumes a BNK-03 import watermark and source identity that do not exist

`services/bank-reconciliation-svc/SPEC_DEVIATIONS.md` A12 says its
`statement.source_watermark` is “this service's `max(created_at)`, not
BNK-03's import watermark”, and B7 says “Until BNK-03 integration (A12), the
population's `source_watermark` is `max(created_at)` ... compare the wrong
clock.” That assumption is correct: this service has no
`import_watermark`, no source-file reference, no statement period, no
parser/schema version and no source row count. BNK-05 cannot complete A12 or
B7 against the current BNK-03 implementation. The `content_hash` it could
read is caller-supplied, not a hash of source bytes, so it is not a
substitute. **Do not treat BNK-05's watermark as integrated.**

### C2. Closed (2026-10-24) — BNK-05's canonical-read client did not satisfy this service's principal check

**Finding (2026-10-22).** `services/bank-reconciliation-svc/internal/banking/client.go` sent only
`X-Tenant-Id` to `GET /v1/banking/canonical-transactions/{id}`. This service's
handler calls `requirePrincipal`, which requires `X-Principal-Id`, before
looking up the transaction (`internal/handler/bnk0304_handler.go`, the
`GetCanonicalTransaction` handler). An internal service call that does not
pass through a gateway that injects the header will receive 401, and BNK-05
will map that to `ErrBankingConnectorUnavailable`; every canonical match
verification fails closed. This is a concrete contradiction between the
BNK-05 survey's assumption that the canonical read is usable and this
service's read guard. The fix is a cross-service decision: pass the
originating principal through BNK-05's client, or introduce an explicit
service-to-service identity that this service accepts. Do not remove the
principal requirement just to make the call succeed.

**Resolution.** The first option: BNK-05 now forwards the originating
principal, which every one of its four canonical-verification entry points
(`MatchStatementLine`, `ProposeMatch`, `ConfirmMatch`, `RunAutomaticMatching`)
had already resolved through `requirePrincipal` before calling
`verifyCanonicalMatch`. The defect was entirely on the client side; this
service's handler and its principal requirement are unchanged.

- `banking.Client.GetCanonicalTransaction` takes `principalID` as a required
  argument and fails closed, without sending, when it is empty.
- The client sends the §4 envelope with the vendored `envelope` header
  constants, the same set `envelope.Resolver` sends on its own outbound call.
  That set is tenant, principal, correlation id and request id (propagated
  from the inbound envelope) and `X-Source-Channel: system`. Sending only the
  principal would have fixed today's 401 but left the call refused with 400
  `envelope_incomplete` once this service runs `ZS_ENVELOPE_ENFORCEMENT=strict`.
  The cross-process run below confirmed this.
- `ConfirmMatch` forwards the **checker's** principal, not the proposer's,
  because the re-verification is a read made on the checker's behalf.

Tests:

- **This service:** `internal/store/canonical_read_contract_test.go`
  (`TestCanonicalTransactionRead_CrossServiceContract`) runs the real handler,
  the `cmd/server` middleware stack and a real Postgres row under RLS. It
  asserts 401 with no principal, and 200 for exactly the reconciliation
  client's header set under both `write-strict` and `strict`. It also asserts
  that strict refuses a bare tenant+principal read with 400.
- **bank-reconciliation-svc:** `internal/handler/canonical_principal_test.go`
  covers all four entry points.
- **Cross-process:** `internal/handler/connector_e2e_test.go` (integration
  tag) drives the real BNK-05 handler and client against a running
  banking-connector-svc binary. It failed before the fix (401 → 503
  `banking_connector_unavailable`) and passes after.

**Observed while verifying, not fixed (out of C2's scope).**

- `GET /healthz` on this service answers 401, because `middleware.TenantContext`
  runs before the health route and refuses a request with no `X-Tenant-Id`.
  The envelope middleware exempts the probe paths, but the tenant middleware
  does not, so an orchestrator liveness probe without a tenant header fails.
- C4's treasury client still sends only `X-Tenant-Id`. That works today only
  because `ListConnections` is unauthenticated (A1), and becomes this same
  defect the moment read authz is added.

### C3. BNK-05 assumes mapping-version reproducibility and a re-performance signal

BNK-05's canonical client decodes `MappingVersion`, and its survey relies on
BNK-03/04's canonical fact. This service's `mapping_version` is only a
per-line re-normalization counter (A19); there is no mapping-registry
version, no `BankNormalizationVersion` table, no `GetNormalizationVersion`
query, and no `BankTransactionNormalizationSuperseded` event (A20/A22/A25).
BNK-05 can read the current canonical amount/currency (now derived from the
raw line) but cannot prove which mapping produced it or learn that a
correction superseded a fact it already matched. A mapping correction can
leave a BNK-05 match/certificate pointing at the old canonical version with
no re-performance trigger. This is a real cross-service gap, not just a
BNK-04 query omission.

### C4. BNK-08/BNK-01's `ListConnectionOptions` claim was false before this pass and is only partly true now

`services/treasury-svc/SPEC_DEVIATIONS.md` marks BNK-01's
`ListConnectionOptions` as “Done — live from banking-connector-svc, fails
closed.” Before this pass that was not true: `GetConnectionByID` and
`ListConnections` selected only the original nine columns, so
`bank_account_id` was always empty and the treasury client's account filter
returned no options. The full projection is fixed now, so BNK-02-created
connections appear. Two caveats remain: legacy `CONNECTED` rows have no
`bank_account_id` and are still invisible, and `ListConnections` has no read
authorization (A1). Treasury's client also sends only `X-Tenant-Id`; it
works today only because this read is tenant-scoped and unauthenticated. If
BNK-02 read authz is added, treasury's client must carry a principal or a
service identity.

### C5. BNK-08's cash-position source assumption remains true

`services/treasury-svc/SPEC_DEVIATIONS.md` A9 says “Nothing ingests BNK-03
statement balances or BNK-07 settlement movements,” and BNK-08's cash
position is built from a zero placeholder. This service does not contradict
that statement: BNK-03 emits only `bank-statement.ingested` (A16), and the
event does not represent an accepted statement or a balance feed. There is
no `BankStatementAccepted` event and no consumer in treasury-svc. BNK-08
cannot close A9 by consuming the current BNK-03 event set; a new accepted/
balance event or a synchronous read of accepted statements is required.

### C6. BNK-06

`services/payment-initiation-adapter-svc/SPEC_DEVIATIONS.md` makes no
BNK-02/03/04 dependency claim; its bank-facing path is the provider adapter
through BNK-09/treasury, not this service. No direct contradiction was found.
The future risk is that BNK-06's payment attempts do not consult BNK-02's
connection status: a REVOKED or SUSPENDED connection does not itself block a
payment attempt. That gate belongs to BNK-09/payment authorization today, so
the absence is not a BNK-06 spec violation, but the two profiles should not
be wired together without the credential-version/authorization evidence in
A5.

### C7. Adjacent consumers that depend on the canonical fact

`services/accounts-receivable-svc/internal/ar08/source.go` treats a positive
canonical amount as money received and only accepts `NORMALIZED` as final.
This pass makes the canonical date/amount/currency derive from the raw line,
which is the right direction. The remaining AR-08 risk is the same mapping/
lineage gap as C3: it cannot verify the mapping version or raw hash behind
the canonical row. This is not a BNK-02/03/04 contradiction, but it is the
same missing evidence chain.

## D. Verification performed

- `gofmt -w` on the changed Go files.
- `go build ./...` — pass.
- `go test -count=1 ./...` (unit/handler/store, non-integration) — pass.
- Full integration suite against a fresh disposable `postgres:16-alpine`
  container named `banking-it-20261022` on `127.0.0.1:55446`, database
  `banking_it`, with `TEST_DATABASE_URL` set explicitly:
  `go test -tags=integration -count=1 -timeout=600s ./...` — pass.
- Targeted integration tests:
  - `TestBNK02_ConnectionQueries_ReturnFullProjection`
  - `TestBNK03_ReprocessQuarantinedStatement_AllowsOnlyNarrowReset`
  - `TestBNK03_ListStatements_ReturnsIngestionEvidence`
  - `TestBNK03_IngestStatement_BatchPersistsEveryLine`
  - `TestBNK04_NormalizeTransaction_UsesRawEvidenceAndRejectsMismatch`
  - `TestBNK04_ApproveMappingException_UsesRawEvidence`
  — all pass.
- New BNK-01 client tests:
  - `TestBankAccountHTTPClient_VerifyBankAccount_DecodesTreasuryDetail`
  - `TestBankAccountHTTPClient_VerifyBankAccount_RejectsTenantMismatch`
  - `TestBankAccountHTTPClient_VerifyBankAccount_FailsClosedOnNon200`
  — all pass.
- New handler tests:
  - `TestBNK02_InitiateConnection_BindsVerifiedLegalEntityAndCurrency`
  - `TestBNK02_InitiateConnection_RejectsAccountFromAnotherLegalEntity`
  - `TestBNK02_CompleteAuthorization_RejectsScopeExpansion`
  - `TestBNK02_CompleteAuthorization_RejectsExpiredToken`
  - `TestBNK02_ActivateConnection_RejectsExpiredCredential`
  - `TestBNK02_SuspendConnection_DoesNotRevokeReversibleCredential`
  - `TestBNK02_ReconnectConnection_RejectsExpiredCredential`
  — all pass.

The integration harness’s original embedded-Postgres path was also exercised
before the container run; both are disposable. No shared development
database was used.

### C2 fix verification (2026-10-24)

- **Database:** a fresh disposable `postgres:16-alpine` container
  (`c2-contract-pg`, `127.0.0.1:55433`). It held three databases:
  `banking_test` for this suite, `recon_test` for bank-reconciliation-svc and
  `connector_e2e` for the cross-process run. No shared development database
  was used.
- **Build and vet:** `go build ./...`, `go vet ./...` and
  `go vet -tags=integration ./...` all pass.
- **Test suites:** `go test -count=1 -p 1 ./...` and
  `go test -tags=integration -count=1 -p 1 ./...` both pass with
  `TEST_DATABASE_URL` set. The integration run had 79 passes and no skips.
- **Migrations for the cross-process run:** applied file by file with
  `psql --single-transaction -v ON_ERROR_STOP=1`.
- **Connector binary:** connected as a `NOSUPERUSER NOBYPASSRLS` role and was
  run twice, once in the default `write-strict` mode and once with
  `ZS_ENVELOPE_ENFORCEMENT=strict`. bank-reconciliation-svc's
  `TestCanonicalMatch_AgainstRealBankingConnector` passed against both.
- **Negative control:** with the client's `X-Principal-Id` line removed, the
  same test failed with `status 401: {"error":"X-Principal-Id header is
  required"}` and the match returned 503.

## E. Priority for the next pass

1. **P0 — BNK-03 source evidence.** Add a `BankStatementSource`-shaped table
   with raw payload/reference, computed SHA-256, source id, batch id, period,
   parser/schema version, row count/control totals and watermark; compute
   the hash server-side; use the source id/batch id in idempotency; add
   `GetStatementRawReference`, `GetIngestionEvidence` and
   `GetImportWatermark`. This unblocks BNK-05 A12/B7 and the raw-evidence
   negative paths.
2. **P0 — BNK-05 canonical lineage.** The principal half is done (C2,
   closed 2026-10-24). Remaining: add `mapping_version`/normalization
   version lineage and a superseded event/re-performance signal (C3).
3. **P0 — BNK-04 mapping registry.** Introduce immutable mapping versions or
   a `BankNormalizationVersion` row per canonical transaction; store the
   mapping id/version, parser/schema version and raw hash; make
   re-normalization and approval reproduce the same snapshot.
4. **P1 — authorization coverage.** Add read actions, legal-entity checks on
   BNK-03/04 commands, `statement.quarantine.manage`, `statement.raw.read`,
   `bank.transaction.normalize` and `bank.transaction.read`, and register the
   action strings in the access-control taxonomy.
5. **P1 — lifecycle/events.** Emit the spec event names for BNK-02
   activated/degraded/reconsent/revoked/rotated, BNK-03 accepted/quarantined/
   rejected/superseded, and BNK-04 quarantined/normalization-superseded.
   Add the missing queries.
6. **P1 — legacy bypasses.** Delegate or deprecate `POST
   /v1/banking/connections` and `POST /v1/banking/statements`; do not leave a
   header-only `ACCEPTED` path or a consent-free `CONNECTED` path.
7. **P2 — money/format.** Move money to integer minor units or a decimal
   type; add declared row counts/control totals and period validation; add a
   canonical `QUARANTINED` state if the product wants the spec state exactly.

## F. Files changed in this pass

C2 fix (2026-10-24): `internal/store/canonical_read_contract_test.go`
(new) and this register. No production code in this service changed.

Original survey (2026-10-22):

- `internal/clients/bankaccount.go` — BNK-01 currency/entity fields and doc.
- `internal/clients/bankaccount_test.go` — treasury response-shape regression
  test for the account binding.
- `internal/clients/vault.go` — suspension no longer revokes the lease.
- `internal/domain/service.go` — currency, `ScopeSubset`, statement evidence
  fields.
- `internal/domain/bnk0304.go` — raw-evidence assertion types and optional
  date/amount/currency.
- `internal/handler/bnk02_handler.go` — entity/currency binding, scope
  subset, expiry and suspend/reconnect guards.
- `internal/handler/bnk0304_handler.go` — evidence-mismatch HTTP mapping.
- `internal/store/bnk02_store.go` — verified currency insert.
- `internal/store/pg_store.go` — full BNK-02/03 projections.
- `internal/store/bnk0304_store.go` — set-based line insert, raw-evidence
  canonical writes, reprocess reset.
- `deployments/migrations/008_allow_bnk03_quarantine_reprocess.up.sql` and
  `.down.sql` — narrow reprocess trigger exception.
- `internal/handler/bnk02_handler_test.go` — BNK-02 handler regression tests.
- `internal/store/bnk02_store_test.go` — full-projection regression test.
- `internal/store/bnk0304_store_test.go` — reprocess, evidence, batch and
  raw-value tests.
- `internal/store/main_integration_test.go` — caller-supplied disposable DB
  support.
- `internal/store/outbox_events_test.go` — updated for optional assertions.
