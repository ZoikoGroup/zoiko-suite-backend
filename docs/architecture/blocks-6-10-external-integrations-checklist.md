# Blocks 6–10: external integrations, implementation and dependency checklists

Status as of 2026-11-02. Branch: `fix/blocks-6-10-final` (uncommitted).

None of the four integrations below is complete. No provider has been called from this code, and no credentials or sandboxes were available. Each section separates **what exists in code (verified)** from **what is missing** and **what is needed from outside engineering**.

---

## 1. E-signature (esignature-integration-svc)

**Provider candidates in the spec:** DocuSign eSignature REST, Adobe Acrobat Sign REST. *Provider decision: not made.*

### Exists and is verified
- [x] Adapter boundary: `internal/provider` with an `Adapter` interface, a registry, and `ErrNotConfigured`.
- [x] Configuration via `ESIGN_PROVIDERS`. The default is empty, so the service fails closed. Naming a provider with no compiled adapter makes startup fail.
- [x] **No fake success.** Create returns **503 `esignature_provider_not_configured`** when no provider is configured, and nothing is persisted or published. Before this branch, every envelope was silently marked SENT. Verified live and in tests (`handler/provider_failclosed_test.go`).
- [x] Envelope lifecycle in Postgres: PENDING → SENT → DELIVERED → SIGNED, plus VOIDED. RLS FORCE, idempotency, cross-tenant 404, authz 403. Verified live.
- [x] Route-shadowing bug fixed: `GET /envelopes/{id}` used to return 404.

### Missing (implementation)
- [ ] A real adapter (`Send`, `Void`, `GetStatus`), using OAuth 2.0 JWT grant for DocuSign or an OAuth client for Adobe.
- [ ] A webhook receiver (DocuSign Connect / Adobe webhooks) with **HMAC signature verification and replay protection** (ZS-SVC-S-001 §DRC-05). Manual status updates are currently trusted from the caller; a verified callback must replace them.
- [ ] Capture and validation of the signed artifact (completed PDF plus certificate of completion) into document-vault-svc.
- [ ] Timeout handling: a send that times out should become UNKNOWN in `provider_attempts`, with reconciliation, rather than a blind retry.

### Needed from outside
- [ ] **Decision:** DocuSign or Adobe (or both).
- [ ] Sandbox account (DocuSign developer account or Adobe developer tenant).
- [ ] Credentials, stored in secret-vault-integration-svc and never in env files:
  - integration key / client ID
  - API user GUID
  - RSA private key (JWT) or client secret
  - account ID and base URI
  - webhook HMAC secret
- [ ] Egress allow-list entry for the provider host.

### Evidence required before "complete"
- [ ] Sandbox send → signer signs → verified webhook → envelope SIGNED, with the signed PDF stored.
- [ ] A tampered webhook signature is rejected.
- [ ] A replayed webhook is ignored.
- [ ] A provider timeout is reconciled and not double-sent.

---

## 2. HMRC (tax-authority-interface-svc)

**Provider:** HMRC Making Tax Digital (MTD) APIs (VAT returns; ITSA if in scope), via the generic INT-08 authority adapter.

### Exists and is verified
- [x] Generic INT-08 adapter, real code. It covers:
  - an egress-guarded HTTP transport
  - connector definitions from connectivity-api-bridge-svc
  - credentials from secret-vault-integration-svc
  - HMAC-verified callbacks (`AUTHORITY_CALLBACKS_JSON`)
- [x] Gated by `AUTHORITY_ADAPTER_ENABLED`, which defaults to **off**. While off, filings stay **202 PENDING** with no acknowledgement (no fake success), and adapter and callback routes return **503 `tax_authority_adapter_not_configured`**. Verified in tests and live.
- [x] Interfaces and filings in Postgres with RLS FORCE, idempotency, cross-tenant 404 and authz 403. Verified live.

### Missing (implementation)
- [ ] An HMRC-specific provider profile, certified through `/provider-profiles/{key}/{ver}/certify`.
- [ ] HMRC OAuth 2.0 (user-restricted authorization-code flow) token acquisition and refresh.
- [ ] **HMRC fraud-prevention headers** (`Gov-Client-*` / `Gov-Vendor-*`). These are mandatory for MTD and are not implemented anywhere.
- [ ] Obligations retrieval (open VAT periods) and mapping of the 9-box VAT return from vat-gst-svc.
- [ ] Frontend: there is no `/interfaces/{id}/test` route. Use `GET /v1/tax-authority/health?provider_key=…`.

