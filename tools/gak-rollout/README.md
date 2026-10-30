# gak-rollout

Prepares one tenant for the Global Accounting Kernel in `general-ledger-svc`: the accounting book, the account mappings and the rule
packs, and then checks that everything those packs name can actually resolve. It does not switch any service over; it prints what to
set.

- Talks to `general-ledger-svc` **over its HTTP API only**. It never touches a table.
- **Dry run by default.** Without `--apply` it only reads and prints the plan.
- **Never forces.** It does not overwrite a mapping, reactivate a suspended book, change a book's identity, or approve or activate a rule
  set. Anything it cannot do cleanly is reported (`CONFLICT`, `BLOCKED`, `FAILED`) and left alone.
- **Safe to repeat.** Run it again after fixing something; what is already in place is reported `OK`.
- Principals are ids only. There is no credential in this tool.

## What it does

1. **Book.** Finds the entity's book by code. Missing: creates it and activates it. A draft: activates it. Active: leaves it. A book
   that exists with another type, framework or currency (a book's identity is fixed), or that is suspended or retired, is reported for a
   person to decide.
2. **Mappings.** For each mapping key in the configuration: already mapped to that account is `OK`; mapped to a different account is a
   `CONFLICT` (a mapping decides which account postings land on, so it is changed deliberately through the chart administration, never
   here); not mapped is created. A key that no selected pack uses is warned about, which catches a typo.
3. **Packs.** Instantiates each pack's rules as **drafts**. A pack that is already instantiated at that version is `OK`.
4. **Readiness.** Asks the kernel (`GET /v1/rollout-readiness`) whether every mapping and dimension the pack names resolves, and lists
   what does not, with the first remedy.

## A mapping is in force from the day it is made

A mapping created through the API takes effect that day and cannot be backdated. So readiness is checked as of the later of the pack's
`effective_from` and today, and the tool warns when a pack is dated earlier than the mappings it has just made: events dated before that
day will not resolve them (`AK-COA MAPPING_MISSING`). Make a pack effective from the go-live date, on or after the day its mappings are
made.

## Running it

```sh
cd tools/gak-rollout && go build -o gak-rollout .

# see the plan
./gak-rollout --base http://127.0.0.1:8098 --tenant <tenant-id> --principal <principal-id> --config rollout.json

# make the changes
./gak-rollout --base http://127.0.0.1:8098 --tenant <tenant-id> --principal <principal-id> --config rollout.json --apply
```

`--principal` is the identity the calls are made as. It needs the book-management, rule-management and account-mapping rights.
`--json` prints the report as JSON. Exit status: `0` all in place or planned; `1` something needs a person; `2` usage or a configuration
that cannot describe a rollout.

## The configuration

```json
{
  "legal_entity_id": "<the legal entity>",
  "book": {
    "book_code": "STAT",
    "book_type": "PRIMARY_STATUTORY",
    "accounting_framework_id": "IFRS",
    "functional_currency": "GBP",
    "fiscal_calendar_id": "<calendar>",
    "chart_of_accounts_id": "<chart>",
    "default_ledger_id": "<ledger>",
    "effective_from": "2026-01-01"
  },
  "mappings": {
    "AR_CONTROL": "1100",
    "REVENUE": "4000"
  },
  "packs": [
    { "name": "ar-standard", "version": "1.0.0", "effective_from": "2026-11-01" }
  ]
}
```

`GET /v1/posting-rule-packs` lists the packs and the mapping keys each one needs. Unknown fields are refused, so a misspelt option is an
error and not a silent default.

## After it

When it finishes clean, it prints the two values to set:

```
LEDGER_KERNEL_BOOK_ID=<the book's id>      payroll-run-svc, asset-management-svc, inventory-management-svc
LEDGER_KERNEL_CURRENCY=GBP                 asset-management-svc, inventory-management-svc
```

and what is still for people: test each instantiated rule set, approve it (someone other than its author), activate it (a third
person), run this tool again until every readiness line is `OK`, and only then switch one domain at a time. The close gates
(`KERNEL_CLOSE_GATE_MODE`, `KERNEL_PERIOD_SEAL_MODE`, `KERNEL_REQUIRED_RECONCILIATIONS` in financial-close-svc) are documented in
`.env.example`; leave them `off` until the domains are posting.
