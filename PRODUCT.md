# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Two audiences, both confirmed:

- **Homelab operators / small teams** who self-host kutu on their own box. They set it up once, then mostly come back to look at packages or files.
- **Internal platform / DevOps teams** serving artifacts to CI/CD and developers across a company, for whom the UI is a daily operational tool.

The most frequent jobs are **browsing registries** (finding a package, its versions and files, checking proxy state) and **browsing files** on raw mounts (view, download, upload). Configuration (mounts, file servers, shares, users, listeners, encryption) is less frequent but high-stakes.

## Product Purpose

A self-hosted artifact registry, file browser and gateway shipped as one Go binary with an embedded web UI, backed by PostgreSQL. Success: an operator can find any artifact or file in seconds, and can wire storage, protocols and access without reading docs.

## Positioning

One binary covers what normally takes several products: package registries (npm, Go, Docker/OCI, Helm, Maven, PyPI, Cargo — proxy or hosted), a multi-backend file browser (local, S3, FTP, SFTP, WebDAV, Vercel Blob), protocol serving of those files (FTP, SFTP, TFTP, WebDAV, S3 API), reverse-proxy graphs, hooks, and at-rest encryption unlocked from the UI.

## Operating Context

- Runs on the operator's own infrastructure; may be on internal or air-gapped networks.
- No authentication layer: the UI sends an editable `X-User` header for audit attribution only.
- When encryption is initialized, the whole UI is replaced by an unlock screen until the key is entered.
- Everything is reached from three top-level areas: Registries, Files, Settings.

## Capabilities and Constraints

- Frontend: Svelte 5 + Tailwind v4 + Vite, `svelte-spa-router` hash routing, lucide icons, CodeMirror for viewers. Built into `_ui/dist` and embedded in the Go binary.
- Terminology: namespace, repository (local / remote), raw mount (prefix), share, serve server, listener.
- Open decisions (asked, not answered): whether the logo, light+dark theme choice, the no-downloaded-fonts rule, and rakunlabs family consistency are binding. Until decided, current behavior (system fonts only, user-selectable theme) is kept.

## Evidence on Hand

- Logo / favicon: `_ui/public/favicon-*.png`.
- No customer logos, testimonials or metrics exist; none may be invented.

## Product Principles

1. Finding things beats configuring things: browse surfaces get the space and the speed.
2. State must be legible at a glance: running / stopped / error, writable / read-only, locked / unlocked.
3. Dangerous actions are explicit and reversible where possible; nothing destructive hides behind a bare icon.
4. One binary, one coherent UI: every area speaks the same component vocabulary.
