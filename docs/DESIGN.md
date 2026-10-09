# Waybill — Design

A waybill is the document that travels with a shipment and records every hand it passes through. The interface borrows from that object: paper, ink, stamps, tear-off receipts, tracking numbers. It avoids the generic fintech look: no blue gradients, no glass cards, no dark-mode-first dashboards.

## 1. Principles

1. **Plain words.** "Sent to your bank", not "payout settled". "We're checking with the bank", not "status unknown".
2. **The tracking page comes first.** If a choice helps the tracking page and hurts another screen, the tracking page wins.
3. **Mobile first, cheap phone first.** The reference device is a low-end Android phone on a slow 4G connection.
4. **Every screen has five states:** ready, loading, empty, error and slow network. A screen is not done until all five are designed.
5. **Colour carries no meaning on its own.** Every status has a word and a mark as well.

## 2. Design tokens

Defined once as CSS custom properties in `web/src/app/globals.css` and exposed to Tailwind through `@theme`.

### Colour

| Token | Value | Use | Contrast on `paper` |
|---|---|---|---|
| `--paper` | `#F6F3EC` | Page background | — |
| `--paper-raised` | `#FBF9F4` | Receipt and card surface | — |
| `--ink` | `#1A1A17` | Body text, rules, stamps | 15.7 : 1 |
| `--ink-muted` | `#5C5A52` | Secondary text | 6.2 : 1 |
| `--rule` | `#D6D0C2` | Hairlines, perforations (decorative only) | — |
| `--delivered` | `#1F6B45` | **Only** for "delivered" (sent to bank, paid in full) | 5.8 : 1 |
| `--focus` | `#1A1A17` | 3 px focus outline with 2 px `paper` offset | — |

The `delivered` green is the only accent. Errors, warnings and pending states use ink, a distinct stamp shape and words, not a second colour (ADR-025). An invalid form field gets a 3 px ink left rule, a "!" glyph and its message directly below, linked by `aria-describedby`. This is more noticeable on paper than a thin red outline, and it reads the same for colour-blind users and in greyscale print. Contrast ratios are WCAG 2.1 relative-luminance values computed against `--paper`; axe re-checks them in CI from stage 1.

Dark mode *(stage 6)*: "carbon copy". `--paper` becomes `#16150F`, `--ink` becomes `#ECE8DD`, and `--delivered` becomes `#5FBF8A`. Contrast is re-verified before release.

### Type

| Role | Face | Sizes (mobile → desktop) |
|---|---|---|
| Text and headings | **IBM Plex Sans** (400, 600) | 16 / 20 / 24 / 32 px |
| Tracking numbers, amounts, addresses, hashes | **IBM Plex Mono** (400, 500), `font-variant-numeric: tabular-nums` | 14 / 16 / 28 px |

Both faces are self-hosted through `next/font`, subset to Latin and the naira sign (₦, U+20A6), with `display: swap` and metric-matched fallbacks to keep CLS at zero. The line length cap is 68 characters.

### Space, shape and motion

- 4 px base grid. Spacing steps: 4, 8, 12, 16, 24, 32, 48.
- Corners: 2 px. Paper does not have rounded corners.
- Borders: 1 px `--ink` for structure, 1 px dashed `--rule` for perforations.
- Motion: the only animation is a stamp landing (scale 1.08 → 1, 120 ms). Under `prefers-reduced-motion: reduce` the stamp appears without motion.

## 3. Motifs

### Stamps

A status is shown as a rubber-stamp mark: an uppercase label in Plex Sans 600, letter-spaced, inside a border, rotated −2° and printed at 90% opacity.

| Stamp | Border | Colour | Meaning |
|---|---|---|---|
| `RECEIVED` | single | ink | Payment seen |
| `CONFIRMED` | double | ink | Safe from being undone |
| `CONVERTED` | single | ink | Changed to naira at the locked rate |
| `SENT TO BANK` | double, filled | `delivered` | Done |
| `WAITING` | dashed | ink-muted | Next step not reached |
| `CHECKING` | dashed, with a clock glyph | ink | "Couldn't find out yet". Never shown as failure |
| `PROBLEM` | thick, with a "!" glyph | ink | Needs someone to act |

Every stamp is a real text element with `role="img"` and an `aria-label` that reads the full sentence, for example "Confirmed at 14:02". Rotation is decorative.

### Receipt

Receipts and invoices are drawn as a slip of `--paper-raised`, with a perforated top and bottom edge made from a CSS `radial-gradient` mask (no images). Amounts are right-aligned in mono. The tracking number is printed at the top like a waybill number: `WB-7KQ4-M2XD-9PRA-3HNC`. The print stylesheet removes the perforation and keeps the content.

### Tracking line

A vertical line with four stations: received, confirmed, converted, sent to bank. Reached stations are solid ink dots. The current station has a stamp. Future stations are hollow. Each station shows its time and one plain sentence.

## 4. Key screens

Each screen lists its states. "Slow" means data has not arrived after 2.5 s.

### 4.1 Tracking page — `/t/{code}` (centrepiece)

Server-rendered HTML that works with JavaScript off. SSE adds live updates when available.

