# ZS-JUR-001 Wave 3: test harness and certification

Builds on [Wave 0](jurisdiction-pack-wave0.md) and [Wave 1](jurisdiction-pack-wave1.md). Implemented in
`jurisdiction-rules-svc` (migration `000007`, depends on `000005` and `000006`). Follows ZS-JUR-001 s20, s22, s23, s28
and JUR-NEG-01, 02, 07, 18, 22.

## Flow

`DRAFT` -> `submit-review` -> `compile` -> `sign` -> **test-bundle** -> **test-runs** -> **reviews** -> **certify** -> `CERTIFIED`.
Every step from test-bundle on is its own authorization action. **There is no release yet**: CERTIFIED is the end of
what this wave builds. RELEASED, WITHDRAWN, rings, rollback and hotfix are Wave 7.

## What the harness executes

The harness runs a test bundle against the **compiled artifact only**, never live rows, so what is tested is exactly what
is signed. The one deterministic behavior the artifact defines is **effective-dated rule resolution with declared
precedence** (`ArtifactDoc.Resolve`, a pure function that Wave 2's runtime resolver will reuse):

* the jurisdiction asked for is looked up by code in the artifact scope; outside it the answer is `UNSUPPORTED_JURISDICTION`
  (JUR-NEG-22), never a guess;
* candidates are rules of that domain and code, in that jurisdiction or its parents (the artifact carries the parent links),
  effective at the instant (half-open interval: start inclusive, end exclusive);
* more than one candidate is decided **only** by declared precedence (distinct, higher wins) or a `supersedes` link; otherwise
  the answer is `AMBIGUOUS`. There is no implicit "most specific wins" or "latest wins" (s10, s39).

Test classes implemented: `GOLDEN`, `BOUNDARY`, `HISTORICAL`, `NEGATIVE`. A run **passes only if every case passes and
coverage is complete**:

| Requirement | Code |
|---|---|
| each rule has a GOLDEN case that resolves to it | JUR-T001 |
| each rule has BOUNDARY cases at its start, start-minus-1ns, and (if it ends) end and end-minus-1ns | JUR-T002 |
| each supersession has a HISTORICAL case showing the older rule still serves its own period (JUR-NEG-01/07) | JUR-T003 |
| at least one NEGATIVE case expects UNSUPPORTED_JURISDICTION (JUR-NEG-22) | JUR-T004 |

Each gap names the exact instant the author must add. A failing run is recorded as evidence like a passing one.

**Not implemented, and stated in every run and every certificate (`classes_not_implemented`):** cross-pack conflicts,
migration, schema/payload, regression corpus, performance. The security class is covered as a certification gate (signed,
verifying artifact), not as bundle cases. No rule formula language exists (s38), so there are no calculation tests.

## Certification gates

`POST .../certify` succeeds only when all of these hold, and otherwise answers 409 `certification_blocked` listing **every**
unmet gate:

1. the artifact is compiled, signed and verifies (digest, recorded digests, registered non-revoked key, signature);
2. the latest test bundle was run against **exactly this artifact** and the run passed with complete coverage (a newer
   bundle revision invalidates the earlier run until re-run);
3. at least `PACK_CERT_MIN_REVIEWS` (default 1) **distinct** independent reviewers approve this artifact, and no reviewer's
   latest decision is REJECT (the latest decision per reviewer and role counts; a rejection needs findings);
4. the certifier is independent of the version author, the compiler, every test author and every approving reviewer.

Reviewers are independent of the author, the compiler and the test authors. The certification report (artifact and bundle
digests, run id, test summary, `classes_not_implemented`, reviews, min reviews, certifier) is canonicalized, hashed and
**signed with the service key** under its own signing domain; with no signer configured, certification is 503. On success
the version becomes CERTIFIED in the same transaction and records the bundle digest.

The database repeats the important gates independently: it refuses a certification without a signed artifact, a passed run
of the same artifact and bundle, or an independent certifier; refuses CERTIFIED status without a certification row; refuses
reviews, tests and runs once the version leaves REVIEW; and treats bundles, runs, reviews and certifications as append-only.

## Authorization actions

`JURISDICTION_PACK_VERSION_AUTHOR_TESTS|EXECUTE_TESTS|REVIEW|CERTIFY` (demo seed bundle `JURISDICTION_PACK_REGISTRY_FULL`).
Real environments must give them to different roles. Events: `jurisdiction-pack.tests-run`, `.reviewed`, `.certified`.

## Decisions the document leaves open, and what this wave assumed

* **Review count and roles (s20, s28 "enhanced review").** Not specified. Assumed: `PACK_CERT_MIN_REVIEWS`, default 1, role
  labels are free UPPER_SNAKE text with no required-role matrix. A production policy (for example two reviewers with TAX and
  LEGAL roles for anything touching liabilities) must be set by Tax/Legal leadership; nothing here enforces required roles.
* **Coverage threshold (s22 "Coverage threshold met").** The specification gives no number. Assumed the structural rules above
  (every rule, every boundary, every supersession, one unsupported-jurisdiction case), which are deterministic, not a percentage.
* **Who may certify.** Only independence is enforced. There is no list of certified-certifier identities or role check inside
  this service beyond the authorization action.
* **Certifier is not the signer of the artifact.** Not required: the signer may also certify, because both use the service key.
* **Hierarchy semantics.** Resolution walks the parent chain and lets declared precedence decide. s10's rule that subdivisions
  override national rules only where the regime permits local variation is **not modelled** (no regime attribute says so).

## Not built

Release, withdrawal and rollback of CERTIFIED versions (Wave 7); the runtime resolver and its use of verified artifacts (Wave 2);
test classes listed above; calculation tests; cross-pack regression; CI gating of pack changes (the `internal/registryit` suite
is not in the repository's CI matrix, and `ci.yml` was not changed).

## Verification

Unit tests (resolution incl. half-open boundaries and hierarchy, strict bundle parsing, coverage gaps, failing cases) and the
integration suite `internal/registryit` (embedded Postgres 16): full lifecycle, every blocker, independence through the API
and through raw SQL, minimum-review policy, stale-run invalidation, signature tamper detection on the certification report,
append-only checks, and the 000005 to 000007 down/up round trip.
