// Package transmission wraps hekmon/transmissionrpc/v3 behind the narrow
// interface the rest of the bot needs: add a torrent (raw metainfo bytes or a
// magnet link) and list the status of every torrent Transmission knows about.
package transmission

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	transmissionrpc "github.com/hekmon/transmissionrpc/v3"
)

// DefaultTimeout bounds each RPC request.
const DefaultTimeout = 30 * time.Second

// defaultRPCPath is appended to the base URL when it carries no explicit path
// (Transmission serves RPC there by default).
const defaultRPCPath = "/transmission/rpc"

// ErrNotFound reports that Transmission has no torrent with the given hash.
// Removing something that is already gone is the expected outcome of a second
// tap on a stale button, not a failure worth surfacing as one.
var ErrNotFound = errors.New("transmission: torrent not found")

// Interface is the narrow surface the rest of the app depends on; fakes
// implement it in tests of the subscription engine, watcher, and bot handlers.
type Interface interface {
	// AddTorrent adds a torrent from raw .torrent metainfo bytes and returns
	// its info hash. Adding a torrent Transmission already has is a success
	// and returns the existing torrent's hash.
	AddTorrent(ctx context.Context, meta []byte) (hash string, err error)
	// AddMagnet adds a torrent from a magnet link; same semantics as AddTorrent.
	AddMagnet(ctx context.Context, link string) (hash string, err error)
	// Active returns the status of every torrent currently in Transmission
	// (including finished ones); callers filter by hash.
	Active(ctx context.Context) ([]TorrentStatus, error)
	// RemoveTorrent deletes the torrent with the given info hash along with
	// whatever it downloaded. A hash Transmission does not know yields
	// ErrNotFound rather than a generic failure.
	RemoveTorrent(ctx context.Context, hash string) error
}

// TorrentStatus is a snapshot of one torrent's download progress.
type TorrentStatus struct {
	Name    string
	Hash    string        // info hash (Transmission's hashString)
	Percent float64       // fraction done, 0..1
	Rate    int64         // download rate, bytes/s
	ETA     time.Duration // estimated time remaining; negative = unknown
	Done    bool          // fully downloaded (Percent == 1)
}

// Client talks to a single Transmission instance. Create it with New.
type Client struct {
	rpc   *transmissionrpc.Client
	httpc *http.Client // kept so tests can shorten the timeout
}

var _ Interface = (*Client)(nil)

// New creates a Client for the Transmission instance at baseURL (scheme +
// host, e.g. http://umbrel.local:9091). When baseURL has no path the default
// /transmission/rpc is used; an explicit path is preserved. user/pass enable
// HTTP basic auth when non-empty.
func New(baseURL, user, pass string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid transmission URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid transmission URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid transmission URL %q: missing host", baseURL)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = defaultRPCPath
	}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}

	httpc := &http.Client{Timeout: DefaultTimeout}
	rpc, err := transmissionrpc.New(u, &transmissionrpc.Config{CustomClient: httpc})
	if err != nil {
		return nil, fmt.Errorf("create transmission RPC client: %w", err)
	}
	return &Client{rpc: rpc, httpc: httpc}, nil
}

// AddTorrent implements Interface by handing Transmission the base64-encoded
// metainfo, avoiding any need for Transmission to reach the torrent's origin.
func (c *Client) AddTorrent(ctx context.Context, meta []byte) (string, error) {
	if len(meta) == 0 {
		return "", errors.New("empty torrent metainfo")
	}
	b64 := base64.StdEncoding.EncodeToString(meta)
	return c.add(ctx, transmissionrpc.TorrentAddPayload{MetaInfo: &b64})
}

// AddMagnet implements Interface by passing the magnet link as the filename
// argument of torrent-add.
func (c *Client) AddMagnet(ctx context.Context, link string) (string, error) {
	if link == "" {
		return "", errors.New("empty magnet link")
	}
	return c.add(ctx, transmissionrpc.TorrentAddPayload{Filename: &link})
}

func (c *Client) add(ctx context.Context, payload transmissionrpc.TorrentAddPayload) (string, error) {
	// TorrentAdd already folds torrent-duplicate into a successful answer, so
	// re-adding an existing torrent yields its hash rather than an error.
	torrent, err := c.rpc.TorrentAdd(ctx, payload)
	if err != nil {
		return "", fmt.Errorf("transmission torrent-add: %w", err)
	}
	if torrent.HashString == nil || *torrent.HashString == "" {
		return "", errors.New("transmission torrent-add: response has no info hash")
	}
	return *torrent.HashString, nil
}

// RemoveTorrent implements Interface. torrent-remove takes numeric torrent
// ids rather than hashes in this client library, so the hash is resolved first
// — which doubles as the existence check behind ErrNotFound.
func (c *Client) RemoveTorrent(ctx context.Context, hash string) error {
	if hash == "" {
		return errors.New("empty info hash")
	}
	torrents, err := c.rpc.TorrentGetHashes(ctx, []string{"id"}, []string{hash})
	if err != nil {
		return fmt.Errorf("transmission torrent-get for %s: %w", hash, err)
	}
	if len(torrents) == 0 || torrents[0].ID == nil {
		return fmt.Errorf("transmission: %s: %w", hash, ErrNotFound)
	}
	// Deleting the data is the point: the user rejected this release, and
	// leaving a partial download behind would need an SSH session to clean up.
	err = c.rpc.TorrentRemove(ctx, transmissionrpc.TorrentRemovePayload{
		IDs:             []int64{*torrents[0].ID},
		DeleteLocalData: true,
	})
	if err != nil {
		return fmt.Errorf("transmission torrent-remove %s: %w", hash, err)
	}
	return nil
}

// activeFields is the subset of torrent-get fields TorrentStatus needs.
var activeFields = []string{"name", "hashString", "percentDone", "rateDownload", "eta"}

// Active implements Interface.
func (c *Client) Active(ctx context.Context) ([]TorrentStatus, error) {
	torrents, err := c.rpc.TorrentGet(ctx, activeFields, nil)
	if err != nil {
		return nil, fmt.Errorf("transmission torrent-get: %w", err)
	}
	statuses := make([]TorrentStatus, 0, len(torrents))
	for _, t := range torrents {
		s := TorrentStatus{ETA: -time.Second} // unknown until reported
		if t.Name != nil {
			s.Name = *t.Name
		}
		if t.HashString != nil {
			s.Hash = *t.HashString
		}
		if t.PercentDone != nil {
			s.Percent = *t.PercentDone
		}
		if t.RateDownload != nil {
			s.Rate = *t.RateDownload
		}
		if t.ETA != nil {
			s.ETA = time.Duration(*t.ETA) * time.Second
		}
		s.Done = s.Percent >= 1
		statuses = append(statuses, s)
	}
	return statuses, nil
}
