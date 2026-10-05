---
version: 1
slug: "src-app-svelte"
primary_target: "src/App.svelte"
related_targets: []
---

Scope: whole kutu web UI (shell, Registries, Files, Settings). Mode: Operate.
Audience: homelab operators and platform/devops teams; most frequent jobs are browsing registries and files, configuration is rarer but high-stakes.

## Direction contract

THESIS: kutu reads like a boxed-software reference manual open at a tabbed section: each top-level area is a divider of one full-strength hue, and the working leaves sit above it. Refuses the neutral grey dev-console with one blue accent.

OWN-WORLD: Section hues at full strength — Registries oxide orange, Files teal, Settings ultramarine — paint the header tab, the board strip and every accent of that area. Leaves are milk-acetate ivory (light) or print-ink (dark). Labels in condensed tracked caps (Archivo, self-hosted), body in the same family at normal width, data in system mono. 1px ink rules, 3px corners, flat; selection marked with a punched-hole dot.

STORY: The operator always knows which section they are in from the colour alone, scans dense lists at one legible size, and edits configuration in place without losing their spot.

FIRST VIEWPORT: Ink header with logo left and three stepped index tabs; the active tab is taller, filled with its hue and joins a full-width hue board strip. Below, the section's own panes. Settings: left sub-nav with punched-hole marker, ruled list rows with inline editing, title-block About.

FORM: Acetate tab manual (catalog rw-manual-acetate-tab-board), challenger chosen on re-roll 1; seed key 2f9c2500. Signature move: the stepped section tab joining its hue board; motion is 90ms two-step, no easing.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
