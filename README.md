# tg-torrent-bot

A single-binary Go Telegram bot for one-hand torrent management from a phone,
built to run on an [Umbrel](https://umbrel.com/) Raspberry Pi 4B next to
Prowlarr and Transmission from the Umbrel app store. The bot is the only
custom piece; everything else is off-the-shelf.

It does two things:

1. **Interactive search** — any plain-text message is a search query. The bot
   asks Prowlarr (TrackerA+TrackerB) and lists the results sorted by seeders,
   each with its full title, swarm health, and whatever the release title says
   about the media inside (resolution, source, codecs, audio tracks,
   subtitles, container). Prefix a word with `-` to exclude it
   (`формула 1 2026 2160p -AV1`). One tap sends the torrent to Transmission.
   When the download finishes, the bot sends a one-time "✅ finished"
   notification.
2. **Subscriptions** — tap 🔔 under any search results to watch that exact
   query, or write one out with `/sub <query> | <filters>` (e.g.
   `/sub space show 2026 | rus, 1080p, x265, -720p`). A ticker re-runs the
   search every ~20 minutes, applies include/exclude/size filters, dedupes
   against a seen-table, auto-adds new matches to Transmission, and notifies
   via Telegram. Hands-off auto-downloading of recurring content.

   A subscription only grabs releases **published after it was created**, so
   subscribing to a series mid-run does not pull in every earlier episode; add
   the `backlog` filter when you do want what is already on the tracker. Each
   grab is announced with a 🗑 button that removes the torrent and deletes its
   files, for the times the bot guessed wrong.
3. **Housekeeping** — `/status` lists everything the bot has, running and
   recently finished, each entry numbered with a matching 🗑 button. Tap one and
   confirm to remove that torrent from Transmission and delete what it wrote to
   disk, whether it came from a search or from a subscription.

After an app update the bot posts what it is now running — the new version and
that version's [CHANGELOG.md](CHANGELOG.md) entry — once per release, so a
silent container restart no longer hides what arrived.

Only the chat IDs on the allowlist can talk to the bot; every other update is
dropped silently. List several to share one bot with a household — each search
is answered in the chat that asked, while subscription and completion notices
go to everyone. Subscriptions and downloads belong to the bot, not to a chat:
anyone allowed can list, pause or remove any of them.

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
│  │            Settings page on :8542 (Umbrel app tile)    │    │
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

One process, four long-lived loops (wired in `cmd/bot/main.go` via errgroup):

- **Telegram long-poll loop** — handles messages, commands, and callbacks.
  Long polling means no inbound ports and no port forwarding on the home
  network.
- **Subscription ticker** — runs each active subscription through Prowlarr,
  filters, skips already-seen releases, adds matches to Transmission,
  notifies.
- **Completion watcher** — polls Transmission every 30 s for downloads the bot
  added; sends exactly one "finished" message per download.
- **Settings server** — serves the configuration form on port 8542 (see
  [Configuration](#configuration)). Without a usable configuration it is the
  *only* thing that runs; a failure to start never takes the other loops down.

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
cmd/bot/                wiring, setup mode, self-check, graceful shutdown
internal/config/        settings file + env-var loading, validation
internal/store/         SQLite: subscriptions, seen, downloads
internal/filter/        include/exclude/size matching
internal/mediainfo/     release-title parsing: codecs, audio, subs, container
internal/prowlarr/      Prowlarr /api/v1/search client
internal/transmission/  thin wrapper over hekmon/transmissionrpc
internal/subs/          subscription engine + completion watcher
internal/release/       post-update announcement: version + changelog entry
internal/tgbot/         Telegram handlers, formatting, keyboards
internal/web/           settings page: form, save, restart-to-apply
```

## Configuration

### Settings page (recommended)

The bot serves its own settings page on port 8542 — on Umbrel, that is what
the app tile opens, so configuration is a browser form, not an SSH session.
The page has no login of its own: Umbrel's app proxy already gates it, exactly
like every other app.

1. Open the app from the Umbrel dashboard.
2. Fill in the Telegram token, allowed chat IDs, Prowlarr and Transmission
   URLs, the Prowlarr API key, and (optionally) Transmission credentials and
   the subscription interval.
3. Save. The bot writes the settings and restarts to apply them; the page
   reloads itself a few seconds later.

Details worth knowing:

- Settings are stored as JSON in `/data/config.json` (mode `0600`, on the same
  persistent volume as the database), so they survive updates and restarts.
  `CONFIG_PATH` overrides the location.
- **A settings file wins over environment variables.** Once the page has saved
  one, it is the only source for the values below; the environment is used
  only when the file is absent. `DB_PATH` and `CONFIG_PATH` are the exception —
  they are infrastructure and stay environment-only.
- **Every value is shown in full**, tokens and passwords included, so they can
  be read off the page and copied. There is nothing to hide from: the page is
  already behind Umbrel's app proxy. A submission is therefore the whole
  configuration — clearing a field clears the setting rather than keeping the
  stored value.
- **Saving restarts the bot.** There is no hot reload: the process exits
  non-zero and the container restart policy starts it again with the new
  configuration. Downtime is a couple of seconds; SQLite and the Telegram
  update offset both survive it.
- **Nothing configured yet?** The bot starts in *setup mode*: it logs that
  setup is needed and runs only the settings page — no Telegram loop, no
  database — until a complete configuration is saved.

### Environment variables (headless / compose)

Plain-compose and other headless installs can skip the page entirely and
configure everything up front (see `.env.example`). These are also the values
a pre-1.1 install already has; they keep working untouched until the settings
page saves a file.

| Var | Required | Meaning |
|---|---|---|
| `TELEGRAM_TOKEN` | yes | Bot token from [@BotFather](https://t.me/BotFather) |
| `ALLOWED_CHAT_ID` | yes | Chat allowlist, comma-separated for several chats; get yours from [@userinfobot](https://t.me/userinfobot) |
| `PROWLARR_URL` | yes | e.g. `http://umbrel.local:9696` |
| `PROWLARR_API_KEY` | yes | Prowlarr → Settings → General → API Key |
| `TRANSMISSION_URL` | yes | e.g. `http://umbrel.local:9091` |
| `TRANSMISSION_USER` / `TRANSMISSION_PASS` | no | Only if Transmission RPC auth is enabled |
| `DB_PATH` | no (default `/data/bot.db`) | SQLite location — point at a mounted volume in Docker |
| `CONFIG_PATH` | no (default `/data/config.json`) | Settings file written by the page |
| `SUB_INTERVAL` | no (default `20m`) | Subscription tick interval, Go duration syntax |

Required values missing from both sources are not fatal — the bot starts in
setup mode and waits for the form.

## Commands

| Command | What it does |
|---|---|
| *any plain text* | Search Prowlarr, show tappable results sorted by seeders; `-word` excludes |
| 🔔 *(button)* | Subscribe to the search you just ran, its exclusions kept, new releases only |
| 🗑 *(button)* | Reject a subscription grab: remove it from Transmission and delete its files |
| `/sub <query> \| <filters>` | Create a subscription (filters optional) |
| `/subs` | List subscriptions with filters, paused state, grab count, cutoff date |
| `/unsub <id>` | Remove a subscription |
| `/pause <id>` | Pause or resume a subscription |
| `/test <id>` | Dry-run: show what WOULD match right now, download nothing |
| `/status` | Active downloads with progress bars, plus recently completed ones; tap 🗑*number* to delete one |
| `/help` | Usage summary |
| `/start` | Same as `/help` |

Searches return the top 50 releases (5 per keyboard page), sorted by
seeders. Subscriptions grab at most 10 new matches per tick; the rest are
picked up on the following ticks.

### Excluding from a search

A search word starting with `-` excludes instead of searching: `-AV1` drops
every result whose title contains AV1, and `-/av1|vp9/` does the same with a
regex — the `/sub` exclusion tokens, applied to one search. Only a leading `-`
counts, so `WEB-DL` stays an ordinary word, and the exclusions never reach
Prowlarr: each indexer reads query syntax its own way, so the bot filters the
results itself and the meaning stays the same whichever tracker answered.

```
формула 1 2026 2160p 10 rus -AV1
```

Size bounds (`>1gb`, `<30gb`) are subscription-only and stay ordinary search
words here.

Each result is listed like this:

```
1. Космос / Space Show (2026) [BDRemux 1080p, AVC, HDR10] MKV [Dub, MVO, AVO,
   Original Eng + Sub Rus, Eng] DTS-HD MA 5.1, ~24000 kbps
   4.5GB · ↑146 ↓12 · TrackerA
   1080p Remux · H.264 · HDR10 · MKV · ~24000 kbps
   Audio: DTS-HD MA 5.1 · Dub, MVO, AVO, Original · Eng
   Subs: Rus, Eng
   Apple TV 4K: ✅ plays (Infuse/Plex)
```

A result the bot has already handed to Transmission is marked in front of its
title, on both the text and the button: **⬇️** while it is still downloading,
**✅** once it has finished. Matching is by info hash, falling back to the exact
release title for indexers that publish no hash — so only what the bot itself
grabbed is marked, not torrents added directly in Transmission.

Everything below the size line is read out of the release title itself —
Prowlarr publishes no structured media metadata — so a line is shown only when
the title actually stated it. A release that names no codec simply has no codec
line. The Apple TV 4K verdict assumes a third-party player (Infuse, Plex, VLC);
under that profile only AV1 and 3D are flagged, since the built-in tvOS player
would reject nearly every tracker release for its container alone.

### Looking before you download

Under the result buttons sits a row of **ℹ️1 … ℹ️5**. Tapping one describes that
release in full, in its own message, without downloading anything:

```
ℹ️ 1. Космос / Space Show (2026) [BDRemux 1080p, AVC, HDR10] MKV …
   4.5GB · ↑146 ↓12 · TrackerA
   1080p Remux · H.264 · HDR10 · MKV · ~24000 kbps
   Audio: DTS-HD MA 5.1 · Dub, MVO, AVO, Original · Eng
   Subs: Rus, Eng
   Apple TV 4K: ✅ plays (Infuse/Plex)
   Published 2026-07-30 · 812 grab(s)
   https://tracker-a.example.com/forum/viewtopic.php?t=111

Season 2026, dual audio, forced subtitles included.

📁 14 file(s) · 4.5GB
   Сезон 1/Этап 01.mkv — 340MB
   …
   … and 4 more file(s)

[ ⬇️ Download ]
```

The title is shown untruncated (the results list shares its space between five
releases), the description and tracker link are whatever the indexer published,
and the file list is read out of the .torrent itself. A magnet-only release has
no .torrent to read, so it says the file list is unavailable and still offers
the download — the magnet works regardless.

Below the ℹ️ row sits **🔗1 … 🔗5**: one button per result whose indexer published
a page for it, opening that page in the browser for everything the API never
carries — screenshots, the uploader's notes, the comments. Only results with a
usable link get a button, so the row is often short and is left out entirely
when no result on the page has one.

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
| `backlog` | also grab what is already on the tracker | `backlog` |

A release matches when **all** includes match, **no** excludes match, and its
size is within bounds. An empty filter matches everything.

`backlog` is not a title pattern — it is a setting on the subscription. By
default a subscription ignores anything published before it was created, which
is what makes "watch for new episodes" work without dragging in the whole
season; `backlog` turns that off. Releases whose indexer publishes no date at
all are skipped on the subscription's very first tick and taken from then on,
so a dateless tracker neither floods the chat nor goes silent forever.

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

A plain build reports its version as `dev` and announces nothing. Release
builds get the tag baked in by CI (`--build-arg VERSION=v1.2.3`, injected with
`-ldflags -X .../internal/release.Version`); pass the same build arg to
reproduce a release image locally.

Docker image (multi-stage, static binary, ~20 MB final image). Released images
are built by CI for `linux/amd64` and `linux/arm64`; to build one locally:

```sh
docker build --platform linux/arm64 -t tg-torrent-bot:latest .
```

## Deployment

### Umbrel (recommended)

Install it as an app from the community app store — this wires the settings
page behind Umbrel's authenticating proxy and puts the bot on the same network
as Prowlarr and Transmission:

1. Install **Prowlarr** and **Transmission** from Umbrel's own app store, add
   your indexers to Prowlarr, and copy its API key.
2. Add `https://github.com/freemanoid/umbrel-app-store` under App Store →
   ⋯ → Community App Stores.
3. Install **TG Torrent Bot**, open it, and fill in the settings form.

### Plain compose

```sh
docker compose up -d          # then open http://localhost:8542
```

The published image is used as-is, SQLite and the settings file persist in
`./data`, and the settings page is bound to `127.0.0.1` — it has no
authentication of its own, so keep it off untrusted networks.

### Reaching Prowlarr and Transmission on Umbrel

Umbrel fronts each app's published port with an *app proxy* that requires a
dashboard login, and `umbrel.local` is mDNS, which does not resolve inside a
container. So `http://umbrel.local:9091` does **not** work for Transmission RPC
— the proxy answers `302` and redirects to the login page. Use container names
on Umbrel's shared bridge network instead (the app store's compose joins it for
you):

```sh
PROWLARR_URL=http://prowlarr_server_1:9696
TRANSMISSION_URL=http://transmission_server_1:9091
```

If Transmission's RPC has no auth, leave `TRANSMISSION_USER` /
`TRANSMISSION_PASS` empty. Downloads land wherever the Transmission app is
configured to put them — on Umbrel, `~/umbrel/home/Downloads`.

### Releasing

Images are built by GitHub Actions on version tags and published to
`ghcr.io/freemanoid/tg-torrent-bot` (multi-arch: amd64 + arm64):

```sh
# add the release's entry to CHANGELOG.md first — the bot posts it in the chat
git tag v1.2.3
git push origin v1.2.3
```

The workflow then automatically bumps `version` and the pinned image digest in
the [app store repo](https://github.com/freemanoid/umbrel-app-store) (via a
write deploy key), so the app shows an update button in the Umbrel dashboard
with no manual steps. Plain-compose deployments update with
`docker compose pull && docker compose up -d`.

State always survives updates: the SQLite database and the settings file
(`config.json`) both live on a mounted volume (`app-data/.../data/` on Umbrel),
and `.env` is external to the image.

A `workflow_dispatch` run of the release workflow builds both platforms without
pushing — a dry run for Dockerfile changes.

Tip: tune your first real subscription with `/test <id>` against live results
before trusting auto-grab.
