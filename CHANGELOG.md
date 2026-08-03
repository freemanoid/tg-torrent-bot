# Changelog

Release notes written for the person using the bot. This file is embedded in
the binary: after an update the bot posts the entry matching the version it is
running, so keep entries short, plain, and headed `## v<version>` — the same
version the git tag carries.

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
