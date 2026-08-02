// Package tgtorrentbot exists only to embed repository files that the binary
// needs at runtime. go:embed cannot reach outside its own package directory,
// and CHANGELOG.md belongs at the repository root where people expect to find
// it — so the embed lives here rather than inside internal/release.
package tgtorrentbot

import _ "embed"

// Changelog is the raw text of CHANGELOG.md. internal/release parses the entry
// for the running version out of it and posts that after an update.
//
//go:embed CHANGELOG.md
var Changelog string
