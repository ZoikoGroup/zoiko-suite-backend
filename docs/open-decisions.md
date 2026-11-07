# Open decisions: ZoikoSuite Assistant

Decisions the assistant cannot make for itself. Until each closes, the assistant avoids the claim
or label in question.

## OD-01 Taxonomy: dashboard areas vs documented modules (blocks fixed counts)

**Decides:** the signed Services/Categories Reconciliation v2.1 (Product Architecture, Global
Product Lead). **Status:** open; the artifact is not available.

| Source | What it says |
|---|---|
| Admin dashboard (frontend-platform `/admin`) | 11 areas: Finance & Payments, Legal & Contracts, Commercial Ops, Purchase Requests, Tax, Compliance, Evidence, Delegated Authority, Intelligence & Reporting, Security & Trust, Integrations |
| Doc 03 §5, Doc 02 T03 | Four module domains with ten named canonical modules (Finance: Accounting & General Ledger, Accounts Payable, Accounts Receivable, Treasury & Cash Position; Workforce: Payroll, Human Resources; Tax, Legal & Compliance: Tax Management, Legal & Contracts, Compliance & Obligations; Commercial: Procurement & Commercial Operations), six homepage thematic domains, and the tiers Core Modules / Governance Platform / Platform Foundation. No stable IDs: those come from the signed v2.1 registry |
| Earlier synthetic seed (`data/seed`) | A 10-module / 4-domain grouping (Finance, Workforce, Tax/Legal/Compliance, Commercial). This was a **synthetic placeholder** written for tests, not the documented taxonomy |

Conflicts:

1. 11 dashboard areas vs 4 documented module domains vs 6 documented homepage domains.
2. The documents name the ten canonical modules (Doc 03 §5) but give no stable IDs. (Corrected 2026-10-28: an earlier version of this note said the names were missing.)
3. Purchase Requests is its own dashboard area but reads as part of Commercial Ops.
4. Integrations is a dashboard area; the documents treat integrations as per-connector status
   claims, not as a domain.

What the assistant does until this closes:

- "What modules…" is answered from `00-modules.md` (Doc 03 §5 names, Doc 02 T03 tiers) with the
  availability qualifier, and never quotes a number of modules or domains (test K3). The dashboard
  areas article (`00-areas.md`) describes console areas only.
- `services/public-ai-svc/data/public-kb/taxonomy-crosswalk.json` lists each dashboard area as
  `UNMAPPED_PENDING_V2_1`; no final taxonomy is invented.
- When v2.1 is signed: map every area to its `module_id`, `capability_id`, `domain_view_id`,
  `public_route_id` and `sidebar_route_id` in the crosswalk (ingest already fails publication on
  unmapped or conflicting entries), then update the areas article.

## OD-02 Public URLs (blocks links and CTAs)

No real public page URLs are registered. The route registry and articles use only placeholders,
which the service never exposes: the widget shows source titles without links and no CTA buttons.
**Decides:** Web owner, via the signed route registry (DEC-10).

## OD-03 Public KB approval (blocks production)

All articles in `data/public-kb` are `DRAFT_PENDING_REVIEW`. They answer only in local/dev
(`KB_ALLOW_DRAFT=true`, labelled "Draft content"). Production needs each article approved by
someone other than its owner through the two-person workflow. **Decides:** content owners per area.

## OD-04 Retention period

Shown to visitors as "[period from signed policy]" until the DPO signs the retention policy
(DEC-06). The service refuses to start outside dev/test without `PUBLIC_AI_RETENTION`.

## OD-05 Embeddings

Retrieval is lexical (BM25) with aliases; no embedding model is approved (DEC-05, ADR-002). The
service logs this at start.

## OD-06 Authenticated admin/account copilot (live platform data)

**Status:** DEFERRED (product decision, 2026-10-28).

**Decision:** an authenticated admin/account copilot is FR-16 (P2) and belongs to R2. It is a separate program that needs its own architecture, threat model, and privacy and liability review. It is not part of R0.

**Until then:**
- The public assistant never answers live status, counts, domain health, cluster readiness or tenant data. These questions get the X01 refusal with two chips (Explore modules, Get support), no CTA and no model call.
- The chatbot is shown on `/admin` (product owner decision, 2026-10-28) as the same anonymous public
  assistant: no cookies, sign-in or tokens, and live status is still refused. Live status is read on
  the dashboard itself.
- The chatbot has no admin-only route and no sign-in or token path. It must never import `getDomainHealth()` or any other dashboard, health, auth or session module.
- Enforced by:
  - frontend: ESLint `no-restricted-imports` ("public chatbot boundary" in `eslint.config.mjs`), `components/assistant/import-boundary.test.tsx`, `boundary.test.tsx` (the widget is active only on `/assistant`) and the Playwright test that, signed in on `/admin`, the assistant refuses live data and sends no cookie or Authorization;
  - backend: `isolation.TestT12_NoRouteToPrivateSystems`, `acceptance.TestLivePlatformQuestionsAlwaysRefused` and `TestK5_LiveMicroservicesRefused`.
