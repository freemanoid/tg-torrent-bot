# tg-torrent-bot

A single-binary Go Telegram bot for one-hand torrent management from a phone,
built to run on an [Umbrel](https://umbrel.com/) Raspberry Pi 4B next to
Prowlarr and Transmission from the Umbrel app store. The bot is the only
custom piece; everything else is off-the-shelf.

It does two things:

1. **Interactive search** — any plain-text message is a search query. The bot
   asks Prowlarr (TrackerA+TrackerB), shows results as an inline keyboard
   sorted by seeders, and one tap sends the torrent to Transmission. When the
   download finishes, the bot sends a one-time "✅ finished" notification.
2. **Subscriptions** — `/sub <query> | <filters>` (e.g.
   `/sub space show 2026 | rus, 1080p, x265, -720p`). A ticker re-runs the
   search every ~20 minutes, applies include/exclude/size filters, dedupes
   against a seen-table, auto-adds new matches to Transmission, and notifies
   via Telegram. Hands-off auto-downloading of recurring content.

Only one chat ID is allowed to talk to the bot; every other update is dropped
silently.

## Architecture

```
                        Telegram servers
                              ▲
                              │ long polling (no inbound ports)
                              ▼
┌──────────────────────── Umbrel Pi ─────────────────────────────┐
│                                                                │
│  ┌───────────── tg-torrent-bot (this repo) ───────────────┐    │
│  │                                                        │    │
│  │  Telegram loop        Subscription ticker   Completion │    │
│  │  search, commands,    every SUB_INTERVAL:   watcher    │    │
│  │  inline keyboards     search → filter →     every 30s: │    │
│  │                       dedupe → auto-add     notify on  │    │
│  │                                             100%       │    │
│  │                                                        │    │
│  │            SQLite (subscriptions, seen, downloads)     │    │
│  └──────────┬──────────────────────────────┬──────────────┘    │
│             │ /api/v1/search               │ RPC: add torrent, │
│             │ + .torrent download          │ poll progress     │
│             ▼                              ▼                   │
│  ┌────────────────────┐         ┌────────────────────┐         │
│  │      Prowlarr      │         │    Transmission    │         │
│  │   your indexers    │         │  download client   │         │
│  └────────────────────┘         └────────────────────┘         │
└────────────────────────────────────────────────────────────────┘
```

One process, three long-lived loops (wired in `cmd/bot/main.go` via errgroup):

- **Telegram long-poll loop** — handles messages, commands, and callbacks.
  Long polling means no inbound ports and no port forwarding on the home
  network.
- **Subscription ticker** — runs each active subscription through Prowlarr,
  filters, skips already-seen releases, adds matches to Transmission,
  notifies.
- **Completion watcher** — polls Transmission every 30 s for downloads the bot
  added; sends exactly one "finished" message per download.

All state lives in one SQLite file (pure-Go driver, no CGO), so restarts are
always safe: Telegram resumes via update offset, and the seen-table prevents
re-grabbing. Design details worth knowing:

- The bot **downloads the .torrent bytes itself** through Prowlarr's proxied
  `downloadUrl` and hands base64 metainfo to Transmission, instead of passing
  a URL — this sidesteps container-networking issues where Transmission can't
  resolve the bot's or Prowlarr's address. Magnet links are passed through
  directly when that's all a release has.
- A release is recorded as seen only **after** Transmission confirms the add;
  a failed add is retried on the next tick.
- Inline-keyboard search results are cached in memory with short IDs (Telegram
  callback data is limited to 64 bytes) and expire after 1 h. A restart drops
  pending keyboards — just search again.

### Package layout

```
cmd/bot/                wiring, self-check, graceful shutdown
internal/config/        env-var config loading + validation
internal/store/         SQLite: subscriptions, seen, downloads
internal/filter/        include/exclude/size matching
internal/prowlarr/      Prowlarr /api/v1/search client
internal/transmission/  thin wrapper over hekmon/transmissionrpc
internal/subs/          subscription engine + completion watcher
internal/tgbot/         Telegram handlers, formatting, keyboards
```

## Configuration

All configuration comes from environment variables (see `.env.example`):

| Var | Required | Meaning |
|---|---|---|
| `TELEGRAM_TOKEN` | yes | Bot token from [@BotFather](https://t.me/BotFather) |
| `ALLOWED_CHAT_ID` | yes | Single-user allowlist; get yours from [@userinfobot](https://t.me/userinfobot) |
| `PROWLARR_URL` | yes | e.g. `http://umbrel.local:9696` |
| `PROWLARR_API_KEY` | yes | Prowlarr → Settings → General → API Key |
| `TRANSMISSION_URL` | yes | e.g. `http://umbrel.local:9091` |
| `TRANSMISSION_USER` / `TRANSMISSION_PASS` | no | Only if Transmission RPC auth is enabled |
| `DB_PATH` | no (default `/data/bot.db`) | SQLite location — point at a mounted volume in Docker |
| `SUB_INTERVAL` | no (default `20m`) | Subscription tick interval, Go duration syntax |

## Commands

| Command | What it does |
|---|---|
| *any plain text* | Search Prowlarr, show tappable results sorted by seeders |
| `/sub <query> \| <filters>` | Create a subscription (filters optional) |
| `/subs` | List subscriptions with filters, paused state, grab count |
| `/unsub <id>` | Remove a subscription |
| `/pause <id>` | Pause or resume a subscription |
| `/test <id>` | Dry-run: show what WOULD match right now, download nothing |
| `/status` | Active downloads with progress bars |
| `/help` | Usage summary |
| `/start` | Same as `/help` |

Searches return the top 50 releases (10 per keyboard page), sorted by
seeders. Subscriptions grab at most 10 new matches per tick; the rest are
picked up on the following ticks.

## Filter syntax (`/sub`, after the `|`)

Comma-separated tokens, matched case-insensitively (Cyrillic included)
against the release title:

| Token | Meaning | Example |
|---|---|---|
| plain token | required substring | `rus`, `1080p`, `x265` |
| `-token` | excluded substring | `-camrip`, `-720p` |
| `/regex/` | required regex | `/x26[45]/` |
| `-/regex/` | excluded regex | `-/cam\|ts/` |
| `>N gb/mb` | minimum size | `>1gb` |
| `<N gb/mb` | maximum size | `<30gb` |

A release matches when **all** includes match, **no** excludes match, and its
size is within bounds. An empty filter matches everything.

```
/sub space show 2026 | rus, 1080p, x265, -720p, <30gb
```

## Building

Requires Go 1.26+. Pure Go all the way down (`modernc.org/sqlite`, no CGO),
so cross-compilation needs nothing special:

```sh
go test ./...                                        # full test suite
go vet ./...
go build ./cmd/bot                                   # host binary
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/bot   # Pi binary (~10 MB, static)
```

Docker image (multi-stage, static binary, ~10 MB final image):

```sh
docker build --platform linux/arm64 -t tg-torrent-bot:latest .
```

Run with compose (reads `.env`, persists SQLite in `./data`):

```sh
cp .env.example .env   # then fill in the values
docker compose up -d
```

`docker-compose.yml` is written to be paste-able as a Portainer stack — it has
no `build:` key, so the image must already exist on the host.

## Deployment

Deployment to the Umbrel Pi is covered by two companion plans:

- [`docs/plans/20260731-umbrel-manual-setup.md`](docs/plans/20260731-umbrel-manual-setup.md)
  — one-time manual steps in the Umbrel UI: installing Prowlarr and
  Transmission from the app store, adding indexers, collecting API keys.
- [`docs/plans/20260731-umbrel-claude-code-setup.md`](docs/plans/20260731-umbrel-claude-code-setup.md)
  — deploying the bot to the Pi via Claude Code over SSH, including the real
  end-to-end smoke test against live trackers.

### Reaching Prowlarr and Transmission on Umbrel

Umbrel fronts each app's published port with an *app proxy* that requires a
dashboard login, and `umbrel.local` is mDNS, which does not resolve inside a
container. So `http://umbrel.local:9091` does **not** work for Transmission RPC
— the proxy answers `302` and redirects to the login page.

Instead the bot joins Umbrel's shared bridge network and talks to the service
containers directly:

```yaml
# docker-compose.yml
networks:
  - umbrel_main_network   # declared external: true
```

```sh
PROWLARR_URL=http://prowlarr_server_1:9696
TRANSMISSION_URL=http://transmission_server_1:9091
```

Transmission's RPC has no auth on this setup, so `TRANSMISSION_USER` /
`TRANSMISSION_PASS` stay empty. Downloads land in `/home/umbrel/umbrel/home/Downloads`
on the host (the external SSD).

### Releasing

Images are built by GitHub Actions on version tags and published to
`ghcr.io/freemanoid/tg-torrent-bot` (multi-arch: amd64 + arm64):

```sh
git tag v1.2.3
git push origin v1.2.3
```

The workflow run's summary shows the pushed image digest. To roll it out:

- **Umbrel (community app)**: bump `version` and the pinned image digest in the
  [app store repo](https://github.com/freemanoid/umbrel-app-store); the app then
  shows an update button in the Umbrel dashboard.
- **Plain compose**: `docker compose pull && docker compose up -d`.

State always survives updates: the SQLite database lives on a mounted volume
(`app-data/.../data/` on Umbrel), and `.env` is external to the image.

A `workflow_dispatch` run of the release workflow builds both platforms without
pushing — a dry run for Dockerfile changes.

Tip: tune your first real subscription with `/test <id>` against live results
before trusting auto-grab.
