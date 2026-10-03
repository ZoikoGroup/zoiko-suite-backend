# ZS-JUR-001 Wave 1: pack compiler, signed artifacts, verification

Builds on [jurisdiction-pack-wave0.md](jurisdiction-pack-wave0.md). Implemented in `jurisdiction-rules-svc`
(migration `000006`, depends on `000005`). The compiler follows ZS-JUR-001 s21; verification follows s3, s23, s28 and
JUR-NEG-03/04/20/25.

## Flow

`DRAFT` (manifest registered) -> `submit-review` -> **compile** (unsigned artifact, digest) -> **sign** (signature) ->
**verify** (any loader). Compile, sign, verify and key administration are four separate authorization actions.
The states after REVIEW (CERTIFIED, RELEASED ...) need the certification harness (Wave 3) and are not reachable.

## Compiler (`domain.Compile`, pure function)

Input is read in one transaction with the version row locked. Output is an artifact (canonical JSON) plus a report.
The artifact contains **no clock and no randomness**, so identical inputs give an identical digest; a rule's later
lifecycle status is excluded for the same reason. It contains: scope (jurisdiction and regime ids and codes), every rule
module with its payload and a per-rule `content_digest`, the sources and interpretations those rules cite, and an
SBOM-style dependency section (exact pins; registered packs carry the dependency's artifact digest; the compiler name
and version).

Errors (the compile is rejected, 422, nothing stored): manifest identity mismatch; rule outside the pack scope; no
regime / regime outside the pack; no source; no interpretation or one not APPROVED (JUR-NEG-18); source not
independently reviewed, superseded or unknown; RETIRED rule; **overlapping rules of the same domain and code with no
declared distinct precedence and no supersedes link** (JUR-NEG-02; back-to-back half-open intervals do not overlap);
**non-integer JSON numbers in rule content** (JUR-NEG-25: rates and amounts must be decimal strings); a pack depending
on itself, on a missing version, a WITHDRAWN/EMERGENCY_BLOCKED version, or one that has not been compiled.

Warnings (compile succeeds): DRAFT rule (its current content is snapshotted); rule never effective inside the pack
window; every source is non-binding guidance (JUR-NEG-23); external code-list or schema dependencies that cannot be
verified because no registry for them exists; a pack with no rule modules.

Dependency cycles cannot occur by construction: a dependency must already exist and be compiled, and versions are
immutable, so a version can never depend on something compiled after it.

Re-running compile on unchanged inputs returns the stored artifact (200). If the inputs changed since (a DRAFT rule's
provenance was edited, for example), the same version is refused (409 `already_compiled`, JUR-NEG-20): publish a new
version.

## Signing and verification

* Ed25519 over `"ZS-JUR-001/pack-artifact/v1\n" + artifact_digest` (domain separated; a signature over the bare digest
  does not verify).
* The private key is read from a **file** named by `PACK_SIGNING_KEY_FILE` (a mounted secret), identified by
  `PACK_SIGNING_KEY_REF`. Both must be set or neither (the service refuses to start half-configured). Unset means the
  sign command answers 503 `signing_not_configured`. The key never enters an environment variable, a table or an artifact.
* Only **public** keys are registered (`pack_signing_keys`), with status ACTIVE -> RETIRED -> REVOKED (forward only,
  never deleted; a `key_ref` names one key forever). The service signs only when its key is registered, ACTIVE and has
  the same public key. RETIRED keys still verify what they signed; REVOKED keys do not.
* `POST .../verify` is the loader check: it recomputes the digest from the stored bytes, compares the digests recorded in
  the artifact and version rows, and checks the signature. It answers 200 only when verified and 409 with reasons
  (`unsigned`, `digest_mismatch`, `signing_key_unknown`, `signing_key_revoked`, `signature_invalid`, ...) otherwise,
  and emits `jurisdiction-pack.verification-failed`. A version with no artifact also fails closed.
* The database independently refuses: editing or deleting an artifact, overwriting a signature, creating an artifact for a
  version that is not under REVIEW or pre-signed, changing key material, deleting a key. The integration suite also
  tampers with the stored bytes and the signature with those guards switched off, and the verifier rejects both.

## Authorization actions

`JURISDICTION_PACK_VERSION_COMPILE|SIGN|VERIFY`, `PACK_SIGNING_KEY_REGISTER|RETIRE|REVOKE` (added to the demo seed bundle
`JURISDICTION_PACK_REGISTRY_FULL`, platform scope). Real environments must give compile, sign, key custody and verify to
different roles. **Nothing prevents one principal holding both compile and sign** in the demo seed.

## Decisions the document leaves open, and what this wave assumed

* **Signing key hierarchy and custody (s38).** Not decided. Assumed: one Ed25519 key loaded from a mounted secret,
  behind the `domain.Signer` interface. `key-management-svc` today only stores key metadata (register, rotate,
  disable) and has no sign operation, so it could not be used. Replacing the signer with a KMS/HSM-backed one is a
  new `Signer` implementation; nothing else changes. No root/intermediate hierarchy, no multi-party signing, no
  automatic rotation.
* **Rule DSL / decision tables (s38).** Not decided, so there is no formula compiler. "Code generation / normalization"
  is limited to canonicalization of the existing rule rows. The float lint covers JSON numbers in rule content only; it
  cannot see arithmetic inside a future formula language.
* **Test execution (s21, s22)** belongs to Wave 3. The report states `tests_executed: false`; `test_bundle_digest` stays empty.
* **Rule payload doctrine (OQ-1 "Model B")** was resolved in Wave 4: see [the decision record](jurisdiction-pack-decision-rule-parameters.md). `rule_payload` is
  still packaged without interpretation; calculation values travel in the typed `parameters` field.
* **Immutable artifact registry** is the `pack_artifacts` table guarded by triggers, not an object store with
  retention/WORM; the artifact's immutability depends on database privileges.

## Not built

The test/certification harness is built (see [Wave 3](jurisdiction-pack-wave3.md)); RELEASED and later states (Wave 7); the runtime resolver that consumes verified
artifacts (Wave 2); promotion rings, rollback, withdrawal and hotfix (Wave 7); SBOM scanning of the compiler toolchain;
cross-pack conflict detection (only conflicts inside one pack are detected); a code-list/schema registry for the external
dependencies; a runtime check that a loader actually calls `verify` before using an artifact.

## Verification

Unit tests (compiler rules, reproducibility, signing, domain separation, key loading, config) and the integration suite
`internal/registryit` (embedded Postgres 16; full compile-sign-verify lifecycle, every rejection path, SQL tamper
attempts, key lifecycle, 000005/000006 down-and-up round trip).
