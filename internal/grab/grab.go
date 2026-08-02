// Package grab hands Prowlarr releases to Transmission. It is the one shared
// spot for the "prefer .torrent, fall back to magnet" policy used by both the
// interactive search flow and the subscription engine.
package grab

import (
	"context"
	"errors"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// TorrentFetcher downloads raw .torrent metainfo bytes; *prowlarr.Client
// implements it, tests fake it.
type TorrentFetcher interface {
	FetchTorrent(ctx context.Context, downloadURL string) ([]byte, error)
}

// AddRelease hands the release to Transmission, preferring the .torrent file
// (fetched through Prowlarr's proxy) over a magnet link so Transmission never
// needs network access to the tracker or the bot. When the .torrent fetch
// fails but the release also carries a magnet link, the magnet is used as a
// fallback: Prowlarr answers magnet-backed downloadUrls with a redirect to a
// magnet: URI, which the HTTP fetch cannot follow.
func AddRelease(ctx context.Context, fetcher TorrentFetcher, trans transmission.Interface, r prowlarr.Release) (string, error) {
	if r.DownloadURL == "" && r.MagnetURL == "" {
		return "", errors.New("release has no download link")
	}
	if r.DownloadURL != "" {
		meta, err := fetcher.FetchTorrent(ctx, r.DownloadURL)
		if err == nil {
			return trans.AddTorrent(ctx, meta)
		}
		if r.MagnetURL == "" {
			return "", err
		}
		// fetch failed but a magnet link exists — fall through to it
	}
	return trans.AddMagnet(ctx, r.MagnetURL)
}
