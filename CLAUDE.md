# tg-torrent-bot

Single-binary Go Telegram bot for torrent search and subscriptions, deployed
on an Umbrel Raspberry Pi next to Prowlarr and Transmission. See `README.md`
for architecture and user-facing docs.

## Build & test

```sh
go test ./...        # full suite; must pass before every commit
go vet ./...
go build ./cmd/bot   # host binary
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/bot   # Pi binary (static)
```

## Architecture

One process, four long-lived loops wired in `cmd/bot/main.go` via errgroup:

- **Telegram long-poll loop** (`internal/tgbot`) — search, commands, inline
  keyboards, allowlist middleware (single ALLOWED_CHAT_ID; everything else is
  dropped silently).
- **Subscription engine** (`internal/subs/engine.go`) — every SUB_INTERVAL:
  search → filter → skip seen → add to Transmission → notify. Capped at 10
  grabs per subscription per tick.
- **Completion watcher** (`internal/subs/watcher.go`) — polls Transmission
  every 30 s, sends exactly one "finished" notification per download.
- **Settings server** (`internal/web`) — settings form on :8542; saving writes
  `/data/config.json` and restarts the process to apply it. With no usable
  config, main starts in setup mode: this server alone, no store, no loops.

Supporting packages: `internal/config` (settings file, env fallback),
`internal/store` (SQLite), `internal/filter` (release filters), `internal/prowlarr` and
`internal/transmission` (HTTP clients), `internal/grab` (shared
"prefer .torrent, fall back to magnet" add policy).

## Conventions

- **TDD**: write tests first; every code change ships with new/updated tests
  covering both success and error paths. All tests must pass before moving on.
- **Interfaces at the consumer**: every external service (Telegram, Prowlarr,
  Transmission, the store) is accessed through a narrow interface declared in
  the consuming package and faked in its tests. No mocking frameworks.
- **Pure Go only, no CGO**: SQLite is `modernc.org/sqlite`; cross-compiling
  for ARM64 must stay `CGO_ENABLED=0`-clean.
- **Migrations are append-only**: `internal/store/migrate.go` tracks schema
  versions in `PRAGMA user_version`. Never edit a shipped migration — append
  a new one.
- **Per-connection SQLite pragmas ride in the DSN** (`_pragma=` query
  parameters in `store.Open`), never via `Exec`, so reconnects keep them.
- **Failures in background loops are logged, never fatal**; each loop retries
  on its own schedule.

## Deployment

Releases are cut by pushing a `v*` tag: CI builds a multi-arch image, pushes it
to `ghcr.io/freemanoid/tg-torrent-bot`, and auto-bumps the version and pinned
digest in the [app store repo](https://github.com/freemanoid/umbrel-app-store),
which is what Umbrel installs. Never edit those two files by hand.

This repo is public — keep code, tests, and docs free of personal details
(tracker names, chat IDs, host paths); the fixtures use generic placeholders.
