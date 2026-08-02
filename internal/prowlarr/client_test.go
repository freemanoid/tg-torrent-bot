package prowlarr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// isTimeout reports whether err carries a network timeout anywhere in its
// wrap chain (http.Client.Timeout produces a url.Error with Timeout() true).
func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

// mustNew builds a Client or fails the test; the URLs in these tests are
// always valid.
func mustNew(t *testing.T, baseURL, apiKey string) *Client {
	t.Helper()
	c, err := New(baseURL, apiKey)
	if err != nil {
		t.Fatalf("New(%q): %v", baseURL, err)
	}
	return c
}

func TestNewRejectsInvalidURL(t *testing.T) {
	for _, u := range []string{"", "umbrel.local:9696", "ftp://umbrel.local:9696", "http://"} {
		t.Run("url="+u, func(t *testing.T) {
			if _, err := New(u, "k"); err == nil {
				t.Fatalf("New(%q): want error, got nil", u)
			}
		})
	}
}

// searchJSON is a realistic Prowlarr /api/v1/search payload: unsorted,
// mixed-tracker results, one release with a magnet and no download URL,
// and null-ish optional fields.
const searchJSON = `[
  {
    "guid": "https://tracker-b.example.com/forum/viewtopic.php?t=222",
    "title": "Космос. Сезон 2026. Этап 14 [WEB-DL 1080p, x265, Rus]",
    "size": 4831838208,
    "seeders": 120,
    "indexer": "TrackerB",
    "downloadUrl": "http://prowlarr:9696/2/download?apikey=k&link=abc",
    "magnetUrl": null,
    "infoHash": "aaaa1111",
    "publishDate": "2026-07-30T12:34:56Z"
  },
  {
    "guid": "https://tracker-a.example.com/forum/viewtopic.php?t=111",
    "title": "Space Show 2026 Episode 14 [1080p, HEVC]",
    "size": 2147483648,
    "seeders": 3500,
    "leechers": 87,
    "indexer": "TrackerA",
    "downloadUrl": "http://prowlarr:9696/1/download?apikey=k&link=def",
    "magnetUrl": "magnet:?xt=urn:btih:bbbb2222",
    "infoHash": "bbbb2222",
    "publishDate": "2026-07-31T08:00:00Z",
    "infoUrl": "https://tracker-a.example.com/forum/viewtopic.php?t=111",
    "description": "Season 2026, episode 14. Dual audio.",
    "grabs": 812,
    "files": 3
  },
  {
    "guid": "https://tracker-a.example.com/forum/viewtopic.php?t=333",
    "title": "Space Show 2026 E14 [720p, CamRip]",
    "seeders": 42,
    "indexer": "TrackerA",
    "magnetUrl": "magnet:?xt=urn:btih:cccc3333"
  }
]`

