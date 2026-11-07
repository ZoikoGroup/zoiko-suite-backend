# ZoikoSuite Assistant: UI specification for Design approval

Source of truth for sizes: `zoiko-suite-frontend-platform/components/assistant/assistant.css`
(every value is a `--za-*` CSS variable). **Doc01** marks a rule taken from Document 01 (Product
Requirements and Wireframe §06, §07 W01, §08 W02, §10, §17); **proposed** marks an engineering
choice for Design to approve or change. Colours reuse the existing brand palette (navy, slate,
amber, red, green); no new brand colour was introduced.

## Launcher (S00)

| Token | Value | Basis |
|---|---|---|
| Accessible name | "Chat with ZoikoSuite"; open: "Close chat" | Doc01 §06 |
| Position | fixed bottom-right, safe-area aware | Doc01 §06 |
| `--za-launcher-margin` / `-desktop` | 16 px mobile / 20 px ≥ 768 px | proposed |
| `--za-launcher-size` / `-lg` | 56 px / 64 px at ≥ 1024 px | proposed |
| `--za-launcher-min-target` | 44 × 44 px | Doc01 §06 |
| `--za-launcher-radius` | 16 px (matches the tile's corners) | proposed |
| `--za-launcher-offset` | `LAUNCHER_BOTTOM_OFFSET` + height of any detected cookie banner / sticky footer at the bottom | Doc01 W01-A |
| `--za-close-badge` | 18 px | proposed |
| Unread dot | 10 px, only for a reply that lands while closed | Doc01 §06 |
| Focus ring | 2 px, `--za-focus` (≥ 3:1) | Doc01 §17 / WCAG |
| Hover/focus scale | 1.04, none under reduced motion | proposed |

## Desktop panel (≥ 768 px, W01)

| Token | Value | Basis |
|---|---|---|
| Layout | right-side panel, page stays visible | Doc01 §06, W01 |
| `--za-panel-right` | 24 px | proposed |
| Bottom | launcher height + 32 px (`--za-panel-gap-above-launcher`) | proposed |
| `--za-panel-width` (min / max) | 400 px (360 / 440) | proposed |
| `--za-panel-width-expanded` | 560 px via Expand/Minimize | Doc01 §06 (expand/minimize) |
| `--za-panel-height` | min(680 px, 100dvh − 96 px), min 480 px | proposed |
| `--za-panel-radius` | 16 px, 1 px border, soft shadow | proposed |
| Regions | header (fixed) · conversation (only scroll area) · composer (fixed) | proposed |
| `--za-scrollbar` | 6 px themed | proposed |

## Mobile sheet (< 768 px, W02)

| Token | Value | Basis |
|---|---|---|
| No horizontal scroll | 320–430 CSS px | Doc01 §08 |
| `--za-sheet-height` | 88dvh bottom sheet; full viewport at ≤ 360 px | Doc01 §06 (sheet) / proposed (88dvh) |
| `--za-sheet-radius` | 12 px | proposed |
| Drag handle | 36 × 4 px | proposed |
| Keyboard | VisualViewport sets `--za-kb-inset`; composer stays above the keyboard | Doc01 §06 |
| Back button | closes the sheet first | Doc01 §08 |
| Close | focus to launcher; page scroll restored; background scroll locked only while open | Doc01 §06, §08 |
| Bubbles / CTAs | 92 % bubbles, full-width CTA | Doc01 §06 |

## Header

| Token | Value | Basis |
|---|---|---|
| `--za-header-height` | 64 px | proposed |
| `--za-header-tile` | 32 px, radius 8 px | proposed |
| Title | "ZoikoSuite Assistant", 16 px / 500, one line | Doc01 §06 |
| Subtitle | 12 px "AI assistant. Public ZoikoSuite information only."; ≤ 380 px panel width: "AI assistant. Public information only." | Doc01 §06 (disclosure) / proposed (short copy) |
| Controls | Info, Clear chat, Expand/Minimize (≥ 768 px), Close | Doc01 §06 |
| `--za-control-touch` / `--za-control-fine` | 44 px touch / 32 px pointer-fine; icon 20 px; gap 8 px | Doc01 §06 (44) / proposed (32) |
| "More" menu | Info + Clear chat fold into it at ≤ 360 px panel width **and on touch devices** | proposed (see conflicts) |

## Typography and spacing (proposed, rem-based)

Title 16 px; body 15 / 22 px; sources and chips 13–14 px; meta 12 px (never smaller). Spacing
4 / 8 / 12 / 16 / 24. Panel padding 16 px. Bubbles: padding 10 × 14 px, radius 16 px with a 4 px
tail corner, max width 88 % desktop / 92 % mobile, 12 px between bubbles, 16 px between turns.

## Welcome (S01)

| Item | Value | Basis |
|---|---|---|
| Content | Z tile 48 px, title, 1–2 line greeting, AI disclosure | Doc01 §06, §10 |
| Starter prompts | max four on desktop; 3 on mobile; rest behind a "More topics" text button | Doc01 §06 |
| Chips | min height 40 px pointer / 44 px touch; 14 px text; 8 px gap; pill; wrap, no carousel | Doc01 §06 (44, no carousel) / proposed |
| Focus | composer on desktop; heading on mobile (no unasked keyboard) | proposed |

## Conversation and answers

Answer order: text → status badge (icon + text) → up to 3 source cards (44 px rows, title only,
no raw domain) → one primary CTA (≥ 44 px, full width on mobile) → feedback row (32 px visible,
44 px hit area). Restricted markdown only (bold, lists, inline code). A new answer scrolls to its
start; when the visitor has scrolled up, a 44 px "Jump to latest" pill appears instead. Scroll
position is kept across close/reopen. One polite live region announces each answer once.

## Composer

| Item | Value | Basis |
|---|---|---|
| Growth | one line (44 px) to four lines (112 px), then scrolls inside | Doc01 §06 |
| Keys | Enter sends, Shift+Enter newline, IME composition never sends | Doc01 §06 |
| Text | 15 px; 16 px on touch (no iOS zoom) | proposed |
| Send | explicit 44 × 44 button; becomes "Stop" while a turn runs | Doc01 §06, §10 S02 |
| Limit | 2,000 characters; counter from 1,800 | Doc05 / proposed |
| Sensitive data | amber warning above the composer; send blocked; text editable; "Remove it" | Doc01 §06 (see conflicts) |
| Footer | privacy line + notice link; info icon only when height < 520 px | proposed |

## States (Doc01 §10)

S00 launcher only · S01 welcome · S02 "Checking approved sources…" + 3 dots (static under reduced
motion) + Stop · S03 grounded answer · S04 one question + options + "Show general overview" · S05
"I couldn't verify that" + amber "Unverified" + routes + Try again · S06 amber safety panel · S07
expected channel, no wait time · S08 red error, Try again, official links, "Reference: <id>" with
Copy, offline banner · S09 end card: rating, reason chips, "Retention notice", Clear chat.

## Motion

Open/close 180 ms opacity + transform; expand 150 ms; none under `prefers-reduced-motion`.

## Colour roles and contrast (both themes)

| Role | Light | Dark |
|---|---|---|
| User bubble | navy-900 | navy-600 |
| Assistant bubble | slate-100 | slate-800 |
| Control borders (`--za-control-border`) | slate-500 (4.8:1) | slate-400 (≥ 5:1) |
| Focus (`--za-focus`) | navy-500 (6.3:1) | navy-300 |
| Warning / error | amber-50 + amber-900 / red-50 + red-900 | amber-950 + amber-100 / red-950 + red-100 |

Text ≥ 4.5:1 and UI ≥ 3:1 are checked by Playwright (axe plus a token contrast check).

## Conflicts found (Doc01 vs brand tokens or layout), and how they were resolved

1. **Focus colour in dark mode.** The brand focus blue (navy-500) is about 2.8:1 on the dark panel,
   below the 3:1 Doc01/WCAG floor. Dark mode uses navy-300 from the same palette.
2. **Control borders.** The previous slate-200/300 borders were 1.2–1.5:1 on white. Raised to
   slate-500 (light) / slate-400 (dark) via `--za-control-border`.
3. **Launcher edge.** The specified `rgba(0,0,0,0.12)` edge is about 1.3:1 on the light page. It is
   kept as specified because the tile's own navy and gold artwork identifies the control; Design
   may want a stronger edge.
4. **One-line title vs 44 px targets.** At 320 px, or on touch tablets with four 44 px controls,
   "ZoikoSuite Assistant" cannot stay on one line beside the 32 px tile. Resolved by folding Info
   and Clear chat into "More" on touch devices as well as at ≤ 360 px, and hiding the decorative
   header tile below 340 px of panel width.
5. **Disclosure on one line.** "AI assistant. Public ZoikoSuite information only." does not fit at
   ≤ 380 px of panel width; the approved short copy "AI assistant. Public information only." is
   used there instead of clipping.
6. **"Max four quick-action prompts" vs "More topics".** "More topics" is a text button, not a
   fifth chip.
7. **"Composer disabled while safety block".** Disabling the textarea would stop the visitor
   removing the sensitive text. Sending is blocked; the text stays editable with "Remove it".
8. **S07 CTA.** No approved demo or support URL exists, so escalation shows the channel text
   without a button until the route registry has real URLs.
