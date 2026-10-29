# Synthetic seed data (dev mode)

Every record here has `"synthetic": true`. None of it is ZoikoSuite fact.

- `kb/release.json`: six official routes on `example.invalid`, the capability crosswalk
  (10 modules in 4 domains: Finance, Workforce, Tax/Legal/Compliance, Commercial) and synonyms.
- `kb/sources/`: 10 approved sources with two passages each (20 evidence passages). Passages
  carry no figures: prices, trial lengths and statuses live only in typed claims.
- `claims.json`: plan availability, placeholder prices (`[price]`) and trials (`[N] days`) for
  Starter, Growth and Enterprise in US and GB. The **GB trials are deliberately expired** so the
  assistant must abstain on them. SOC 2 is on HOLD (never claimed); two integration statuses.

Local dev mode serves this dataset by default (`PUBLIC_AI_DATASET=seed`). The unit, golden and
document acceptance suites keep using `testdata/fixtures`; `internal/acceptance/seed_test.go`
runs the T1-T12 scenarios on this seed. Real content is loaded only through `kbctl` with signed
approvals.
