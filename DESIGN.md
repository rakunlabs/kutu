---
name: kutu
description: Self-hosted artifact registry, file browser and gateway — a reference manual open at a tabbed section.
colors:
  oxide: "#d95d22"
  oxide-deep: "#b44a16"
  teal: "#0f8f87"
  teal-deep: "#0b746d"
  ultramarine: "#3653d3"
  ultramarine-deep: "#2c44b8"
  vermilion-errata: "#b32d1b"
  acetate-ivory: "#fffdf9"
  leaf-ground: "#f1ede4"
  leaf-rule: "#cbc3b2"
  ink: "#2a2823"
  ink-muted: "#6c665a"
  print-ink-board: "#191816"
  print-ink-leaf: "#22201d"
  print-ink-rule: "#312e2a"
  print-ink-header: "#100f0e"
  paper-text: "#e6e0d4"
  state-ok: "#2f7d4f"
  state-warn: "#a86a00"
typography:
  page-title:
    fontFamily: "'Archivo Variable', system-ui, sans-serif"
    fontSize: "22px"
    fontWeight: 700
    letterSpacing: "-0.01em"
  section-label:
    fontFamily: "'Archivo Variable', system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 600
    letterSpacing: "0.08em"
    fontVariation: "'wdth' 68"
  body:
    fontFamily: "'Archivo Variable', system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  body-small:
    fontFamily: "'Archivo Variable', system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 400
  label-caps:
    fontFamily: "'Archivo Variable', system-ui, sans-serif"
    fontSize: "11px"
    fontWeight: 600
    letterSpacing: "0.08em"
    fontVariation: "'wdth' 68"
  data:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "13px"
    fontFeature: "'tnum' 1"
rounded:
  tag: "2px"
  base: "3px"
  pill: "999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "16px"
  lg: "24px"
  xl: "40px"
components:
  button-primary:
    backgroundColor: "{colors.ultramarine-deep}"
    textColor: "{colors.acetate-ivory}"
    rounded: "{rounded.base}"
    height: "32px"
    padding: "0 12px"
  button-secondary:
    backgroundColor: "{colors.acetate-ivory}"
    textColor: "{colors.ink}"
    rounded: "{rounded.base}"
    height: "32px"
    padding: "0 12px"
  button-danger:
    backgroundColor: "{colors.vermilion-errata}"
    textColor: "{colors.acetate-ivory}"
    rounded: "{rounded.base}"
    height: "32px"
  input:
    backgroundColor: "{colors.acetate-ivory}"
    textColor: "{colors.ink}"
    rounded: "{rounded.base}"
    height: "32px"
    padding: "0 10px"
  leaf:
    backgroundColor: "{colors.acetate-ivory}"
    rounded: "{rounded.base}"
  tag:
    textColor: "{colors.ink-muted}"
    typography: "{typography.label-caps}"
    rounded: "{rounded.tag}"
    height: "20px"
  section-tab-active:
    backgroundColor: "{colors.oxide}"
    textColor: "{colors.acetate-ivory}"
    typography: "{typography.label-caps}"
    height: "40px"
---

# Design System: kutu

## Overview

**Creative North Star: "The Tabbed Reference Manual"**

kutu reads like a boxed-software reference manual lying open at a tabbed section. Every top-level area is a divider of one full-strength hue — Registries is oxide orange, Files is teal, Settings is ultramarine — and the working content sits above it on flat ivory leaves (light) or print-ink leaves (dark). The operator should know where they are from colour alone, before reading a word.

It is an operator's tool, so the world contributes only type, palette, density and one signature move; navigation, tables, forms and dialogs stay the standard web controls people already know. Density is real but legible: one body size for lists, labels in condensed tracked capitals, every measured value in tabular mono.

Rejected: the neutral grey dev-console with a single blue accent, and the previous scatter of 9–11px text.

**Key Characteristics:**
- One hue per section, resolved through `[data-section]`; `accent-*` utilities always mean "this section's hue".
- Stepped index tabs in an ink header that run into an 8px full-width hue board.
- Flat ivory/print-ink leaves with 1px rules and 3px corners; no decorative shadow.
- Condensed caps (`wdth` 68) for every label, tag and column head.
- State is a word plus a mark (filled dot, hollow dot), never colour alone.

## Colors

Three saturated section hues over warm-tinted neutrals; vermilion is held back for errata.

