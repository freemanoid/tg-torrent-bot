---
name: local-testing
description: Run and test tg-torrent-bot on a development machine — unit tests, a credential-free run of the real binary in setup mode, and the rules for full runs against real Telegram/Prowlarr/Transmission. Use this whenever the user wants to test, run, start, debug, or verify the bot locally, check whether a change works, look at the settings page, or reproduce a bug outside the Pi — and before asking for or handling any token, API key, or chat ID.
---

# Testing tg-torrent-bot locally

Work down this ladder and stop at the lowest rung that actually proves the
change. Rungs 1 and 2 need no credentials at all, and they cover most work —
reach for rung 3 only when the behaviour genuinely depends on a live service.

## Rung 1 — unit tests (default)

```sh
go test ./...                      # whole suite, seconds, fully hermetic
go test ./internal/subs -run TestTick -v
go test -race ./internal/tgbot
go vet ./...
```

Every external service is faked, so nearly any behaviour can be pinned here.
If something *cannot* be tested this way, that is usually a design signal: the
dependency should become a narrow interface declared in the consuming package
(the project's convention) rather than a reason to spin up a real service.

Tests must be green before any commit.

## Rung 2 — run the real binary in setup mode (no credentials)

With no usable configuration the process starts the settings page alone — no
store, no Telegram, no loops. That is enough to exercise the web UI, form
validation, and the save-and-restart path end to end:

```sh
mkdir -p /tmp/tgbot-dev
CONFIG_PATH=/tmp/tgbot-dev/config.json DB_PATH=/tmp/tgbot-dev/bot.db go run ./cmd/bot
```

Then open <http://localhost:8542>, or check it headlessly:

```sh
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8542/healthz   # 200
curl -s http://localhost:8542/ | grep -o 'Not configured'                # setup banner
```

Expect a startup log line naming the missing fields, then `settings server
listening`. Submitting the form writes `config.json` and **exits the process** —
that is the restart-to-apply design, not a crash; in production the container
restart policy brings it straight back.

Always point `CONFIG_PATH` and `DB_PATH` at a scratch directory outside the
repo. Never at a real deployment's app-data, and never at paths inside the
working tree, where a stray `git add` could commit them.

## Rung 3 — full run against real services (needs credentials — ask first)

A fake token will not get you here: the bot calls `getMe` on startup and exits
with `Unauthorized`. Tests bypass this with `WithSkipGetMe`; there is no
runtime flag. So a full-mode run needs a real Telegram bot token plus reachable
Prowlarr and Transmission instances.

**Ask the user for every credential — never invent one, and never reuse one
found in a deployment, a config file, or shell history.** Prompt for exactly
what is needed and say why, for example: "To run against real Telegram I need a
bot token — please create a throwaway bot with @BotFather rather than using the
production one, and tell me the chat ID to allow."

Then hold to these rules, which exist to protect the user's running deployment
and their secrets:

- **Insist on a separate throwaway bot.** Two long-pollers sharing one token
  fight over updates — Telegram answers `409 Conflict` — and the local instance
  silently steals messages from the deployed bot. This breaks the user's real
  bot while you test, in a way that is confusing to diagnose.
- **Accept secrets as environment variables for that run only.** Never write
  them into repo files, tests, fixtures, scratch scripts, commit messages, or
  documentation. `.env`, `data/`, and `*.db` are gitignored — keep it that way,
  and prefer a scratch directory outside the repo entirely.
- **Never echo a secret back** in output, logs, or a summary. Confirm by
  property ("token accepted, bot is @…"), not by value.

```sh
TELEGRAM_TOKEN=… ALLOWED_CHAT_ID=… \
PROWLARR_URL=http://localhost:9696 PROWLARR_API_KEY=… \
TRANSMISSION_URL=http://localhost:9091 \
CONFIG_PATH=/tmp/tgbot-dev/config.json DB_PATH=/tmp/tgbot-dev/bot.db \
go run ./cmd/bot
```

(Environment variables apply only when no settings file exists — a
`config.json` in `CONFIG_PATH` takes precedence, so delete it first if you want
the env values to be used.)

Backends, if the user wants real ones locally:

```sh
docker run -d -p 9091:9091 lscr.io/linuxserver/transmission
docker run -d -p 9696:9696 lscr.io/linuxserver/prowlarr
```

To use an existing Umbrel host's services instead, tunnel them — the app proxy
rejects Transmission RPC on the published port, so go straight to the
containers:

```sh
ssh -L 9091:transmission_server_1:9091 -L 9696:prowlarr_server_1:9696 <host>
```

## Cleaning up

Stop the process, then remove the scratch directory (`rm -rf /tmp/tgbot-dev`) so
a later run starts from setup mode again and no credentials linger on disk.
Check `git status` before committing: nothing from a local run belongs in a
commit.

`CLAUDE.md` in the repo root has the architecture, conventions, and change
recipes if the task goes beyond running the thing.
