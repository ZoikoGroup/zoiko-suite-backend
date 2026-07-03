@'
---
name: service-spec
description: Use when creating or scaffolding a new ZoikoSuite microservice.
---

Before writing any code for a new service, produce a spec block with:
- Service name, service class, business purpose
- Owned objects (pull exact field names from docs/architecture/04-data-model.md
  if the entity already exists there)
- Inbound APIs / Outbound APIs
- Published events / Consumed events
- Governance dependencies (which Governance Plane engines this service calls)
- Evidence obligations
- Idempotency requirement
- Failure mode (fail closed / fail safe / degraded / compensating saga)

Only after I approve this spec, scaffold the service code, tests, and an
OpenAPI stub. Reference docs/architecture/03-microservices.md §08 for the
canonical examples (Policy Service, Authorization Service).
'@ | Set-Content -Path ".agents\skills\service-spec\SKILL.md" -Encoding utf8

git add .agents\
git commit -m "Add doctrine rule, agent personas, and service-spec skill"