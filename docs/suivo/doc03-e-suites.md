# Doc 03 §23 verification suites E01–E10 → tests

Each suite in Doc 03 §23 mapped to the Go tests in `services/public-ai-svc` that cover it, and what
is still missing. All named tests run in CI (`go test ./...`) and the nightly replay. The holdout set
of cases written by legal, commercial and security reviewers (not by the engineer who built ranking)
does not exist yet: that is a people dependency (Doc 07 partition E).

| Suite | Covered by | Still missing |
|---|---|---|
| E01 Taxonomy | `acceptance.TestA04_CanonicalModulesWithCitations`, `TestA05_SixDomainsMapToModules`, `TestA06_CategoryNotInRegistry`, `TestKB014_CanonicalAPAlias`, `TestKB015_SixDomainsAreNotSixModules`, `TestK3_ModulesWithoutAFixedCount`, `knowledge` crosswalk checks | The signed v2.1 taxonomy (DEC-02): today the crosswalk is unreconciled |
| E02 Pricing | `TestA07_SignedStarterPriceUS`, `TestA08_WebsiteVersusPriceBook`, `TestA09_TrialDisabledNoTrialCTA`, `TestKB003_PublicPageContradictsSignedPrice`, `TestKB009_ExpiredTrialClaim`, `TestKB019_PlanLabelMapsToStableID`, `TestKB025_PricingInUnsupportedMarket`, `claims.TestPriceClaimCompleteness`, `TestTrialClaimCompleteness`, `validate.TestFiguresAreWholeTokensAndP1NeedsAClaim` (incl. capacity), `TestDoc02GoldenParaphrases` G03–G07 | Allowance and entitlement claims need real data (D03-04); invalid-currency fixture |
| E03 Jurisdiction | `TestA10_MarketMissingRestrictedCapability`, `TestKB004_MarketMissingAsksOnce`, `TestG12_TaxFilingGermany`, `orchestrator.TestRegionalCoverageNeedsMarketClaim`, paraphrases G12/G13/G33 | `FILING_COVERAGE` claims per country (D03-04) |
| E04 Trust | `TestA11_UnknownCertification`, `TestKB006_StaleSOC2NeverCertified`, `TestG10_SOC2ISOCertified`, `TestK6_SOC2NeverClaimed`, paraphrases G10/G24, `claims.TestConditionalAndTargetClaimsAreQualified` | Real, independently validated trust records |
| E05 Citations | `TestKB018_UnapprovedOrigin`, `TestKB026_CitationsDeduplicated`, `acceptance.TestReplyCarriesSourceDateAndDisposition`, `validate.TestClaimToSpan`, `cmd/linkcheck.TestCheck` (broken, redirected, off-allowlist) | Stale-anchor check; real URLs (D05-02) |
| E06 Authorization | `TestA01_AnonymousPublicScope`, `TestA03_ForgedRoleAndTenant`, `TestKB002_PrivateDocumentMislabelledPublic`, `TestKB007_PayloadCannotBypassPublicFilter`, `TestKB013_PrivatePayrollSlip`, `TestKB021_ForgedTenantID`, `knowledge.TestDeniedChunksAreNotRanked` | — |
| E07 Injection | `TestA19_UserPromptInjection`, `TestA20_InjectionFromRetrievedPage`, `TestKB012_HiddenInstructionQuarantined` (comments, titles, headings, CSS-hidden, hidden attribute, bidi, zero-size), `policy.TestMultilingualInjection`, `FuzzClassifyAndRedact` | PDF and HTML-meta fixtures (no PDF/HTML ingestion yet) |
| E08 Deletion | `TestA13_RevokedClaimInCache`, `TestKB010_RevokedAfterCachePopulated`, `TestKB011_RollbackKeepsRevocation`, `TestKB030_WithdrawnSourceInvalidatesReferences`, replay revocation in `httpapi` | — |
| E09 International | `TestKB023_SupersededAtMidnightUTC`, `acceptance.TestSupersedesEndsTheOldVersion`, `TestUnsupportedLocaleFallsBackToDefault`, `contract.TestServedLocaleFallsBackToDefault`, G33 paraphrases | en-GB content variants (only en-US is approved) |
| E10 Operational | `TestKB020_IdempotentRerun`, `TestKB022_EmbeddingsUnavailable`, `TestKB027_EvaluationGatesPromotion`, `events.TestRelayDeliversOnceAndDivertsToDLQ`, `cmd/drill` DR-01..07, `cmd/loadtest.TestModelFaultsUnderLoad`, `release.TestSignatureEnforced` | Index-corruption drill on a live store |
