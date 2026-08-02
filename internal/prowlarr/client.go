// Package prowlarr provides a minimal client for Prowlarr's v1 search API:
// searching across all configured indexers and downloading .torrent files
// through Prowlarr's proxy.
package prowlarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds each HTTP request. Prowlarr fans a search out to slow
// trackers, so this is generous. It has to cover the worst case rather than the
// common one: indexers behind Cloudflare (TrackerA) are proxied through
// FlareSolverr, and the first search after Prowlarr's session cookie expires
// pays for a full browser challenge and login — ~193 s when measured on the Pi,
// against ~6 s once the cookie is warm. Timing that out would fail the search
// outright and make an unattended subscription tick miss its cycle.
const DefaultTimeout = 240 * time.Second

// searchLimit caps how many releases one search requests from Prowlarr.
const searchLimit = 50

// maxTorrentSize caps FetchTorrent downloads; .torrent metainfo files are at
// most a few megabytes, so anything larger is a broken or hostile response.
const maxTorrentSize = 32 << 20

// Release is one search result from Prowlarr's /api/v1/search endpoint.
//
// Everything below InfoHash is optional colour for the detail view: which
// fields an indexer fills in varies wildly, so every one of them may be zero
// and the display must say nothing rather than guess. The struct stays
// comparable on purpose — tests compare whole releases.
type Release struct {
	GUID        string `json:"guid"`
	Title       string `json:"title"`
	Size        int64  `json:"size"` // bytes
	Seeders     int    `json:"seeders"`
	Leechers    int    `json:"leechers"`
	Indexer     string `json:"indexer"`     // e.g. "TrackerA", "TrackerB"
	DownloadURL string `json:"downloadUrl"` // .torrent proxied through Prowlarr; may be empty
	MagnetURL   string `json:"magnetUrl"`   // may be empty
	InfoHash    string `json:"infoHash"`

	Description string    `json:"description"` // free text from the indexer; usually short, often absent
	InfoURL     string    `json:"infoUrl"`     // the release's page on the tracker
	PublishDate time.Time `json:"publishDate"` // zero when the indexer reported none
	Grabs       int       `json:"grabs"`       // how many times the tracker says it was downloaded
	FileCount   int       `json:"files"`       // file count as the indexer reports it
}

// Client talks to a single Prowlarr instance.
type Client struct {
	baseURL string
	apiHost string // host the API key may be sent to
	apiKey  string
	httpc   *http.Client
}

// New creates a Client for the Prowlarr instance at baseURL (scheme + host,
// e.g. http://umbrel.local:9696), authenticating with apiKey. The URL is
// validated up front: a malformed PROWLARR_URL must be a clear startup error,
// not a stream of confusing 401s from an API key that never gets attached.
func New(baseURL, apiKey string) (*Client, error) {
	trimmed := strings.TrimRight(baseURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid prowlarr URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid prowlarr URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid prowlarr URL %q: missing host", baseURL)
	}

	c := &Client{
		baseURL: trimmed,
		apiHost: u.Host,
		apiKey:  apiKey,
		httpc:   &http.Client{Timeout: DefaultTimeout},
	}
	// Go forwards custom headers across redirects; never let the API key
	// follow a redirect off the Prowlarr host.
	c.httpc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !strings.EqualFold(req.URL.Host, c.apiHost) {
			req.Header.Del("X-Api-Key")
		}
		return nil
	}
	return c, nil
}

// Search runs query across all Prowlarr indexers and returns releases sorted
// by seeders, most first. An empty result is not an error.
func (c *Client) Search(ctx context.Context, query string) ([]Release, error) {
	params := url.Values{
		"query": {query},
		"limit": {strconv.Itoa(searchLimit)},
	}
	resp, err := c.get(ctx, c.baseURL+"/api/v1/search?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("prowlarr search %q: %w", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prowlarr search %q: %s", query, statusError(resp))
	}

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("prowlarr search %q: decode response: %w", query, err)
	}
	slices.SortStableFunc(releases, func(a, b Release) int { return b.Seeders - a.Seeders })
	return releases, nil
}

// Ping verifies Prowlarr is reachable and the API key is accepted by hitting
// the authenticated /api/v1/health endpoint. Used as a startup self-check.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.get(ctx, c.baseURL+"/api/v1/health")
	if err != nil {
		return fmt.Errorf("prowlarr ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prowlarr ping: %s", statusError(resp))
	}
	return nil
}

// FetchTorrent downloads raw .torrent metainfo bytes from downloadURL
// (normally a Release.DownloadURL, proxied through Prowlarr).
func (c *Client) FetchTorrent(ctx context.Context, downloadURL string) ([]byte, error) {
	resp, err := c.get(ctx, downloadURL)
	if err != nil {
		return nil, fmt.Errorf("prowlarr fetch torrent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prowlarr fetch torrent: %s", statusError(resp))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentSize+1))
	if err != nil {
		return nil, fmt.Errorf("prowlarr fetch torrent: read body: %w", err)
	}
	if len(data) > maxTorrentSize {
		return nil, fmt.Errorf("prowlarr fetch torrent: file larger than %d MB", maxTorrentSize>>20)
	}
	return data, nil
}

// get issues a GET, attaching the API key only for requests to the configured
// Prowlarr host: FetchTorrent is called with URLs taken from search results,
// and if an indexer (or a redirecting Prowlarr) points at a third-party host
// the key must not leak there. The caller owns the response body.
func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(req.URL.Host, c.apiHost) {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	return c.httpc.Do(req)
}

// statusError renders a non-200 response as a short descriptive message,
// consuming (a prefix of) the body.
func statusError(resp *http.Response) string {
	if resp.StatusCode == http.StatusUnauthorized {
		return "HTTP 401 Unauthorized — check PROWLARR_API_KEY"
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return fmt.Sprintf("HTTP %s: %s", resp.Status, msg)
	}
	return "HTTP " + resp.Status
}
