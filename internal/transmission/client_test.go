package transmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rpcHandler handles one decoded RPC call and returns the "result" string and
// the "arguments" object for the response body.
type rpcHandler func(t *testing.T, method string, args map[string]any) (result string, respArgs any)

// fakeServer implements the minimal transmission-rpc protocol: basic auth,
// the X-Transmission-Session-Id CSRF handshake (409 on missing/stale ID), and
// JSON-RPC-ish request/response bodies with tag echoing.
type fakeServer struct {
	t          *testing.T
	user, pass string // basic auth required when user != ""
	sessionID  string
	path       string // expected RPC path; default /transmission/rpc
	handler    rpcHandler
	conflicts  atomic.Int32 // 409 handshake responses served
	calls      atomic.Int32 // successful RPC calls served
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wantPath := f.path
	if wantPath == "" {
		wantPath = "/transmission/rpc"
	}
	if r.URL.Path != wantPath {
		f.t.Errorf("unexpected request path %q, want %q", r.URL.Path, wantPath)
		http.NotFound(w, r)
		return
	}
	if f.user != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if r.Header.Get("X-Transmission-Session-Id") != f.sessionID {
		w.Header().Set("X-Transmission-Session-Id", f.sessionID)
		w.WriteHeader(http.StatusConflict)
		f.conflicts.Add(1)
		return
	}
	var req struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
		Tag       int            `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode request body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	result, respArgs := f.handler(f.t, req.Method, req.Arguments)
	if respArgs == nil {
		respArgs = map[string]any{}
	}
	f.calls.Add(1)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"result":    result,
		"arguments": respArgs,
		"tag":       req.Tag,
	}); err != nil {
		f.t.Errorf("encode response: %v", err)
	}
}

// newFake starts a fake transmission RPC server and a Client pointed at it.
func newFake(t *testing.T, handler rpcHandler) (*Client, *fakeServer) {
	t.Helper()
	fake := &fakeServer{t: t, sessionID: "sess-123", handler: handler}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", "")
	if err != nil {
		t.Fatalf("New(%q): %v", srv.URL, err)
	}
	return c, fake
}

func added(hash string) map[string]any {
	return map[string]any{
		"torrent-added": map[string]any{"id": 1, "name": "Some.Release", "hashString": hash},
	}
}

func TestAddTorrentSendsBase64Metainfo(t *testing.T) {
	meta := []byte("d8:announce30:fake-tracker-not-a-real-thing0e")
	wantB64 := base64.StdEncoding.EncodeToString(meta)

	c, fake := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		if method != "torrent-add" {
			t.Errorf("method = %q, want torrent-add", method)
		}
		if got, _ := args["metainfo"].(string); got != wantB64 {
			t.Errorf("metainfo = %q, want %q", got, wantB64)
		}
		if _, ok := args["filename"]; ok {
			t.Errorf("filename must not be set when adding metainfo, got %v", args["filename"])
		}
		return "success", added("abc123hash")
	})

	hash, err := c.AddTorrent(context.Background(), meta)
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if hash != "abc123hash" {
		t.Errorf("hash = %q, want abc123hash", hash)
	}
	if fake.conflicts.Load() == 0 {
		t.Error("expected at least one 409 CSRF session handshake")
	}
	if fake.calls.Load() != 1 {
		t.Errorf("RPC calls = %d, want 1", fake.calls.Load())
	}
}

func TestAddTorrentEmptyMeta(t *testing.T) {
	c, fake := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		t.Errorf("unexpected RPC call %q", method)
		return "success", added("x")
	})
	if _, err := c.AddTorrent(context.Background(), nil); err == nil {
		t.Fatal("AddTorrent(nil) error = nil, want error")
	}
	if fake.calls.Load() != 0 {
		t.Errorf("RPC calls = %d, want 0", fake.calls.Load())
	}
}

func TestAddMagnetSendsFilename(t *testing.T) {
	link := "magnet:?xt=urn:btih:deadbeefcafe&dn=Some.Release"

	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		if method != "torrent-add" {
			t.Errorf("method = %q, want torrent-add", method)
		}
		if got, _ := args["filename"].(string); got != link {
			t.Errorf("filename = %q, want %q", got, link)
		}
		if _, ok := args["metainfo"]; ok {
			t.Errorf("metainfo must not be set when adding a magnet, got %v", args["metainfo"])
		}
		return "success", added("deadbeefcafe")
	})

	hash, err := c.AddMagnet(context.Background(), link)
	if err != nil {
		t.Fatalf("AddMagnet: %v", err)
	}
	if hash != "deadbeefcafe" {
		t.Errorf("hash = %q, want deadbeefcafe", hash)
	}
}

func TestAddMagnetEmptyLink(t *testing.T) {
	c, fake := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		t.Errorf("unexpected RPC call %q", method)
		return "success", added("x")
	})
	if _, err := c.AddMagnet(context.Background(), ""); err == nil {
		t.Fatal(`AddMagnet("") error = nil, want error`)
	}
	if fake.calls.Load() != 0 {
		t.Errorf("RPC calls = %d, want 0", fake.calls.Load())
	}
}

func TestAddTorrentDuplicateIsSuccess(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", map[string]any{
			"torrent-duplicate": map[string]any{"id": 7, "name": "Already.There", "hashString": "existinghash"},
		}
	})

	hash, err := c.AddTorrent(context.Background(), []byte("meta"))
	if err != nil {
		t.Fatalf("AddTorrent duplicate: %v", err)
	}
	if hash != "existinghash" {
		t.Errorf("hash = %q, want existinghash (the pre-existing torrent's hash)", hash)
	}
}

func TestAddTorrentRPCFailure(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		return "invalid or corrupt torrent file", nil
	})

	_, err := c.AddTorrent(context.Background(), []byte("junk"))
	if err == nil {
		t.Fatal("AddTorrent error = nil, want RPC failure error")
	}
	if !strings.Contains(err.Error(), "invalid or corrupt torrent file") {
		t.Errorf("error %q does not surface the RPC failure reason", err)
	}
}

func TestAddTorrentMissingHash(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", map[string]any{
			"torrent-added": map[string]any{"id": 1, "name": "No.Hash"},
		}
	})

	if _, err := c.AddTorrent(context.Background(), []byte("meta")); err == nil {
		t.Fatal("AddTorrent error = nil, want error for missing hashString")
	}
}

func TestActiveMapsFields(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		if method != "torrent-get" {
			t.Errorf("method = %q, want torrent-get", method)
		}
		rawFields, _ := args["fields"].([]any)
		var fields []string
		for _, f := range rawFields {
			s, _ := f.(string)
			fields = append(fields, s)
		}
		for _, want := range []string{"name", "hashString", "percentDone", "rateDownload", "eta"} {
			if !slices.Contains(fields, want) {
				t.Errorf("requested fields %v missing %q", fields, want)
			}
		}
		return "success", map[string]any{
			"torrents": []any{
				map[string]any{
					"name":         "Космос. Сезон 2026 [1080p, x265, Rus]",
					"hashString":   "aaa111",
					"percentDone":  0.42,
					"rateDownload": 1250000,
					"eta":          300,
				},
				map[string]any{
					"name":         "Old.Movie.1994.BDRip",
					"hashString":   "bbb222",
					"percentDone":  1.0,
					"rateDownload": 0,
					"eta":          -1,
				},
			},
		}
	})

	got, err := c.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}

	first := got[0]
	if first.Name != "Космос. Сезон 2026 [1080p, x265, Rus]" {
		t.Errorf("Name = %q", first.Name)
	}
	if first.Hash != "aaa111" {
		t.Errorf("Hash = %q, want aaa111", first.Hash)
	}
	if first.Percent != 0.42 {
		t.Errorf("Percent = %v, want 0.42", first.Percent)
	}
	if first.Rate != 1250000 {
		t.Errorf("Rate = %d, want 1250000", first.Rate)
	}
	if first.ETA != 300*time.Second {
		t.Errorf("ETA = %v, want 5m0s", first.ETA)
	}
	if first.Done {
		t.Error("Done = true for torrent at 42 percent")
	}

	second := got[1]
	if !second.Done {
		t.Error("Done = false for fully downloaded torrent")
	}
	if second.ETA >= 0 {
		t.Errorf("ETA = %v, want negative (unknown) for finished torrent", second.ETA)
	}
}

func TestActiveEmpty(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", map[string]any{"torrents": []any{}}
	})

	got, err := c.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestActiveRPCFailure(t *testing.T) {
	c, _ := newFake(t, func(t *testing.T, method string, args map[string]any) (string, any) {
		return "something went wrong", nil
	})

	if _, err := c.Active(context.Background()); err == nil {
		t.Fatal("Active error = nil, want RPC failure error")
	}
}

func TestAuthSuccess(t *testing.T) {
	fake := &fakeServer{t: t, user: "umbrel", pass: "s3cret", sessionID: "sess-123"}
	fake.handler = func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", added("authedhash")
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "umbrel", "s3cret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:feed")
	if err != nil {
		t.Fatalf("AddMagnet with auth: %v", err)
	}
	if hash != "authedhash" {
		t.Errorf("hash = %q, want authedhash", hash)
	}
}

func TestAuthFailure(t *testing.T) {
	fake := &fakeServer{t: t, user: "umbrel", pass: "s3cret", sessionID: "sess-123"}
	fake.handler = func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", added("nope")
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "umbrel", "wrong-password")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:feed")
	if err == nil {
		t.Fatal("AddMagnet error = nil, want auth error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not mention HTTP 401", err)
	}
}

func TestTimeout(t *testing.T) {
	fake := &fakeServer{t: t, sessionID: "sess-123"}
	fake.handler = func(t *testing.T, method string, args map[string]any) (string, any) {
		time.Sleep(500 * time.Millisecond)
		return "success", added("slow")
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.httpc.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err = c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:feed")
	if err == nil {
		t.Fatal("AddMagnet error = nil, want timeout error")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Errorf("error %v, want a net.Error with Timeout() == true", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("timed out after %v, want ~50ms", elapsed)
	}
}

func TestNewPreservesCustomRPCPath(t *testing.T) {
	fake := &fakeServer{t: t, sessionID: "sess-123", path: "/custom/rpc"}
	fake.handler = func(t *testing.T, method string, args map[string]any) (string, any) {
		return "success", added("custompath")
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL+"/custom/rpc", "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:feed")
	if err != nil {
		t.Fatalf("AddMagnet via custom path: %v", err)
	}
	if hash != "custompath" {
		t.Errorf("hash = %q, want custompath", hash)
	}
}

func TestNewBadURL(t *testing.T) {
	for _, bad := range []string{"://missing-scheme", "", "not a url at all\x00"} {
		if _, err := New(bad, "", ""); err == nil {
			t.Errorf("New(%q) error = nil, want error", bad)
		}
	}
}
