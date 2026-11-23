# STRIDE threat model: ZoikoSuite Assistant (R0)

Doc 11 F01 asks for STRIDE plus OWASP LLM/API scenarios with control owners (Doc 06). This covers the R0 public-only assistant: browser widget → console proxy → public-ai-svc → model provider, knowledge and claim releases, Postgres and the event outbox. OWASP LLM items are mapped in `owasp-llm-map.md`.

Trust boundaries:
- **B1** Visitor's browser ↔ console origin (anonymous, untrusted).
- **B2** Console proxy ↔ public-ai-svc (allowlisted paths and headers only).
- **B3** public-ai-svc ↔ model provider (redacted text only, no tools).
- **B4** Operators (`kbctl`) ↔ releases, flags, revocations.
- **B5** public-ai-svc ↔ event broker.

| # | STRIDE | Boundary | Threat | Control | Test evidence | Owner |
|---|---|---|---|---|---|---|
| T01 | Spoofing | B1/B2 | Another site uses a visitor's session grant | Grant bound to its creating origin; cross-site requests refused by the proxy | `TestGrantBoundToOrigin`, `TestCT003_OriginDenied`, `boundary.test.tsx` | Security |
| T02 | Spoofing | B1 | Forged role or tenant in a request | No identity in the public API; strict schema | `TestA03_ForgedRoleAndTenant`, `TestKB021_ForgedTenantID` | Backend |
| T03 | Spoofing | B4 | Self-approved release promoted | Two distinct approvers and an independent promoter | `TestKB016_SignerNotIndependent` | Knowledge Owner |
| T04 | Tampering | B4 | Release content altered after signing | Ed25519 signatures; chunk checksum, tier and scope checks | `TestSignatureEnforced`, `TestKB008_ChunkLosesParentLabel` | KB Engineering |
| T05 | Tampering | B1 | Instruction injection in chat (incl. encoded) | Safety fold, exception rules, base64 check, no tools | `TestSafetyFoldDefeatsEncodingBypasses`, `TestA19`, `TestA21` | AppSec |
| T06 | Tampering | B4 | Poisoned article, title, synonym or keyword stuffing | Ingest quarantine | `TestKB012_HiddenInstructionQuarantined`, `TestA20` | KB Engineering |
| T07 | Tampering | B3 | Model output with markup, links or invented facts | Output validation; unsafe drafts never repaired | `TestA22`, `TestA23`, `TestOutputValidation` | AI Lead |
| T08 | Repudiation | B4 | Operator denies a flag, promotion or revocation | Append-only control audit, written in the same transaction | `TestOperatorActionsAreAudited`, `TestFlagChangesAreAudited`, `TestPostgresControlAuditAppendOnly` | SRE |
| T09 | Repudiation | B1 | Visitor disputes an answer | Turn stored with release ids and request id; feedback enums | `TestCT009`, `TestFeedbackOnOwnTurn` | Product |
| T10 | Info disclosure | B1 | Secrets or contact details echoed or sent on | Redaction before model; contact masking; sensitive draft warning | `TestRedactSensitiveInput`, `TestContactDetailsNotKept` | DPO |
| T11 | Info disclosure | B2 | Private or internal content retrieved | Public-only labels enforced before ranking | `TestKB007`, `TestPrivateAndPoisonedNeverRetrieved` | Security |
| T12 | Info disclosure | — | Visitor text in logs, traces, metrics | Policy decision record has enums only; metric labels low-cardinality | `TestA39_TelemetryCarriesNoCustomerText`, `TestMetricsEndpoint` | SRE |
| T13 | Info disclosure | B3 | Data sent to an unapproved provider or region | Provider approval reference and region allowlist required outside dev | `TestA40_UnapprovedRegionFailsClosed`, `TestProductionGates` | DPO |
| T14 | Info disclosure | B1 | System prompt or policy extracted | Refusal before model; leak check on output | `TestA21_SystemPromptDisclosure`, `TestG21` | AI Lead |
| T15 | Denial of service | B1 | Flooding or cost spike | Per-IP, per-session and in-flight limits; staged throttling; session token cap | `TestPerIPLimit`, `TestSpendStages`, `TestSessionTokenCapStages` | SRE |
| T16 | Denial of service | B3 | Provider slow or down | Deadlines, circuit breaker, static fallback | `TestA29_ProviderTimeout`, `TestProviderFailuresFallBackSafely` | SRE |
| T17 | Denial of service | B5 | Broker down; events pile up or replay stale | Outbox, DLQ, audited replay with freshness checks | `TestRelayDeliversOnceAndDivertsToDLQ`, `TestDLQReplayRechecks` | SRE |
| T18 | Elevation | B2 | Public API reaches private services or admin | No route or credential; isolation test on config and env | `TestT12_NoRouteToPrivateSystems`, `TestA34_NoAdminRoute` | Security |
| T19 | Elevation | B1 | Chat used to act (pay, book, submit) | Exception refusals; false-confirmation validator; route-only handoff | `TestG17`, `TestFalseConfirmationsRejected` | Product |
| T20 | Elevation | B4 | Unapproved policy deployed | Service refuses to start unless the policy digest matches | `TestPolicyDigestCoversRulesAndIsEnforced` | Security |

Residual risks (need decisions or infrastructure): network egress proof (DEC-01), provider DPA (DEC-05), retention (DEC-06), image signing and provenance, and a staffed incident response (R-12).
