# ZS-SVC-N-001 Wave 9 Backlog — Cross-Domain Certification & Production Rollout

Deferred until all platform services are built. This is **not** a per-service
build like Waves 1–8 — it's the platform-wide certification gate every spec's
waves feed into, and it can't be honestly executed against a partially-built
platform (most of its own prerequisites don't exist yet as live, deployed
services).

## Why this is deferred, not skipped

ZS-SVC-N-001 §13 gives Wave 9 one line: "Cross-domain certification, red-team/
abuse testing, regional failover, cost/capacity, production rollout." The spec
itself has zero further elaboration — confirmed by full-text search of the
converted doc (2198 lines, checked for every relevant section header and
keyword). The real content lives in three separate, platform-wide standards:

| Standard | Doc ID | What it actually defines |
|---|---|---|
| `ZoikoSuite_Engineering_Verification_Certification_Acceptance_Standard...docx` | **ZS-QA-001** | Release certification from verifiable evidence, risk-weighted release decisions, independent authorization |
| `ZoikoSuite_Global_Non_Functional_Requirements_SLO_Resilience_Standard...docx` | **ZS-NFR-001** | Platform-wide capacity, RPO/RTO, resilience/failover targets |
| `ZoikoSuite_Global_Operational_Runbook_Production_Control_Standard...docx` | **ZS-OPS-001** | Production incident response, regional failover procedures |

ZS-SVC-N-001 §11/§12 ("Definition of Ready"/"Definition of Done") explicitly
reference ZS-QA-001 and ZS-SEC-001 by name for release certification and
security/privacy test mapping — so Wave 9 is this spec's services going
through that shared gate, not a standalone deliverable scoped to N-001 alone.

None of ZS-QA-001/ZS-NFR-001/ZS-OPS-001 have been read in full yet — only
their cover pages, to confirm topic. **Before starting Wave 9 for real**, read
all three fully and turn this backlog into a precise checklist; what follows
is a scoping placeholder, not a worked plan.

## What's genuinely out of reach in a code session

- **Red-team/abuse testing** — real adversarial penetration testing needs a
  live, deployed target and security tooling, not unit/integration tests
  written against a throwaway local Postgres instance.
- **Regional failover** — requires actual multi-region infrastructure to
  drill against; nothing in this repo stands up more than one region.
- **Cost/capacity** — real load testing against realistic traffic needs a
  running environment sized like production, plus the cost-metering
  infrastructure itself (not yet built for most services).
- **Production rollout** — sequencing, canary/ring deployment, rollback
  drills — all meaningless before there's a real environment to roll out to.

## What IS achievable now, if wanted before full deferral

These don't require live multi-region infra and could be done against the
services already built this session, as a partial down payment — flagged
explicitly as partial, not a substitute for the real Wave 9:

1. **Cross-domain contract verification** — statically check that AI-03's
   assumed DATA-06 authorization-before-retrieval contract, AI-01/02's
   evidence shapes, etc. actually match what those services expose today
   (a correctness check, not a live integration test — these services
   aren't deployed together).
2. **Deeper adversarial testing on what's already built** — actively try to
   break RLS, forge idempotency replays, bypass immutability triggers on the
   10 services from this session, beyond the happy-path-adjacent negative
   tests already written per service.
3. **Full read of ZS-QA-001/ZS-NFR-001/ZS-OPS-001** — turn this backlog into
   an accurate, specific checklist instead of the current placeholder, so
   whoever picks this up later isn't starting from a one-line summary.

## Status

| Item | Status |
|---|---|
| Wave 1–8 (all ZS-SVC-N-001 services) | ✅ Done — see `assistant-rag-svc`, `anomaly-detection-svc` (AI-04 addition), `forecasting-svc` (AI-05 addition), and the 7 services in PR #184 |
| Wave 9 — cross-domain certification | ❌ Not started — blocked on platform-wide build completion |
| Read ZS-QA-001/ZS-NFR-001/ZS-OPS-001 in full | ❌ Not started |
| Items 1–3 above (partial, achievable now) | ❌ Not started — not begun, pending explicit decision to do partial work now vs. wait |