### Primary (section hues)
- **Oxide** (#d95d22): Registries. Tab, board, selection mark, primary buttons, icons in that section.
- **Teal** (#0f8f87): Files. Same roles inside the file browser.
- **Ultramarine** (#3653d3): Settings and the unlock screen.

Each hue carries a full 50–950 scale (see `global.css`, `[data-section=…]`); primary buttons use step 600 in light and 500 in dark.

### Tertiary
- **Vermilion Errata** (#b32d1b): destructive buttons, delete icons, errors. Never decorative.

### Neutral
- **Acetate Ivory** (#fffdf9): leaves (panels, rows, inputs) in light mode.
- **Leaf Ground** (#f1ede4): page ground behind leaves.
- **Leaf Rule** (#cbc3b2): 1px rules and input strokes.
- **Ink** (#2a2823) / **Ink Muted** (#6c665a): text and secondary text.
- **Print-Ink Header** (#100f0e): the permanent header bar in both themes.
- **Print-Ink Board / Leaf / Rule** (#191816 / #22201d / #312e2a): dark-mode ground, leaves and rules.
- **State OK / Warn** (#2f7d4f / #a86a00): running, writable / unreachable, caution.

### Named Rules
**The One Hue Per Section Rule.** A screen shows only its own section hue. Never hard-code another section's colour; use `accent-*` and let `[data-section]` resolve it.

**The Errata Rule.** Vermilion appears only where something is destroyed or broken.

## Typography

**Body / UI Font:** Archivo Variable (self-hosted via `@fontsource-variable/archivo`, width axis 62–125%), falling back to system-ui.
**Data Font:** system monospace stack with tabular figures.

**Character:** One grotesque family does everything; rank comes from weight, case and width, not from a second display face.

### Hierarchy
- **Page title** (700, 22px, -0.01em): one per pane — "Raw mounts", repository name.
- **Section label** (600, 13px, caps, `wdth` 68, 0.08em): sub-sections such as SERVERS, SHARES.
- **Body** (400, 14px, 1.5): descriptions, capped at 65ch.
- **Body small** (13px): row secondary text, hints, form fields.
- **Label caps** (600, 11px, caps, `wdth` 68, 0.08em): column heads, title-block labels, tags.
- **Data** (mono, 13–14px, tabular): prefixes, paths, hashes, sizes, endpoints.

### Named Rules
**The Twelve-Pixel Floor Rule.** Running text never goes below 12px; 11px is reserved for condensed caps labels.

## Layout

App shell: a 48px ink header with stepped tabs and an 8px hue board, then a full-height work area. Registries and Files are multi-pane (list / list / detail, tree / viewer / info); Settings is a 224px index plus a 1024px-max content column with 32px gutters. Below 768px, panes collapse to master–detail (Registries shows a "← Repositories" back control), the Settings index becomes a horizontally scrolling tab row, and table headers hide in favour of self-labelled row cards. Spacing steps are 4 / 8 / 16 / 24 / 40px; sections within a panel are 40px apart.

## Elevation & Depth

Flat. Depth comes from tonal layering: header ink, then ground, then leaf. Shadows appear only on things that float above the page.

### Shadow Vocabulary
- **Floating** (`box-shadow: 0 12px 40px -8px rgb(0 0 0 / 0.35)`): confirmation dialog.
- **Popover** (`box-shadow: 0 8px 24px -6px rgb(0 0 0 / 0.3)`): context menus and toasts.

### Named Rules
**The Flat Leaf Rule.** Panels, rows and cards never carry a shadow at rest.

## Shapes

3px corners on every leaf, button and input; 2px on tags; fully round only for status dots, switches and the punched-hole marker. Borders are 1px; dashed 1px rules split an editor's basic fields from its backend-specific fields, and mark empty-state leaves.

## Components

### Buttons
- **Shape:** 3px, 32px tall (28px `btn-sm`), 13px semibold.
- **Primary:** section hue 600 on ivory text; hover goes to 700. Dark mode uses 500 and lightens on hover.
- **Secondary:** ivory with a leaf-rule stroke; hover darkens the stroke to ink.
- **Ghost:** transparent; hover fills with ground.
- **Danger / Danger-ghost:** vermilion fill, or vermilion text with a tinted hover.
- **Motion:** 90ms `steps(2)` on colour changes; no easing curves.

### Tags
- **Style:** 20px, 1px rule border, condensed caps 11px, muted ink. Used for kind, protocol, "read-only", "TLS", "dev build".

### Status
- **Style:** 12px semibold word preceded by an 8px dot: filled for OK / warn / error, hollow ring for off/stopped.

### Leaves / Containers
- **Corner Style:** 3px. **Background:** acetate ivory / print-ink leaf. **Border:** 1px leaf rule. **Shadow:** none.
- List rows inside leaves are separated by 1px rules; an open inline editor tints its row with section hue 50.

### Inputs / Fields
- **Style:** 32px, 1px rule, 3px corners, ivory (light) / ink-950 (dark).
- **Focus:** border becomes section hue 500 with a 3px 18% hue ring.
- **Disabled:** ground fill, muted text. Hints sit below in 12px muted text.

### Navigation
- **Header tabs:** condensed caps 12px with icon. Inactive tabs sit 6px lower in muted warm grey; the active tab steps up, fills with its hue and joins the hue board. Below 640px only icons show, with the label kept for screen readers.
- **Settings index:** each item is a title plus a one-line hint; the active one gets a tinted row and a filled punched hole.
- **List selection (`.sel`):** section hue 50 ground plus a 3px hue mark at the left edge.

### Section Tab + Hue Board (signature)
The stepped section tab running into the full-width 8px board is the one move the world contributes. It is the only place the hue appears at full strength over a large area.

### Confirm Dialog
A native `<dialog>` used in place of `window.confirm`. It names what breaks and what stays untouched. Destructive confirms use the danger button and put focus on Cancel.

## Do's and Don'ts

### Do:
- **Do** use `accent-*` utilities or `var(--sec-*)` for anything section-coloured.
- **Do** use the shared classes in `global.css` (`.btn`, `.input`, `.field`, `.tag`, `.status`, `.leaf`, `.sel`, `.label-caps`) instead of new class strings.
- **Do** put the section in the URL (`/settings/:section`) so reloads and shared links land in place.
- **Do** confirm destructive actions with `confirmAction()`, naming the consequence and what is preserved.

### Don't:
- **Don't** use `window.confirm` / `alert`.
- **Don't** set running text below 12px, or use uppercase labels without the condensed caps class.
- **Don't** hard-code hex colours in components, or reintroduce `gray-*` or `rounded-lg` surfaces.
- **Don't** signal state by colour alone; pair it with a word or a mark.
- **Don't** load fonts from a CDN; kutu can run air-gapped.