| State | Shows |
|---|---|
| Ready | Tracking number (mono, large), amount in both currencies, contractor's verified name, the tracking line, "last updated" time. |
| Loading | Server-rendered, so there is no spinner on first load. Live reconnects show "Reconnecting…" next to the last-updated time. |
| Empty | Not applicable: a tracking page always has at least *created*. Before payment, the four stations are all `WAITING`, with "Waiting for the payment to arrive." |
| Error (not found) | "We can't find a payment with this tracking number. Check the link you were sent." No hint about whether the code is close to a real one. |
| Error (server) | The last known state stays on screen, with "We couldn't refresh this page. Showing the status as of 14:02." |
| Slow | The page loads without the live channel. A "Refresh" link is shown after 10 s without an update. |
| Reorg | The station goes back to `CHECKING`, with "The network undid this payment. We're watching for it again." |
| Done | `SENT TO BANK` stamp in `delivered`, and a link to the receipt. |

### 4.2 Payment link — `/pay/{code}`

| State | Shows |
|---|---|
| Ready | Who you're paying (verified name and a "Verified" mark with an explanation), what for, amount, expiry. Two choices: *Stablecoin* and *Card or bank transfer*. |
| Stablecoin chosen | Network picker, deposit address (mono, copy button, QR), exact amount, "Connect wallet" as an alternative. A warning in plain words about sending only the named token on the named network. |
| Loading | Skeleton blocks the same size as the content (no layout shift). |
| Expired | "This link expired on 12 Oct. Ask the contractor for a new one." |
| Already paid | "This invoice is paid." Link to tracking. |
| Error | "We couldn't load this payment link." Retry button. |
| Slow | Content keeps its space reserved. The "Taking longer than usual" text is announced politely. |

### 4.3 Create invoice (contractor)

| State | Shows |
|---|---|
| Ready | Form: description, amount, payout choice. If naira: the live quote with a countdown to its expiry. |
| Submitting | Button disabled, "Creating…". The idempotency key is generated once per form instance, so a double-submit creates one invoice. |
| Quote expired | "The rate changed. New rate ₦X. Continue?" |
| Error | Inline field errors, linked with `aria-describedby`. A summary at the top gets focus. |
| Done | Payment link and tracking link with copy buttons, plus share on WhatsApp. |

### 4.4 Contractor home

| State | Shows |
|---|---|
| Empty | "No invoices yet. Create your first one." One button. |
| Ready | Balances (available and pending, mono), recent invoices with stamps. |
| Loading / error / slow | Skeleton; retry; cached data with an "as of" time. |

### 4.5 Batch pay and approval (payer)

The run is shown as a manifest: lines with name, amount and reference. Lines with problems carry a `PROBLEM` stamp and a reason. The run cannot be approved by its creator. States: draft, checking, needs approval, approved, sending, sent, partly failed (named per line).

### 4.6 Proof of reserves — `/reserves`

Totals owed per asset next to on-chain holdings per network, with explorer links. A "Check my balance" form verifies a Merkle proof in the browser. States: ready, stale (with "as of"), and checking.

### 4.7 Pull the plug — `/demo`

Three large buttons (kill a worker mid-payout, force a reorg, replay a webhook), each followed by a before-and-after table of ledger totals and on-chain balances. The page exists only when the server runs in demo mode on Anvil.

## 5. Accessibility (WCAG 2.1 AA)

- Every interactive element can be reached by keyboard, in a logical order. A visible 3 px focus ring is always on.
- A "Skip to content" link on every page.
- Route changes move focus to the page `<h1>`.
- Form errors move focus to an error summary.
- Live updates are announced through one `aria-live="polite"` region ("Your payment is confirmed"), never by moving focus.
- Inputs have visible labels; placeholders are never used as labels. Amount inputs use `inputmode="decimal"` and are parsed as integers in minor units.
- Text contrast is at least 4.5 : 1, and non-text UI at least 3 : 1. Both are checked by axe in Playwright *(stage 1)*.
- Touch targets are at least 44 × 44 px.
- `prefers-reduced-motion` is honoured. No content flashes.
- Pages work at 200% zoom and 320 px width without horizontal scroll.

## 6. Performance budgets

Measured by Lighthouse CI (mobile profile, simulated slow 4G, 4× CPU slowdown) on every push from stage 1. A regression past a budget fails CI.

| Metric | Tracking page | Other pages |
|---|---|---|
| LCP | ≤ 2.0 s | ≤ 2.5 s |
| INP (lab proxy: TBT ≤ 200 ms) | ≤ 200 ms | ≤ 200 ms |
| CLS | ≤ 0.05 | ≤ 0.1 |
| JS shipped up front (gzip) | ≤ 185 KB | ≤ 185 KB |
| Total transfer, first load | ≤ 250 KB | ≤ 500 KB |

The JavaScript budget is the measured Next.js and React baseline (about 170 KB gzip for any App Router page with a client component) plus about 15 KB for Waybill's own code. The original 90 KB figure was set without measuring and is not achievable on this stack (ADR-033). The tracking page does not depend on JavaScript: it is complete server-rendered HTML, so a slow phone can read it before any script runs.

Wallet libraries (`wagmi`, `viem`) are not in any page's initial load. They load only after the payer presses "Pay with a browser wallet".

`make budget` (and the CI stack job) measures the gzipped initial JavaScript of the tracking and payment pages on the running stack, and fails on overspend or on wallet code in the initial load. LCP, INP and CLS are measured by Lighthouse once its tooling is approved.
