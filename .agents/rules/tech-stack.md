---
trigger: always_on
---

# .agents/rules/tech-stack.md
- Tier 0 latency-critical services (Identity, Authorization, Policy): Go
- Intelligence Plane services (forecasting, anomaly, risk scoring): Python
- All other domain services (Finance, HR, Legal, Tax, Commercial Ops): Node/TypeScript (NestJS)
- No service may pick a language outside this list without explicit approval.