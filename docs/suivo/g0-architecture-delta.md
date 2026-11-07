# G0 Architecture Delta (STEP 0)

Branch: suivo/r0-build (from main at commit 180b9366)

## Repo facts (verified)
- Go 1.25, services/ with module zoiko.io/<name>
- Patterns: chi v5, pgx v5, kafka-go, zap, uuid; layout cmd/server, internal/*, deployments/migrations/*.up.sql/*.down.sql
- CI: .github/workflows/ci.yml per-service matrix (go mod verify, go build, go vet, tests) with Postgres 16 service container
- Existing: services/assistant-rag-svc (read-only for conventions; must NOT reuse DB/index/topics/creds/runtime)
- Frontend: none in repo

## Discovery matrix
| Need | Status | Repo path / evidence | Next action |
|---|---|---|---|
| HTTP framework | EXISTING | github.com/go-chi/chi/v5 (used across services) | Use chi v5 in services/public-ai-svc |
| DB (Postgres) | EXISTING | pgx/v5 used; CI has Postgres 16 service | Use own schema public_ai with dedicated role/creds (PUBLIC_AI_DATABASE_URL) |
| Migrations | EXISTING | deployments/migrations/NNNNNN_*.up.sql/.down.sql pattern | Add services/public-ai-svc/deployments/migrations/ |
| Kafka/outbox | EXISTING | kafka-go used; eventing module exists | Implement transactional outbox in service; publisher to Kafka; follow CloudEvents 1.0 |
| Config | EXISTING | pattern via env/config per service | Add internal/config with Section 7 defaults (dev) |
| Logging/telemetry | EXISTING | go.uber.org/zap used; OTel referenced in docs | zap + redaction; OTel attributes per Doc 08 App B (no raw prompts/responses) |
| Health | EXISTING | cmd/healthcheck pattern | Implement live/ready/startup + /public-ai/v1/health/public |
| CI | EXISTING | .github/workflows/ci.yml matrix | Add public-ai-svc to matrix in STEP 1 |
| Secrets | EXISTING | env-based; no .env committed | .env.example only; signing keys from env (dev) |
| Search (R0) | MISSING/TO IMPLEMENT | No dedicated PUBLIC_APPROVED namespace reused | Postgres FTS (lexical) only in R0; interface for future vector |
| Model provider | MISSING | No public-ai provider | Fake provider until DEC-05; interface model.Provider |
| Signing (revocation) | MISSING | — | Ed25519 dev signing (D08-03) via env; verify on load |
| Widget host | MISSING | No frontend in repo | Ask in STEP 13; build in web/public-ai-widget/ only if told |

## Blockers (permanent)
DEC-01 hosting/gateway; DEC-02/A04 taxonomy; DEC-03/A05 price/trial/claims; DEC-04 name/logo; DEC-05 LLM/vendor/region/DPA; DEC-06 retention/cookies/DPIA; DEC-07 TTLs; DEC-08 SLOs; DEC-09 signing/revocation owner; DEC-10 handoff destinations; DEC-16 GO/STOP/rollback authority.
