<img align="left" height="64" src="_ui/public/favicon-192x192.png" />

# kutu

[![License](https://img.shields.io/github/license/rakunlabs/kutu?color=blue&style=flat-square)](https://raw.githubusercontent.com/rakunlabs/kutu/main/LICENSE)
[![Coverage](https://img.shields.io/sonar/coverage/rakunlabs_kutu?logo=sonarcloud&server=https%3A%2F%2Fsonarcloud.io&style=flat-square)](https://sonarcloud.io/summary/overall?id=rakunlabs_kutu)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/rakunlabs/kutu/test.yml?branch=main&logo=github&style=flat-square&label=ci)](https://github.com/rakunlabs/kutu/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/rakunlabs/kutu?style=flat-square)](https://goreportcard.com/report/github.com/rakunlabs/kutu)

A self-hosted artifact registry, file browser and gateway with a single embedded web UI.
Runs as one binary backed by PostgreSQL, with no authentication layer in front of it.

## Features

- **Artifact registry** — npm, Go, Docker, Helm, Maven, PyPI and Cargo, proxying or hosting locally.
- **Raw mounts** — browse and serve files from local disk, S3, FTP, SFTP, WebDAV or Vercel Blob.
- **File serving** — expose those mounts over **FTP**, **SFTP**, **TFTP** and **WebDAV** with shared users and shares.
- **Proxy** — build reverse-proxy graphs (listeners, middlewares, handlers) from the UI.
- **Hooks** — emit file events to external targets.
- **At-rest encryption** — secret values are sealed with a key you unlock from the UI.

## Quick start

Requires Go 1.26, Node + pnpm (for the UI), and a PostgreSQL database.

```bash
# 1. Start PostgreSQL (kutu:kutu@localhost:5432/kutu)
make env

# 2. Build the UI and the binary
make build

# 3. Run
KUTU_STORAGE_DSN="postgres://kutu:kutu@localhost:5432/kutu?sslmode=disable" ./bin/kutu
```

Then open <http://localhost:8080>.

### Development

```bash
make run      # run the server (CONFIG_FILE=env/config/kutu.yaml)
make run-ui   # Vite dev server, proxies the API to :8080
make test     # go test ./...
```

## Configuration

Config is loaded as: defaults → config file → environment. Point at a file with
`CONFIG_FILE=env/config/kutu.yaml`, or set any value via a `KUTU_` env var.

| Key | Env | Default | Description |
| --- | --- | --- | --- |
| `server.host` | `KUTU_SERVER_HOST` | `""` | Bind address (empty = all interfaces). |
| `server.port` | `KUTU_SERVER_PORT` | `8080` | HTTP port. |
| `storage.dsn` | `KUTU_STORAGE_DSN` | — | PostgreSQL connection string (**required**). |
| `encryption.password` | `KUTU_ENCRYPTION_PASSWORD` | `""` | At-rest key; unlocks or initializes encryption on boot. |
| `log_level` | `KUTU_LOG_LEVEL` | `info` | Log verbosity. |
| `telemetry` | — | `{}` | OpenTelemetry (OTLP) export; empty = disabled. |

## File serving

Configure the built-in FTP / SFTP / TFTP / WebDAV servers from **Settings → File serving**:

1. Add one or more **raw mounts** (Settings → Raw mounts).
2. Create a **share** that points at a mount path, e.g. `data/releases`.
3. Add a **user** (FTP / SFTP / WebDAV need credentials; TFTP is anonymous and read-only).
4. Enable a protocol and **Save** — servers reconcile live, no restart needed.

## License

[MIT](LICENSE) © rakunlabs
