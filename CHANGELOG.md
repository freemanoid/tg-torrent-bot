# Changelog

Release notes written for the person using the bot. This file is embedded in
the binary: after an update the bot posts the entry matching the version it is
running, so keep entries short, plain, and headed `## v<version>` — the same
version the git tag carries.

## v1.12.0

- `/status` can now delete. Every download it lists — running or recently
  finished — is numbered and has a matching 🗑 button underneath. Tap one,
  confirm, and the torrent goes from Transmission along with the files it
  wrote. Until now only the torrents a subscription grabbed by itself could be
  deleted from the chat, and only from the message announcing them; anything
  you picked out of search results had to be cleaned up by hand.
- An entry that reads "not in Transmission (removed externally?)" can be
  cleared the same way, so a download you removed elsewhere stops haunting the
  list.

## v1.11.0

- Search results now arrive as a new message instead of replacing the
  "🔎 Searching…" one. A search can run for minutes, and an edited message
  raises no Telegram notification — so you had to keep checking the chat to
  see whether it had finished. Now your phone tells you. The same goes for
  "no results" and for a search that failed.

## v1.10.0

- A search word starting with `-` now excludes: send
  `формула 1 2026 2160p -AV1` and results with AV1 in the title are dropped.
  Regex works too (`-/av1|vp9/`). Until now those words were passed straight to
  the trackers, which each read them their own way — usually as no filter at
  all. The bot filters the results itself now, so it works the same whichever
  tracker answered.
- The 🔔 button keeps those exclusions: subscribing to a search with `-AV1`
  gives you a subscription that will not grab AV1 either.

## v1.9.0

- Subscribing is now one tap: under any search results there is a 🔔 button
  that watches that exact search. No filter syntax, no retyping — send the
  query you mean, then tap.
- New subscriptions only grab releases published **after** you created them.
  Subscribe partway through a series and you get the next episode and every
  one after it, not the whole back catalogue. Add the `backlog` filter when
  you do want what is already on the tracker. Subscriptions you already have
  are untouched and keep grabbing as before.
- Every torrent a subscription grabs by itself now comes with a 🗑 button, on
  the "grabbed" message and on the "finished" one. Tap it and confirm, and the
  bot removes the torrent from Transmission and deletes what it downloaded.
  The subscription will not grab that release again.
- `/subs` shows each subscription's cutoff date, and `/test` now judges
  releases exactly as the subscription will.

## v1.8.0

- The settings page now takes a list of allowed chat IDs, not just one, so the
  bot can be shared — with a partner, a family group, a second device. Each
  search is answered in the chat that asked for it; subscription grabs and
  "finished" notices go to everyone on the list. Subscriptions and downloads
  belong to the bot rather than to one chat, so anyone allowed can list, pause
  or remove any of them.
- The settings page shows the bot token, Prowlarr API key and Transmission
  password in full instead of blank boxes, so they can be read and copied
  straight from the form. Because every value is now visible, saving stores
  exactly what the form shows: emptying a field clears that setting rather
  than keeping the old value.

## v1.7.0

- Search results now carry 🔗 buttons: tap one to open that release's page on
  the tracker — screenshots, notes, comments, everything the search itself
  cannot show. Only results whose indexer published a page get a button.

## v1.6.0

- New ℹ️ buttons under the search results: tap one to see that release in full
  before downloading it — the untruncated title, when the tracker published it
  and how often it was taken, its description and page link, and the list of
  files inside the torrent with their sizes.
- The details message carries its own ⬇️ Download button, so nothing is lost by
  looking first.

## v1.5.0

- Search results now show what you already have: ⬇️ next to a release that is
  still downloading, ✅ next to one that has finished.
- /status lists the last 10 finished downloads under the active ones, and no
  longer goes silent when Transmission is unreachable.

## v1.4.0

- The bot now announces its own updates: after an app update it posts the new
  version and a short list of what changed.

## v1.3.0

- Search results describe each release in full — resolution, source, codecs,
  audio, translations and subtitles — instead of a single clipped line.

## v1.2.0

- A search is acknowledged the moment you send it, so a query that takes
  minutes no longer looks ignored.

## v1.1.0

- Settings moved into a web page: a fresh install starts in setup mode and can
  be configured from the browser instead of over SSH.

## v1.0.3

- Fixes to the published image and deployment.

## v1.0.2

- First published Umbrel app release.
