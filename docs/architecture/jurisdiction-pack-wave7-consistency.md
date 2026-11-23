# ZS-JUR-001 s10 and s23: cross-pack conflicts at release, and the supersede command

Builds on [Wave 7](jurisdiction-pack-wave7.md) (release lifecycle) and [Wave 2](jurisdiction-pack-wave2.md) (runtime resolver). Implemented in `jurisdiction-rules-svc` (migration `000024`; action
`JURISDICTION_PACK_VERSION_SUPERSEDE` registered in `access-control-svc` migration `000027`). No new service.

## Cross-pack conflict check at release (s10)
s10: "Two active rules overlap: certification blocker unless explicit precedence/combination rule exists." Until now only overlaps **inside one pack** were detected (compile time). Two packs that both define a rule would
build, certify and release, and then fail only when used, with `AMBIGUOUS` at runtime (JUR-NEG-24).

`POST .../versions/{v}/publish` now refuses (409 `operation_blocked`, every reason at once) a version when any of its rules or obligation rules overlaps one of **another RELEASED pack** that has:
the same rule domain and code (or the same obligation code), a jurisdiction that is the same or an ancestor or descendant (so a subdivision rule collides with its country's rule), and an overlapping effective period
(the intersection of the rule's period and its pack's period, end exclusive). The reason names the rule, both jurisdictions, the other pack and version, and the overlapping period.

* **Declared precedence does not clear it across packs.** The resolver honours `precedence` and `supersedes_rule_id` only among the rules of one pack and refuses any overlap between packs. A release check that exempted declared
  precedence would pass pairs the runtime still refuses, so it does not. Rules that must override each other belong in one pack.
* **Versions of the same pack never conflict with each other**: the resolver takes the highest version in force per pack.
* **Only live packs count.** Withdrawing the other pack lets the release go through. Unverifiable or artifact-less packs are ignored, because they cannot be served either.
* One existing test fixture (a dependent pack that re-carried the dependency's rule over the same period) was adjusted: it was exactly the pair this check exists to refuse.

## Supersede command (s23)
`POST /v1/admin/packs/{pack_ref}/versions/{version}/supersede` with `superseded_by` and `reason` (and optional `evidence_ref`). The status edge `RELEASED -> SUPERSEDED` has existed since `000005` but nothing could take it.

A superseded version stays resolvable for pinned replay but is **not eligible for new decisions**, and several released versions of one pack legitimately coexist (an older one may be the only version covering earlier dates). So
superseding is allowed only when nothing the older version served is left without a servant. The newer version must be:

| Guarantee | Blocker |
|---|---|
| RELEASED, of the same pack, strictly later in numeric order | `newer_version_not_released`, `not_newer`, same-pack |
| covers the older version's period (starts no later, ends no earlier or is open) | `period_not_covered` |
| covers every jurisdiction the older version covers | `jurisdiction_not_covered` |
| carries every rule (domain, code, jurisdiction) and obligation the older one has | `not_carried_forward` |
| ACTIVE in every ring and region where the older one is | `newer_not_deployed` |

Every blocker is reported at once. The older version must itself be RELEASED (`invalid_lifecycle_state` otherwise). The move is recorded as a `SUPERSEDE` release event whose reason names the replacing version and why, is announced
as `jurisdiction-pack.superseded`, and repeating it changes nothing and is not announced again. The release-event action list gained `SUPERSEDE`, which (like withdraw and block) requires a reason.

## What is deliberately not built
* **New tax families** (place of supply and situs, reverse charge, exemptions, compound taxes, withholding, caps and floors). The Wave 4 decision record says each is a new closed family *decided with Tax Engineering*. Writing them without
  that decision would be inventing tax law; they stay open until Tax Engineering specifies them.
* **"Strictest minimum across the jurisdiction chain" for retention** (listed as a Wave 6 gap). The s18 text does not ask for it: it asks that a tenant may extend but never shorten a mandatory minimum, which is built. When a national and a
  subdivision retention rule both apply, the resolver already refuses the overlap unless precedence is declared in one pack, which is what s10 requires. A combination rule would be a new design, not a missing check.
* **A regime flag permitting subdivision override** (s10 "only when the regime permits"): declared precedence in one pack is the existing explicit control; a regime-level permission on top would be a further constraint and needs
  Legal and Tax Leadership to say which regimes permit it.
* **Cross-pack precedence at runtime**, so a country pack and a subdivision pack could coexist with the subdivision winning. That is a resolver design change (and the s38 "rule DSL" decision), not a gap in these checks.
* **Automatic supersession** when a newer version is released, and rolling the older version's deployments back. Both are decisions for the release owner; the command is explicit and refuses unless it is safe.

## Deploy order
Apply migration `000024` before the new binary, and `access-control-svc` `000027` so the action is known. The `000024` down script re-narrows the release-event action list as `NOT VALID` (release events are append-only), so a
version already superseded stays superseded.

## Verification
Unit (`pack_consistency_test.go`): conflict detection (undeclared overlap, symmetry, different code or domain, unrelated country, rule ended first, pack window ended, same pack other version, the exclusive end instant, one day of overlap, parent and
subdivision, declared precedence and supersedes links not helping across packs, obligations), and every supersession guarantee, numeric version order, an older version with an end, and all blockers reported together. Integration (embedded Postgres 16):
the full supersede path (validation, refused while the newer is not released, success with event and release-history reason, repeat, pinned replay versus new decisions, backwards and non-released refusals); the deployment gap and its fix; a colliding
pack refused at release and accepted once the other is withdrawn; non-overlapping periods accepted; and the `000005`-`000024` down/up round trip including the action constraint.