func TestSearchSortsAndMapsReleases(t *testing.T) {
	var gotPath, gotQuery, gotLimit, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotLimit = r.URL.Query().Get("limit")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(searchJSON)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	c := mustNew(t, srv.URL+"/", "secret-key") // trailing slash must not produce //api
	releases, err := c.Search(context.Background(), "космос 2026")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotPath != "/api/v1/search" {
		t.Errorf("path = %q, want /api/v1/search", gotPath)
	}
	if gotQuery != "космос 2026" {
		t.Errorf("query param = %q, want %q", gotQuery, "космос 2026")
	}
	if gotLimit != "50" {
		t.Errorf("limit param = %q, want 50", gotLimit)
	}
	if gotAPIKey != "secret-key" {
		t.Errorf("X-Api-Key = %q, want secret-key", gotAPIKey)
	}

	if len(releases) != 3 {
		t.Fatalf("got %d releases, want 3", len(releases))
	}
	// Sorted by seeders descending: 3500, 120, 42.
	if got := []int{releases[0].Seeders, releases[1].Seeders, releases[2].Seeders}; got[0] != 3500 || got[1] != 120 || got[2] != 42 {
		t.Errorf("seeders order = %v, want [3500 120 42]", got)
	}

	want := Release{
		GUID:        "https://tracker-a.example.com/forum/viewtopic.php?t=111",
		Title:       "Space Show 2026 Episode 14 [1080p, HEVC]",
		Size:        2147483648,
		Seeders:     3500,
		Leechers:    87,
		Indexer:     "TrackerA",
		DownloadURL: "http://prowlarr:9696/1/download?apikey=k&link=def",
		MagnetURL:   "magnet:?xt=urn:btih:bbbb2222",
		InfoHash:    "bbbb2222",
		Description: "Season 2026, episode 14. Dual audio.",
		InfoURL:     "https://tracker-a.example.com/forum/viewtopic.php?t=111",
		PublishDate: time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC),
		Grabs:       812,
		FileCount:   3,
	}
	if got := releases[0]; got != want {
		t.Errorf("release[0] = %+v, want %+v", got, want)
	}

	// Magnet-only release: empty DownloadURL, magnet preserved.
	last := releases[2]
	if last.DownloadURL != "" {
		t.Errorf("magnet-only release DownloadURL = %q, want empty", last.DownloadURL)
	}
	if last.MagnetURL != "magnet:?xt=urn:btih:cccc3333" {
		t.Errorf("MagnetURL = %q", last.MagnetURL)
	}
	if last.Size != 0 {
		t.Errorf("missing size = %d, want 0", last.Size)
	}
	// Indexers that report no leecher count decode as 0; the display treats
	// that the same as "not reported" and simply says nothing.
	if last.Leechers != 0 {
		t.Errorf("missing leechers = %d, want 0", last.Leechers)
	}
	// The detail fields are optional for every indexer: absent ones must decode
	// as zero values, never as an error that costs the whole search.
	if last.Description != "" || last.InfoURL != "" || last.Grabs != 0 || last.FileCount != 0 {
		t.Errorf("release without detail fields = %+v, want them zero", last)
	}
	if !last.PublishDate.IsZero() {
		t.Errorf("missing publishDate = %v, want zero", last.PublishDate)
	}
}

func TestSearchEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("[]")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	releases, err := mustNew(t, srv.URL, "k").Search(context.Background(), "nothing")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("got %d releases, want 0", len(releases))
	}
}

func TestSearchHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantInErr []string
	}{
		{
			name:      "unauthorized hints at API key",
			status:    http.StatusUnauthorized,
			body:      `{"error":"Unauthorized"}`,
			wantInErr: []string{"401", "PROWLARR_API_KEY"},
		},
		{
			name:      "server error includes status and body",
			status:    http.StatusInternalServerError,
			body:      "indexer tracker-a exploded",
			wantInErr: []string{"500", "indexer tracker-a exploded"},
		},
		{
			name:      "error without body still descriptive",
			status:    http.StatusBadGateway,
			wantInErr: []string{"502"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer srv.Close()

			_, err := mustNew(t, srv.URL, "k").Search(context.Background(), "q")
			if err == nil {
				t.Fatal("Search: want error, got nil")
			}
			for _, want := range tt.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestSearchBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("<html>not json</html>")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	_, err := mustNew(t, srv.URL, "k").Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search: want error on bad JSON, got nil")
	}
}

func TestFetchTorrent(t *testing.T) {
	torrent := []byte("d8:announce30:http://tracker.example/announcee")
	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/x-bittorrent")
		if _, err := w.Write(torrent); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	got, err := mustNew(t, srv.URL, "secret-key").FetchTorrent(context.Background(), srv.URL+"/1/download?link=abc")
	if err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if string(got) != string(torrent) {
		t.Errorf("torrent bytes = %q, want %q", got, torrent)
	}
	if gotAPIKey != "secret-key" {
		t.Errorf("X-Api-Key = %q, want secret-key", gotAPIKey)
	}
}

func TestFetchTorrentDoesNotSendAPIKeyToForeignHost(t *testing.T) {
	// Download URLs come from search results; if one points at a host other
	// than the configured Prowlarr, the API key must not travel with it.
	var foreignKey string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignKey = r.Header.Get("X-Api-Key")
		w.Write([]byte("d4:infoe"))
	}))
	defer foreign.Close()
	prowlarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d4:infoe"))
	}))
	defer prowlarrSrv.Close()

	c := mustNew(t, prowlarrSrv.URL, "secret-key")
	if _, err := c.FetchTorrent(context.Background(), foreign.URL+"/1/download"); err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if foreignKey != "" {
		t.Errorf("X-Api-Key %q leaked to a foreign host, want no header", foreignKey)
	}
}

