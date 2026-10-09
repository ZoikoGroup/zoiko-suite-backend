# notification-svc: ZS-SVC-Y-001 certification matrix

Every row of §14 (NP-01…60), §15 (INV-01…30) and §16 (TC-01…20), scored
against the code and its evidence on **6 October 2026**.

**Scoring rule.** **Met** means the control is implemented *and* a test or live
check proves it. **Partial** means it is implemented only in part, or
implemented with nothing proving it. **Not met** means it is not implemented.
The **Owner** column marks rows whose missing part belongs to another
component. The service cannot close those alone.

Store tests run against Postgres 16 as `NOSUPERUSER NOBYPASSRLS` roles.
"Live" means `scripts/ncd_live_check.py` (69 checks) against the running
service. Test names are Go test functions in `internal/`.

## Summary

| Matrix | Rows | Met | Partial | Not met |
|---|---|---|---|---|
| §14 NP-01…60 | 60 | 55 | 4 | 1 |
| §15 INV-01…30 | 30 | 27 | 2 | 1 |
| §16 TC-01…20 | 20 | 13 | 6 | 1 |
| **All** | **110** | **95 (86%)** | **12** | **3** |

Weighted, with partial counted as half: **92%**.

Ten of the 15 rows short of met are blocked on something outside this
service: the estate (no caller adoption, no bypass prevention), PRV, DRC, WFC,
XIC, MDM/IAM integration tests, or open decision OD-05. On the **100 rows this
service can close by itself**, it scores **95 met, 5 partial, 0 not met:
95% (97.5% weighted)**.

Eight rows had correct code with nothing proving it: NP-10, NP-39, NP-56,
NP-60, and the invariants resting on them, INV-06, INV-18, INV-24 and INV-25.
Five tests were added during this scoring (NP-56's confirmed with a negative
control). Before those tests the matrix stood at 87 met, 20 partial, 3 not met:
**79% (88% weighted)**.

## §14 Negative-path matrix

