---
name: releasing
description: Cut and push the version tag that releases tg-torrent-bot — picking the next semantic version, tagging, and reporting what CI does downstream. Use this whenever a change that affects the running bot lands on master, without waiting to be asked, and whenever the user says release, ship, cut a version, tag, publish, or asks whether something will reach the Pi. Releasing is the last step of shipping, not a separate errand.
---

# Releasing tg-torrent-bot

A release *is* a pushed `v*` tag. Nothing else publishes anything:
`.github/workflows/release.yml` triggers on `tags: ["v*"]`, builds the
multi-arch image, pushes it to `ghcr.io/freemanoid/tg-torrent-bot`, and
auto-commits the version and pinned digest to the
[app store repo](https://github.com/freemanoid/umbrel-app-store). Umbrel then
offers the update on the Pi.

## Tag without being asked

When work that changes the running bot lands on master, cut the tag as the
final step of shipping it. Do not stop to ask — the owner set this up
deliberately so finishing a change does not need a second prompt, and being
asked every time was the friction worth removing.

That standing go-ahead covers the tag and its push. It does not extend to
anything else that reaches the outside world.

Skip the release, and say so in one line, when:

- Nothing user-facing changed — docs, CI config, comments, tests only. There is
  no image worth publishing.
- The work is on a feature branch that has not been merged to master.
- The user said not to release, called the work WIP or an experiment, or is
  mid-way through a series of commits that belong in one version.

## Pick the version

Read the last tag with `git tag --sort=-v:refname | head -1` and bump from it:

| Bump | When |
|---|---|
| major | A config field is removed or renamed, or a store migration cannot be rolled back — anything that breaks an existing install on update. |
| minor | A new user-visible capability. Usually a `feat:` commit. |
| patch | A bug fix, dependency bump, or performance change that alters nothing about how the bot is used. |

If the range since the last tag mixes kinds, take the largest bump in it.

## Cut it

Check first that master is clean, pushed, and green — `git status`,
`go test ./...`, `go vet ./...`. This matters more than it looks: `release.yml`
does not depend on `ci.yml`, so a tag pushed on a broken commit publishes a
broken image and hands it to Umbrel as an update.

Tags are annotated and read `vX.Y.Z: <short summary>`, matching every tag
before them:

```sh
git tag -a v1.4.0 -m "v1.4.0: subscription pause command"
git push origin v1.4.0
```

Then report the run — `gh run list --workflow=release.yml --limit 1` — and say
plainly that this is what puts the update on the Pi.

## A published tag is immutable

Once a tag is pushed, the app store repo pins the image digest built from it.
Moving or deleting that tag leaves the store pointing at a digest whose tag no
longer means the same thing. A broken release is fixed by releasing the next
patch version forward, never by force-pushing over one.

Never hand-edit the two files in the app store repo either; the release job
owns them.
