# Public knowledge base (dev)

Public capability articles for the ZoikoSuite Assistant, one markdown file per area in `sources/`,
with front matter (`source_id`, `title`, `domain`, `module_id`, `audience: PUBLIC_ANONYMOUS`,
`status`, `owner`, `effective_from`, `expires_at`, `locale`, `intent_ids`).

- Every article is `status: DRAFT_PENDING_REVIEW`. Local dev serves drafts only with
  `KB_ALLOW_DRAFT=true` (the dev default), and every answer from them carries a visible
  "Draft content" label. The service refuses `KB_ALLOW_DRAFT` outside dev/test, a release
  build without it fails if a draft gets in, and a runtime that does not allow drafts refuses to
  load such a release. To publish, owners set `status: APPROVED`, `approvers:` (not the owner)
  and `approval_ts:`, then `kbctl build-kb` / `kbctl publish` (two-person approval).
- Text is rewritten from the product's area descriptions as public capability descriptions. It
  never contains live counters, readiness, latency, cluster or container state, user names or
  anything from `/admin`; ingest quarantines such text (`QUARANTINE_PRIVATE`).
- Prices, trials, markets, certifications, residency and connector status come only from the
  typed claims in `claims.json` (synthetic placeholders), never from these articles.
- No real public URLs exist yet, so `release.json` has no routes and articles have no
  `canonical_url`: the widget shows source titles without links and no CTA.
- `release.json` `synonyms` are the aliases (AR, AP, GL, VAT, GST, SIEM, HRIS, eSignature, mTLS,
  delegation of authority, board resolutions, purchase order).
- `taxonomy-crosswalk.json` maps the 11 dashboard areas against the documented taxonomy. It is
  unreconciled until the signed Services/Categories Reconciliation v2.1 exists; see
  `docs/open-decisions.md`.