| ID | Expectation | Status | Evidence |
|---|---|---|---|
| NP-01 | Domain calling a provider SDK directly is blocked or detected | **Not met** (Owner: estate/XIC) | Nothing stops another service opening SMTP itself. Provider secrets are env vars, not XIC-brokered. |
| NP-02 | Unknown intent: BLOCK NCD-001, no provider call | Met | TestNCD01 registry (`NCD001IntentNotFound`); handler 404 path |
| NP-03 | Retired intent blocked | Met | TestNCD03_RetiredIntentIsNotEffective |
| NP-04 | Draft template never dispatches | Met | TestNCD03_DraftTemplateNeverDispatches |
| NP-05 | Published template edit rejected | Met | TestNCD01_TemplateLifecycleImmutableAndSegregated; DB trigger; live |
| NP-06 | Missing required variable blocks | Met | TestNCD03_ContentBlocksBeforeAnyProvider |
| NP-07 | Unexpected variable rejected | Met | TestNCD03_ContentBlocksBeforeAnyProvider; TestNCD01_ValidationBlocksUnsafeContent |
| NP-08 | CRLF/header injection rejected and audited | Met | TestNCD03_ContentBlocksBeforeAnyProvider (the refusal is persisted as a blocked communication with NCD-004) |
| NP-09 | Unapproved locale for a regulated notice: no silent fallback | Met | TestNCD03_ContentBlocksBeforeAnyProvider |
| NP-10 | Attachment "latest" pointer refused, exact version and hash pinned | Met | **TestNP10_LatestAttachmentPointerIsRefused** (added 6 Oct) |
| NP-11 | Free-text legal recipient needs a controlled exception | Met | TestNCD02_FreeTextEndpointForRegulatedNeedsControlledException |
| NP-12 | Cross-tenant recipient blocked | Met | TestNCD02_RecipientPlanCarriesProvenance; live. Every cross-tenant recipient is refused, since no MDM relationship exists to authorize one. |
| NP-13 | Hard-bounced endpoint suppressed, no provider call | Met | TestNCD02_SuppressionPrecedence; TestNCD04_BounceSuppressesAndFallsBack |
| NP-14 | Marketing opt-out blocks across routes | Met | TestNCD02_SuppressionPrecedence |
| NP-15 | Muted channel overridden only by an explicit security policy | Met | TestNCD02_PreferencesVersusMandatoryPolicy |
| NP-16 | Muted channel respected for routine workflow | Met | TestNCD02_PreferencesVersusMandatoryPolicy; live |
| NP-17 | PRV INDETERMINATE fails closed | Met | TestNCD02_PermissionFailsClosed; live |
| NP-18 | Mandatory notice cannot override a privacy/legal block | Met | TestNCD02_PermissionFailsClosed |
| NP-19 | Quiet hours defer routine messages in recipient time | Met | TestNCD02_QuietHoursUseRecipientCivilTime |
| NP-20 | Missing time zone uses a governed fallback, never a guess | Met | TestNCD02_QuietHoursUseRecipientCivilTime |
| NP-21 | Replayed source event returns the existing communication | Met | TestNCD03_HappyPathEvidenceIsPrecise; TestNCDHTTP_EndToEnd; live |
| NP-22 | Timeout after submit becomes UNKNOWN, never a blind retry | Met | TestNCD03_TimeoutBecomesUnknownAndNeverBlindResends |
| NP-23 | 500 before submit retries with history preserved | Met | TestNCD03_RetryableFailureRetriesWithHistory |
| NP-24 | Duplicate callback deduplicated | Met | TestNCD04_CallbacksDeduplicateAndNeverRollBack; TestNCD04_EventDedupeIsTenantScoped; live |
| NP-25 | Callback before API response: no impossible rollback | Met | TestNCD04_CallbacksDeduplicateAndNeverRollBack; live |
| NP-26 | Invalid callback signature rejected, state unchanged | Met | TestNCD04_CallbackAuthentication; TestNCDHTTP_ProviderEventsNeedSignature; live |
| NP-27 | Provider id mapping to two attempts is quarantined | Met | TestNCD04_ProviderMessageIDCollisionIsQuarantined |
| NP-28 | Outage uses only certified failover, otherwise preserves the queue | Met | TestNCD03_OutagePreservesQueue |
| NP-29 | Fallback cannot lower the evidence class | Met | TestNCD03_FallbackCannotLowerEvidenceOrResidency |
| NP-30 | Fallback cannot violate residency | Met | TestNCD03_FallbackCannotLowerEvidenceOrResidency |
| NP-31 | S3 payload not exposed on a low-confidentiality surface | Met | TestNCD01_ValidationBlocksUnsafeContent ("S3 in email body" blocked, allowed in-app). No SMS/push channel exists yet (OD-03/04). |
| NP-32 | Sensitive fact in the subject blocked | Met | TestNCD01_ValidationBlocksUnsafeContent ("S3 in subject") |
| NP-33 | Forwarded secure-link token is not sufficient alone | **Partial** (Owner: OD-05) | Action links are single-use and scanner-safe (`internal/actionlink`) but not bound to an authenticated audience. The secure-link model is open decision OD-05. |
| NP-34 | Attachment hash differs after provider upload: block | **Partial** | Attachments are pinned by version and hash into the content hash (INV-18). No provider transmits attachments yet, so no post-upload comparison exists. |
| NP-35 | "Provider accepted" is never labelled delivered | Met | TestNCD03_HappyPathEvidenceIsPrecise; legacy `status` precise (console Playwright 21/21) |
| NP-36 | Mailbox accepted shown per semantics, not as read | Met | TestNCD04_CallbacksDeduplicateAndNeverRollBack |
| NP-37 | A missing open pixel never implies non-receipt | Met | TestNCD04_CallbacksDeduplicateAndNeverRollBack; TestEngagementSignalsAreLowConfidenceAndChangeNothing |
| NP-38 | An open via proxy is not an acknowledgment | Met | same; live |
| NP-39 | An unauthenticated click is interaction evidence only | Met | **TestEngagementSignalsAreLowConfidenceAndChangeNothing** (added 6 Oct) |
| NP-40 | Acknowledging a superseded version is rejected | Met | TestNCD05_SupersededVersionCannotBeAcknowledged; live |
| NP-41 | Operator "served" without evidence is rejected, maker-checker | Met | TestNCD05_OperatorEvidenceNeedsMakerChecker |
| NP-42 | Ack deadline expiry escalates, never fabricates an ack | Met | TestNCD05_DeadlineExpiresWithoutFabricatingAck (raises `ACK_EXPIRED` and its event; no WFC exists to receive it, see TC-15) |
| NP-43 | Correction preserves the original | Met | TestNCD05_SupersededVersionCannotBeAcknowledged |
| NP-44 | Misdelivery: incident path, preserve evidence, stop sends | Met | TestNCD06_MisdeliveryStopsSendsAndPreservesEvidence |
| NP-45 | Complaint at provider A still blocks provider B | Met | TestNCD02_LegacySuppressionListIsCanonical |
| NP-46 | Direct unsubscribe writes a durable canonical suppression | Met | `internal/unsubscribe` tests; legacy store upsert-rank test; live marketing round-trip (5 Oct) |
| NP-47 | List without consent provenance: PRV/PDC blocks promotion | **Partial** (Owner: PRV) | Marketing without a PRV PERMIT fails closed (TestNCD02_PermissionFailsClosed), but the PERMIT is caller-supplied. privacy-decision-svc exists and is not called (gap G-1). |
| NP-48 | Promotional module in a transactional template blocked | Met | TestNCD01_ValidationBlocksUnsafeContent ("promotional block") |
| NP-49 | Bulk query spanning tenants is blocked | Met | TestNCD06_GovernedBulkSend |
| NP-50 | Audience change between preview and send detected | Met | TestNCD06_GovernedBulkSend |
| NP-51 | Rate-limit backlog preserves jobs, never drops | Met | TestNCD03_QuotaDefersNeverDrops |
| NP-52 | Job expiring before submit is EXPIRED, never sent stale | Met | TestNCD03_ExpiryAndPriority |
| NP-53 | Critical traffic is not starved by marketing | Met | TestNCD03_ExpiryAndPriority; TestNCD04_ComplaintSpikePausesMarketingOnly |
| NP-54 | Unavailable external assets: local assets or block | Met | TestNCD01_ValidationBlocksUnsafeContent (external image and tracking pixel blocked in sensitive templates) |
| NP-55 | DMARC/DKIM break suspends the email stream | Met | TestSMTPProvider_HeldWhileSenderAuthenticationIsBroken; senderauth tests; proven live 5 Oct; alert `NotificationSenderAuthBroken` (6 Oct) |
| NP-56 | Suppression store unavailable: fail closed | Met | **TestNP56_LegacyGateFailsClosedWhenSuppressionsUnreadable** (added 6 Oct, negative control recorded) |
| NP-57 | Evidence write fails after provider delivery: never lose it | **Partial** | An attempt left SUBMITTING becomes UNKNOWN (`MarkStranded`) and the outbox is transactional. Proven for the legacy register (TestSubmission_StrandedAfterSubmitBecomesUnknown); the NCD `MarkStranded` path has no test. |
| NP-58 | DRC declaration failure escalates, delivery stands | Met | TestNCD05_AcknowledgmentBindsActorAndExactVersion (`RECORD_HANDOFF_PENDING`); gauge and alert added 6 Oct |
| NP-59 | Late provider correction appends with lineage | Met | TestNCD04_CallbacksDeduplicateAndNeverRollBack |
| NP-60 | Tenant admin cannot disable unsubscribe/complaint handling | Met | **TestNCDHTTP_UnsubscribeHandlingCannotBeDisabled** (added 6 Oct) |

