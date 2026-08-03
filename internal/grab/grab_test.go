package grab

import (
	"context"
	"errors"
	"testing"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

type fakeFetcher struct {
	torrent []byte
	err     error
	fetched []string
}

func (f *fakeFetcher) FetchTorrent(_ context.Context, url string) ([]byte, error) {
	f.fetched = append(f.fetched, url)
	if f.err != nil {
		return nil, f.err
	}
	return f.torrent, nil
}

type fakeTrans struct {
	hash    string
	addErr  error
	meta    [][]byte
	magnets []string
}

func (f *fakeTrans) AddTorrent(_ context.Context, meta []byte) (string, error) {
	f.meta = append(f.meta, meta)
	return f.hash, f.addErr
}

func (f *fakeTrans) AddMagnet(_ context.Context, link string) (string, error) {
	f.magnets = append(f.magnets, link)
	return f.hash, f.addErr
}

func (f *fakeTrans) Active(context.Context) ([]transmission.TorrentStatus, error) {
	return nil, nil
}

func (f *fakeTrans) RemoveTorrent(context.Context, string) error { return nil }

var _ transmission.Interface = (*fakeTrans)(nil)

func TestAddReleasePrefersTorrentFile(t *testing.T) {
	fetch := &fakeFetcher{torrent: []byte("meta")}
	trans := &fakeTrans{hash: "h1"}

	hash, err := AddRelease(context.Background(), fetch, trans, prowlarr.Release{
		DownloadURL: "http://prowlarr/dl/1",
		MagnetURL:   "magnet:?xt=urn:btih:abc",
	})
	if err != nil {
		t.Fatalf("AddRelease: %v", err)
	}
	if hash != "h1" {
		t.Errorf("hash = %q, want h1", hash)
	}
	if len(trans.meta) != 1 || string(trans.meta[0]) != "meta" {
		t.Errorf("AddTorrent meta = %q, want the fetched bytes", trans.meta)
	}
	if len(trans.magnets) != 0 {
		t.Errorf("AddMagnet called %d times, want 0", len(trans.magnets))
	}
}

func TestAddReleaseMagnetOnly(t *testing.T) {
	fetch := &fakeFetcher{}
	trans := &fakeTrans{hash: "h2"}

	hash, err := AddRelease(context.Background(), fetch, trans, prowlarr.Release{
		MagnetURL: "magnet:?xt=urn:btih:def",
	})
	if err != nil {
		t.Fatalf("AddRelease: %v", err)
	}
	if hash != "h2" {
		t.Errorf("hash = %q, want h2", hash)
	}
	if len(fetch.fetched) != 0 {
		t.Error("FetchTorrent called for a magnet-only release")
	}
	if got := trans.magnets; len(got) != 1 || got[0] != "magnet:?xt=urn:btih:def" {
		t.Errorf("AddMagnet links = %q, want the magnet URL", got)
	}
}

func TestAddReleaseFetchFailureFallsBackToMagnet(t *testing.T) {
	fetch := &fakeFetcher{err: errors.New(`unsupported protocol scheme "magnet"`)}
	trans := &fakeTrans{hash: "h3"}

	hash, err := AddRelease(context.Background(), fetch, trans, prowlarr.Release{
		DownloadURL: "http://prowlarr/dl/1",
		MagnetURL:   "magnet:?xt=urn:btih:fee",
	})
	if err != nil {
		t.Fatalf("AddRelease: %v", err)
	}
	if hash != "h3" {
		t.Errorf("hash = %q, want h3", hash)
	}
	if got := trans.magnets; len(got) != 1 || got[0] != "magnet:?xt=urn:btih:fee" {
		t.Errorf("AddMagnet links = %q, want the magnet fallback", got)
	}
	if len(trans.meta) != 0 {
		t.Error("AddTorrent called although the fetch failed")
	}
}

func TestAddReleaseFetchFailureWithoutMagnet(t *testing.T) {
	fetch := &fakeFetcher{err: errors.New("proxy error")}
	trans := &fakeTrans{hash: "h4"}

	_, err := AddRelease(context.Background(), fetch, trans, prowlarr.Release{
		DownloadURL: "http://prowlarr/dl/1",
	})
	if err == nil {
		t.Fatal("AddRelease: want fetch error, got nil")
	}
	if len(trans.meta) != 0 || len(trans.magnets) != 0 {
		t.Error("nothing must be added when the only link fails")
	}
}

func TestAddReleaseNoLink(t *testing.T) {
	_, err := AddRelease(context.Background(), &fakeFetcher{}, &fakeTrans{}, prowlarr.Release{})
	if err == nil {
		t.Fatal("AddRelease: want error for a linkless release, got nil")
	}
}
