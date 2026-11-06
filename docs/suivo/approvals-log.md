# APPROVALS LOG - ZoikoSuite Suivo Chatbot
Created: 2026-10-27 11:52:48
Branch: suivo/phase-0-audit

| Date | Decision | Approver (role/name) | Evidence link | Status | Notes |
|---|---------|---------------------|---------------|--------|-------|
| 2026-10-27 | Phase 0 audit complete - ready for review | pending - role: Owner/CTO/Product (as specified in docs) | docs/suivo/00-document-register.md through 05-roadmap-wbs.md | Pending | Awaiting approval to proceed to Phase 1 |
| 2026-10-28 | Assistant widget shown in the admin console (`/assistant`, `/admin`) instead of the public website (Docs 01/11 intend the website) | Product owner (name to confirm) | `components/assistant/mount.ts`, `r0-readiness.md` | Pending confirmation | Recorded from the engineering session; needs the named owner's sign-off |
| 2026-11-03 | Conversation history: tab-only by default; opt-in "Remember my chats on this device" | Product owner (name to confirm) | `components/assistant/history.ts`, `data-inventory.md` | Pending confirmation | Expiry 24 h until DEC-06 sets the retention value; DPO to confirm whether a consent entry is needed |

No gate (G0–G5) has an approval yet; see `r0-readiness.md`. Open items per document: `gap-list-2026-11-05.md`.