## §15 Invariants

| ID | Invariant | Status | Evidence |
|---|---|---|---|
| INV-01 | No person-directed message bypasses NCD | **Not met** (Owner: estate) | No service calls notification-svc; the legacy `POST /v1/notifications` path accepts free text (gaps G-6, G-7) |
| INV-02 | One communication id per logical communication | Met | TestNCD03_TimeoutBecomesUnknownAndNeverBlindResends; live |
| INV-03 | Published templates immutable | Met | DB trigger; TestNCD01_TemplateLifecycleImmutableAndSegregated |
| INV-04 | Dispatch pins intent, template, locale, content hash, endpoint | Met | Immutable attempt columns (trigger); TestNCD03_HappyPathEvidenceIsPrecise |
| INV-05 | Missing variables never render as valid content | Met | NP-06 / NP-07 tests |
| INV-06 | Purpose classification explicit | Met | Intent requires one of six classes (`invalid_purpose_class`); TestNCD01_IntentPolicyInvariants ("no purpose class", "unknown purpose class", added 6 Oct) |
| INV-07 | Marketing permission distinct from service necessity | Met | TestNCD02_PermissionFailsClosed; TestNCD02_PreferencesVersusMandatoryPolicy |
| INV-08 | Commercial entitlement never grants marketing permission | Met | live check (entitlement-only marketing refused) |
| INV-09 | Mandatory status cannot override a prohibition | Met | NP-18 test |
| INV-10 | Opt-out and complaint follow the person across providers | Met | NP-45 test; 5 Oct upsert never weakens a reason |
| INV-11 | Accepted ≠ delivered ≠ read ≠ acknowledged ≠ served | Met | NP-35…39 tests; `legally_served: NOT_DETERMINED_BY_NCD` |
| INV-12 | Ambiguity is UNKNOWN until resolved | Met | NP-22 test |
| INV-13 | No blind retry on timeout | Met | DB trigger; TestNCD03_TimeoutBecomesUnknownAndNeverBlindResends; live |
| INV-14 | Fallback cannot lower evidence, confidentiality or residency | Met | NP-29 / NP-30 test |
| INV-15 | Regulated notices cannot use uncontrolled free-text endpoints | Met | NP-11 test |
| INV-16 | Cross-tenant expansion prohibited | Met | NP-12, NP-49 tests |
| INV-17 | S2/S3 facts excluded from subjects and previews | Met | NP-31 / NP-32 tests |
| INV-18 | Exact attachment versions and hashes pinned | Met | TestNP10_LatestAttachmentPointerIsRefused |
| INV-19 | Correction preserves the original | Met | NP-43 test |
| INV-20 | Pixel, click or DLR alone is never acknowledgment | Met | TestEngagementSignalsAreLowConfidenceAndChangeNothing; NP-38 |
| INV-21 | Ack binds actor, exact version, method, time | Met | TestNCD05_AcknowledgmentBindsActorAndExactVersion |
| INV-22 | NCD never decides legal sufficiency | Met | `legal_sufficiency` is always `NOT_DETERMINED_BY_NCD` (live) |
| INV-23 | Quiet hours use civil time, not fixed offsets | Met | TestQuietWindowEnd_FollowsDST |
| INV-24 | Suppression checked immediately before submission | Met | NCD dispatch recheck (TestNCD02_SuppressionPrecedence); legacy gate (NP-56 test) |
| INV-25 | Unsubscribe/complaint cannot be disabled by configuration | Met | NP-60 test; RFC 8058 transport tests; marketing refused without a working link |
| INV-26 | Provider credentials stay in XIC/Vault/KMS | **Partial** (Owner: XIC) | Credentials never enter templates or domain code, but they are env vars, not vault-brokered (G-3). secret-vault-integration-svc exists. |
| INV-27 | Callbacks need authentication, dedup, valid transitions | Met | NP-24…26 tests; legacy webhook HMAC (TestWebhook_HTTPHandler_Routing) |
| INV-28 | Evidence durable even when analytics are down | Met | Transactional outbox; `000023` evidence refuses DELETE; TestHousekeepingStore_DeliveryEvidenceIsNeverDeleted |
| INV-29 | Projections cannot mutate authoritative state | **Partial** | True by construction (every write route needs a principal and authorization, or a signed callback, and the service consumes no projection), but no test asserts it |
| INV-30 | No fail-open turns BLOCK/INDETERMINATE into permission | Met | NP-17 and NP-56 tests |

