# tg-torrent-bot — guide for AI agents

Single-binary Go Telegram bot: search torrents through Prowlarr, download via
Transmission, plus filter-based subscriptions that auto-grab new releases.
Ships as an Umbrel community app; runs anywhere Docker does. ~9.5k lines
including tests. `README.md` is the user-facing doc; this file is everything an
agent needs to change the code safely.

**Target host: a Raspberry Pi 4B (ARM64, 4–8 GB RAM) running umbrelOS**, next to
Prowlarr and Transmission from Umbrel's app store, on a home LAN behind NAT.
That shapes most of the design decisions below — see
[Target environment](#target-environment).

## Build, test, run

```sh
go test ./...        # full suite; must pass before every commit
go vet ./...
go build ./cmd/bot
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/bot   # Pi target (ARM64), static
docker build -t tg-torrent-bot:latest .                    # multi-stage, ~20 MB
```

No linter beyond `go vet` is configured; keep `gofmt` clean. Tests are
hermetic — no network, no Docker, no real Telegram; the whole suite runs in
seconds.

## Architecture

One process, four loops wired in `cmd/bot/main.go` with an errgroup:

| Loop | Package | What it does |
|---|---|---|
| Telegram long-poll | `internal/tgbot` | Search, commands, inline keyboards. Long polling ⇒ no inbound ports. |
| Subscription engine | `internal/subs/engine.go` | Every `SubInterval`: search → filter → skip seen → add → notify. |
| Completion watcher | `internal/subs/watcher.go` | Every 30 s: poll Transmission, one "finished" message per download. |
| Settings server | `internal/web` | Config form on `:8542`; save writes config and restarts the process. |

Supporting packages: `internal/config` (settings file + env fallback),
`internal/store` (SQLite), `internal/filter` (release matching),
`internal/mediainfo` (pure title parser: resolution, source, codecs, audio,
translations, subs, container, bitrate — Prowlarr publishes none of this as
structured data, so it is read out of the release title),
`internal/prowlarr` + `internal/transmission` (HTTP clients), `internal/grab`
(shared "prefer .torrent, fall back to magnet" add policy used by both the
Telegram handler and the engine — change it in one place, not two).

**Setup mode**: if no complete config exists, `main` starts *only* the settings
server — no store, no clients, no loops. `newApp(nil, opts)` is that path.

## Target environment

A Pi 4B is slow, memory-tight, and writes to flash. Consequences to respect
when changing anything:

- **ARM64, cross-compiled.** Every dependency must build with `CGO_ENABLED=0`;
  a cgo dependency would break the static ARM64 build and the tiny image.
- **Builds don't happen on the Pi anymore.** A native Docker build there took
  ~3.5 min, so CI builds multi-arch images instead. Assume no Go toolchain on
  the host.
- **Be frugal at runtime.** The image is ~20 MB and the process idles at a few
  MB. The host shares its RAM and CPU with every other Umbrel app — a media
  server, DNS filtering, home automation — so don't add heavyweight
  dependencies, background caches, or anything that holds large result sets in
  memory.
- **Polling is deliberately lazy** (20 min subscriptions, 30 s watcher) — it
  keeps CPU and flash writes low. Don't tighten intervals without a reason, and
  don't add a busy loop.
- **Flash-friendly storage.** SQLite lives on the app-data volume (SSD or SD
  card) in WAL mode with a single connection. Avoid chatty write patterns.
- **A single search can take minutes.** Indexers behind Cloudflare go through
  FlareSolverr; a cold challenge measured ~193 s. Everything downstream must
  tolerate slow calls (hence the 240 s Prowlarr timeout) rather than assume
  fast LAN latency.
- **Home network, no inbound ports.** Telegram is long-polled and nothing
  listens publicly; the settings page is LAN/proxy-only. Keep it that way.

## Data model

`store.Subscription{ID, Query, Include, Exclude, MinSizeMB, MaxSizeMB, Paused,
Grabs, CreatedAt, LastCheckedAt}` — `Include`/`Exclude` are raw filter tokens,
rebuilt into a `filter.Filter` by `subFilter()`.

`store.Download{Hash, Title, Source, Status, AddedAt}` — `Source` is `"search"`
or `"sub:<id>"`; `Status` is active/done. The `seen` table (`subID`+`guid`) is
what prevents re-grabbing; it cascades on subscription delete.

`prowlarr.Release{GUID, Title, Size, Seeders, Indexer, DownloadURL, MagnetURL,
InfoHash}` — `DownloadURL`/`MagnetURL` may each be empty; `grab.AddRelease`
handles all four combinations.

## Configuration

Precedence: **`/data/config.json` wins over environment variables.** When the
file exists it is the sole source for user config; env is used only when the
file is absent (that is how pre-1.1 installs keep working). `DB_PATH` and
`CONFIG_PATH` are infrastructure and stay env-only.

Saving from the settings page writes the file and **exits the process** so the
container restart policy applies it. There is no hot reload — don't add one
without redesigning how the loops hold their clients.

## Conventions

- **TDD**: tests first, always. Every change ships with tests for success *and*
  error paths. The suite must be green before moving to the next task.
- **Interfaces at the consumer**: every external dependency (Telegram, Prowlarr,
  Transmission, the store) is a narrow interface declared in the *consuming*
  package and faked in its tests. No mocking frameworks. When a package needs a
  new store method, extend that package's own interface and its fake.
- **Pure Go, no CGO**: SQLite is `modernc.org/sqlite`. ARM64 cross-compilation
  must stay `CGO_ENABLED=0`-clean — never introduce a cgo dependency.
- **Migrations are append-only**: `internal/store/migrate.go` tracks
  `PRAGMA user_version`; each migration runs in its own transaction. Never edit
  a shipped entry, only append.
- **SQLite pragmas ride in the DSN** (`_pragma=` in `store.Open`), never via
  `Exec`, so replaced connections keep them. Pool is capped at one connection.
- **Background failures are logged, never fatal**; each loop retries on its own
  schedule. Only setup-mode web failure is fatal (the page *is* the app then).
- **Test style**: table-driven where natural; `httptest` fake servers for HTTP
  clients; in-memory SQLite for the store; fakes with injected error fields for
  failure paths. Synchronise with channels, never `time.Sleep`.

## Testing locally

Work down this ladder and stop at the lowest rung that proves the change. Only
rung 3 needs credentials. (Claude Code picks the same workflow up automatically
as the `local-testing` skill in `.claude/skills/`; keep the two in sync when
either changes.)

**1. Unit tests — no credentials, use this by default.**

```sh
go test ./...            # whole suite, seconds, fully hermetic
go test ./internal/subs -run TestTick -v
go test -race ./internal/tgbot
go test -cover ./...
```

Every external service is faked, so almost any behaviour can be pinned here.
If something *cannot* be tested this way, that usually means a dependency needs
to become a consumer-side interface — do that instead of reaching for a real
service.

**2. Run the real binary in setup mode — still no credentials.**

With no usable config the process starts the settings page alone, which is
enough to exercise the web UI, the form, validation, and the save-and-restart
path end to end:

```sh
mkdir -p /tmp/tgbot-dev
CONFIG_PATH=/tmp/tgbot-dev/config.json DB_PATH=/tmp/tgbot-dev/bot.db go run ./cmd/bot
# open http://localhost:8542   (or: curl -s localhost:8542/healthz)
```

Saving the form writes `config.json` and **exits the process** — that is the
restart-to-apply design, not a crash. Always point `CONFIG_PATH`/`DB_PATH` at a
scratch directory outside the repo; never at a real deployment's app-data.

**3. Run against real services — credentials required, so ask first.**

A fake token does not work: the bot calls `getMe` at startup and exits with
`Unauthorized`. (Tests bypass this with `WithSkipGetMe`; there is no runtime
flag.) So a full-mode local run needs a real Telegram bot token, and Prowlarr
and Transmission instances.

Rules when an agent needs these:

- **Ask the user for every credential — never invent, guess, or reuse one found
  in a deployment.** Prompt for exactly what is needed and why.
- **Require a separate throwaway bot** from @BotFather, never the production
  token. Two long-pollers sharing a token fight over updates (Telegram answers
  `409 Conflict`) and the local one would silently steal messages from the
  deployed bot.
- **Accept secrets as environment variables for that run only.** Never write
  them into repo files, tests, fixtures, commit messages, or this file.
  `.env`, `data/`, and `*.db` are gitignored — keep it that way, and prefer a
  scratch dir outside the repo entirely.
- Local backends are easy to stand up if the user wants them:
  `docker run -d -p 9091:9091 lscr.io/linuxserver/transmission` and
  `docker run -d -p 9696:9696 lscr.io/linuxserver/prowlarr`.
- To use a real Umbrel host's services instead, tunnel them — the app proxy
  rejects Transmission RPC on the published port:
  `ssh -L 9091:transmission_server_1:9091 -L 9696:prowlarr_server_1:9696 <host>`.

## Recipes

**Add a bot command** — `internal/tgbot/commands.go`: add a `case` in
`handleCommand`, write the `cmdX` method, register it in `commandMenu()` and in
`helpText`. If it needs new store access, extend `SubscriptionStore` (note: it
deliberately excludes `seen` methods so `/test` structurally cannot mark
releases seen) and the test fake. Tests go in `commands_test.go`.

**Add a config field** — five files, in this order: `internal/config/config.go`
(struct, env parse in `Load`, `Validate` if required), `internal/config/file.go`
(json tag, `LoadFrom` + `Save` mapping), `internal/web/server.go` (`formView`
field, template input, POST parsing; if it is a secret add a `HasX` bool and
never render the value), `.env.example`, README config table. Tests in all
three packages.

**Add a filter token type** — `internal/filter/filter.go`: `Parse` and `Match`,
plus `String` so the `Parse ⇄ String` round-trip test still holds. Filters are
persisted as raw token strings, so old subscriptions must keep parsing.

**Change search results / keyboards** — `internal/tgbot/search.go` (flow),
`format.go` (`resultsMessage` → `releaseBlock` for the text, `buttonLabel` for
the keyboard, callback packing). What a release *says about itself* is parsed
in `internal/mediainfo`; add a new codec, language or translation token to its
`phrases` table rather than to the formatter. Callback data is hard-capped at
64 bytes by Telegram; the search cache maps a short ID to results with a 1 h
TTL.

## Nuances that bite

- **Seen is marked only after Transmission confirms the add.** Keep that order —
  a failed add must be retried next tick.
- **Grabs are capped at 10 per subscription per tick** (`maxGrabsPerTick`) so a
  filterless subscription trickles instead of dumping 50 torrents at once.
- **A duplicate torrent is success**, not an error — Transmission returns the
  existing hash. `AddDownload` reactivates a `done` row so re-downloads still
  notify.
- **Prowlarr timeout is 240 s** (`DefaultTimeout`) because a cold FlareSolverr
  Cloudflare challenge measured ~193 s in practice. Don't lower it.
- **The Prowlarr API key is sent only to the configured host** and stripped on
  cross-host redirects; `downloadUrl` can point anywhere.
- **Telegram messages are chunked at 4096 chars**; unbounded replies (`/subs`,
  `/status`) would otherwise silently fail to send.
- **The search results message cannot be chunked** — the inline keyboard is
  attached to one message, so it must fit in a single send or a search that
  cost minutes is lost. `resultsMessage` gives the five blocks on a page a
  shared rune budget instead of dropping any, so the numbering keeps matching
  the buttons. Raise `perPage` and that budget shrinks accordingly.
- **On Umbrel, reach services by container name** (`prowlarr_server_1:9696`,
  `transmission_server_1:9091`). `umbrel.local` is mDNS and does not resolve
  inside containers, and published ports sit behind an auth proxy that answers
  `302` to Transmission RPC.
- **The settings page has no authentication of its own** — Umbrel's app proxy
  provides it, and plain compose binds it to `127.0.0.1`. Never add anything to
  it that would be unsafe behind a thin proxy.
- **umbrelOS deletes non-app containers on boot** and reverts root-filesystem
  changes; only app-data and `/home` persist. That is why this ships as an
  Umbrel app rather than a standalone compose stack.

## Release & deployment

Push a `v*` tag. CI (`.github/workflows/release.yml`) builds a multi-arch image
(amd64 + arm64), pushes it to `ghcr.io/freemanoid/tg-torrent-bot`, then
auto-commits the new version and pinned digest to the
[app store repo](https://github.com/freemanoid/umbrel-app-store) via a deploy
key — Umbrel then offers the update. Never hand-edit those two store files.
`ci.yml` runs vet + tests + an arm64 build check on every push and PR.

**Tagging is part of shipping, not a separate request.** When a change that
affects the running bot lands on master, cut and push the tag as the last step
— without asking. Docs-, test- and CI-only changes are not released. Version
rules, preconditions, and the exact commands live in the `releasing` skill
(`.claude/skills/releasing/SKILL.md`); keep the two in sync when either
changes. A pushed tag is immutable: fix a bad release by releasing forward, not
by moving a tag.

## This repo is public

Keep code, tests, docs, and commit messages free of personal details — no real
tracker names, chat IDs, tokens, host paths, or use-case specifics. Fixtures use
generic placeholders (`Космос`, `Space Show`, `TrackerA`/`TrackerB`,
`tracker-a.example.com`); follow that style. A local `private-archive` branch
may hold pre-sanitization history — never push it, and prefer explicit
`git push origin <branch>` over `--all`/`--mirror`.

## Known gaps (fair game to improve)

- A corrupt `config.json` is fatal at startup instead of falling back to setup
  mode, so the settings page can't be used to fix it.
- Per-indexer failures are invisible: Prowlarr returns HTTP 200 with partial
  results, and only whole-request failures surface an indexer name.
- Message chunking (`splitMessage`) counts runes while Telegram counts UTF-16
  units, so a chunk ending in astral characters could still be rejected.
  `resultsMessage` is the exception — it budgets in UTF-16 units, because that
  message carries the keyboard and cannot be re-sent as two.
- No hot reload: every settings change costs a restart.