func TestFetchTorrentStripsAPIKeyOnCrossHostRedirect(t *testing.T) {
	var foreignKey string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignKey = r.Header.Get("X-Api-Key")
		w.Write([]byte("d4:infoe"))
	}))
	defer foreign.Close()
	// Prowlarr redirects the download to a third-party tracker host.
	prowlarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/file.torrent", http.StatusFound)
	}))
	defer prowlarrSrv.Close()

	c := mustNew(t, prowlarrSrv.URL, "secret-key")
	got, err := c.FetchTorrent(context.Background(), prowlarrSrv.URL+"/1/download")
	if err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if string(got) != "d4:infoe" {
		t.Errorf("torrent bytes = %q", got)
	}
	if foreignKey != "" {
		t.Errorf("X-Api-Key %q followed a redirect off the Prowlarr host", foreignKey)
	}
}

func TestFetchTorrentRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		chunk := make([]byte, 1<<20)
		for written := 0; written <= maxTorrentSize; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, err := mustNew(t, srv.URL, "k").FetchTorrent(context.Background(), srv.URL+"/huge")
	if err == nil {
		t.Fatal("FetchTorrent: want error for a body over the size cap, got nil")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error %q should mention the size cap", err)
	}
}

func TestFetchTorrentNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "release is gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := mustNew(t, srv.URL, "k").FetchTorrent(context.Background(), srv.URL+"/1/download")
	if err == nil {
		t.Fatal("FetchTorrent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not contain 404", err)
	}
}

// stallingServer never responds until the request context is canceled.
func stallingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchContextDeadline(t *testing.T) {
	srv := stallingServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := mustNew(t, srv.URL, "k").Search(ctx, "q")
	if err == nil {
		t.Fatal("Search: want error on context deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v, want context.DeadlineExceeded", err)
	}
}

// New must wire DefaultTimeout into the HTTP client, and DefaultTimeout must
// stay long enough to outlast a FlareSolverr cold start. Indexers behind
// Cloudflare (TrackerA) need a full challenge + login on the first search
// after Prowlarr's cookie expires; that was measured at ~193 s on the Pi,
// against ~6 s once warm. A shorter timeout fails those searches outright and
// makes unattended subscription ticks silently miss a cycle.
func TestNewUsesDefaultTimeout(t *testing.T) {
	c := mustNew(t, "http://prowlarr.test:9696", "k")

	if c.httpc.Timeout != DefaultTimeout {
		t.Errorf("httpc.Timeout = %v, want DefaultTimeout (%v)", c.httpc.Timeout, DefaultTimeout)
	}
	if min := 240 * time.Second; DefaultTimeout < min {
		t.Errorf("DefaultTimeout = %v, want >= %v to survive a FlareSolverr cold start", DefaultTimeout, min)
	}
}

func TestSearchClientTimeout(t *testing.T) {
	srv := stallingServer(t)

	c := mustNew(t, srv.URL, "k")
	c.httpc.Timeout = 50 * time.Millisecond
	_, err := c.Search(context.Background(), "q")
	if err == nil {
		t.Fatal("Search: want error on client timeout, got nil")
	}
	if !isTimeout(err) {
		t.Errorf("error %v, want a timeout error", err)
	}
}

func TestFetchTorrentClientTimeout(t *testing.T) {
	srv := stallingServer(t)

	c := mustNew(t, srv.URL, "k")
	c.httpc.Timeout = 50 * time.Millisecond
	_, err := c.FetchTorrent(context.Background(), srv.URL+"/dl")
	if err == nil {
		t.Fatal("FetchTorrent: want error on client timeout, got nil")
	}
	if !isTimeout(err) {
		t.Errorf("error %v, want a timeout error", err)
	}
}

func TestPing(t *testing.T) {
	var gotPath, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := mustNew(t, srv.URL, "secret")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotPath != "/api/v1/health" {
		t.Errorf("path = %q, want /api/v1/health", gotPath)
	}
	if gotAPIKey != "secret" {
		t.Errorf("X-Api-Key = %q, want secret", gotAPIKey)
	}
}

func TestPingUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := mustNew(t, srv.URL, "bad").Ping(context.Background())
	if err == nil {
		t.Fatal("Ping: want error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "PROWLARR_API_KEY") {
		t.Errorf("error %q should hint at PROWLARR_API_KEY", err)
	}
}

func TestPingUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // connection refused from here on

	if err := mustNew(t, srv.URL, "k").Ping(context.Background()); err == nil {
		t.Fatal("Ping: want error when server is unreachable, got nil")
	}
}