## §16 Control traceability

| ID | Control | Status | Evidence |
|---|---|---|---|
| TC-01 | Direct provider bypass prevention | **Not met** (Owner: estate/XIC) | As NP-01 |
| TC-02 | Immutable template publication | Met | API test plus DB trigger |
| TC-03 | Typed variable schema and escaping (fuzz/security tests) | **Partial** | Table-driven security tests (script, handlers, CRLF, undeclared vars); no fuzz test |
| TC-04 | Locale and legal wording control | Met | TestNCD01_EffectiveIsBitemporal; NP-09 test |
| TC-05 | Recipient provenance (integration tests) | **Partial** (Owner: MDM/IAM) | Provenance tested with a stand-in resolver; no integration test against identity-context-svc, and no MDM |
| TC-06 | Privacy/marketing permission (decision-contract tests) | **Partial** (Owner: PRV) | NCD's side is tested; no contract test against privacy-decision-svc (G-1) |
| TC-07 | Suppression precedence | Met | TestNCD02_SuppressionPrecedence |
| TC-08 | Quiet-hour civil time (DST tests) | Met | TestQuietWindowEnd_FollowsDST |
| TC-09 | Logical idempotency (replay/concurrency) | Met | NP-21 tests; TestIdempotency_ConcurrentSameKey_ExactlyOneCreated |
| TC-10 | UNKNOWN attempt recovery (fault injection) | Met | TestNCD03_TimeoutBecomesUnknownAndNeverBlindResends (injected post-submit timeout) |
| TC-11 | Evidence-preserving failover | Met | TestNCD03_OutagePreservesQueue; TestNCD03_FallbackCannotLowerEvidenceOrResidency |
| TC-12 | Callback authentication and dedup | Met | NP-24…26 tests |
| TC-13 | Cross-provider canonical suppression | Met | TestNCD02_LegacySuppressionListIsCanonical |
| TC-14 | Delivery semantic accuracy (API/UI) | Met | API tests; console Playwright 21/21 on precise legacy statuses. The plane has no UI of its own (G-8). |
| TC-15 | Regulated notice evidence bundle end to end | **Partial** (Owner: WFC/DRC) | NCD-05 tested in-service; no end-to-end run through WFC and DRC (G-2, G-5) |
| TC-16 | Ack exact-version binding | Met | NCD05 ack tests |
| TC-17 | Correction lineage, historical reconstruction | Met | TestNCD01_EffectiveIsBitemporal; NP-43 test |
| TC-18 | Sensitive-content exposure (DLP/content tests) | **Partial** | Content tests exist; no DLP engine or DLP test |
| TC-19 | Email authentication posture (DNS monitoring) | Met | senderauth monitor tests; live NP-55 |
| TC-20 | Communication-to-record lineage (reconciliation) | **Partial** (Owner: DRC) | Record-handoff state, exception, gauge and alert exist; no reconciliation against document-vault-svc (G-2) |