### Needed from outside
- [ ] HMRC Developer Hub application (sandbox), with client ID and secret.
- [ ] Sandbox test users (organisation with a VRN).
- [ ] Production application approval from HMRC (fraud-header validation is part of HMRC's review).
- [ ] Per-tenant callback secrets in the vault.
- [ ] Decision on which MTD APIs are in scope (VAT only, or ITSA too).

### Evidence required before "complete"
- [ ] Sandbox: OAuth consent → retrieve obligations → submit a VAT return → receipt stored as the filing acknowledgement.
- [ ] HMRC's fraud-prevention header validator passes.
- [ ] A failed or duplicate submission is handled idempotently.

---

## 3. Market data (external-data-feed-svc)

**Sources named by the frontend:** ECB FX reference rates, Refinitiv, Bloomberg. *Provider decision: not made.*

### Exists and is verified
- [x] Subscriptions (`feed_type` ∈ MARKET_DATA | CREDIT_SCORE | COMPANY_INFO | FX_RATE | ESG_DATA) and pushed events (`POST /events/ingest`) in Postgres, with RLS FORCE, idempotency, cross-tenant 404 and authz 403. Verified live.
- [x] Nothing returns fabricated data: the service makes no outbound calls.

### Missing (implementation)
- [ ] **Any outbound client.** Nothing pulls data from a provider. The scheduler/poller is absent.
- [ ] **Pushed data is not provider-signed.** `/events/ingest` accepts any caller holding `FEED_EVENT_INGEST`. It needs either a signature check (provider HMAC or mTLS client certificate) or restriction to an internal ingestion service.
- [ ] Rate validation: staleness, gap detection and an as-of date per published rate.
- [ ] Wiring of FX rates to currency-registry-svc / the GL revaluation consumers.

### Needed from outside
- [ ] **Decision:** which source per feed type. ECB is free and unauthenticated (a daily XML/SDMX feed), so it is the cheapest first adapter. Refinitiv and Bloomberg need licences.
- [ ] For licensed providers: contract, API keys or certificates, and redistribution terms.
- [ ] Egress allow-list entries.

### Evidence required before "complete"
- [ ] Scheduled pull of ECB rates → stored events with source and as-of date, re-fetch is idempotent, a stale-feed alert fires.
- [ ] A pushed event without a valid signature is refused.

---

## 4. Key management (key-management-svc)

### Exists and is verified
- [x] Key **metadata** in Postgres (previously in memory), with RLS FORCE: alias, model (BYOK/HYOK), provider, an external key reference (ARN), version history, and the rotate/disable lifecycle.
  - Rotation creates version n+1 atomically under a row lock; 8 concurrent rotations end at version 9 with exactly one CURRENT.
  - Disable is idempotent.
- [x] A test enforces that **no column can hold key material** (`TestSchema_HoldsNoKeyMaterial`).

### Missing (implementation) — tracker row 82 stays open, High
- [ ] **No cryptographic operation exists.** There is no encrypt, decrypt, sign, wrap/unwrap or generate. "Rotate" records a new version; it does not rotate anything in a KMS.
- [ ] No KMS provider adapter (AWS KMS, Azure Key Vault or GCP KMS).
- [ ] No envelope encryption (data key per tenant wrapped by a KMS key) for the services that need it (siem token sealing currently uses its own AES-GCM keyring).
- [ ] No HYOK reachability check or customer-revocation handling.
- [ ] Rotating a DISABLED key is still allowed; the spec is silent.
- [ ] Versions don't record which actor rotated them.

### Needed from outside
- [ ] **Architecture decision:** which KMS, and whether key-management-svc becomes the crypto boundary (encrypt/decrypt API) or stays a registry that other services consult.
- [ ] Cloud account with KMS access, an IAM role for the service, and a test key per model (BYOK import, HYOK external key store).
- [ ] Security sign-off on the Crypto Standard mapping (ZoikoSuite Security/Privacy/Residency/Cryptographic Architecture Standard).

### Evidence required before "complete"
- [ ] Encrypt/decrypt round trip through the real KMS.
- [ ] Rotation produces a new KMS key version, and old ciphertext still decrypts.
- [ ] Disabling the key makes decrypt fail.
- [ ] A cross-tenant key is unusable.
