package tgbot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// fakeTelegram is an httptest server speaking just enough of the Telegram
// Bot API (multipart requests, JSON envelope responses) for offline tests.
// Every served call is also signaled on hits so tests can wait for the bot's
// loops to demonstrably run instead of sleeping.
type fakeTelegram struct {
	*httptest.Server
	mu    sync.Mutex
	calls []fakeCall
	hits  chan string // method name of every served call
}

type fakeCall struct {
	method string // e.g. "sendMessage"
	form   map[string]string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{hits: make(chan string, 64)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		form := map[string]string{}
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			for k, vs := range r.MultipartForm.Value {
				if len(vs) > 0 {
					form[k] = vs[0]
				}
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{method: method, form: form})
		f.mu.Unlock()
		select {
		case f.hits <- method:
		default:
		}

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage":
			w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":42}}}`))
		case "getUpdates":
			w.Write([]byte(`{"ok":true,"result":[]}`))
		default: // setMyCommands, deleteWebhook, ...
			w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// awaitCall blocks until the server has served the given method, proving the
// bot's loops are actually running.
func (f *fakeTelegram) awaitCall(t *testing.T, method string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-f.hits:
			if m == method {
				return
			}
		case <-deadline:
			t.Fatalf("no %s call reached the fake server within 5s", method)
		}
	}
}

func (f *fakeTelegram) callsFor(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func newOfflineBot(t *testing.T, srv *fakeTelegram) *Bot {
	t.Helper()
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	b, err := New("123:testtoken", []int64{testChatID}, h,
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestBotNotifySendsToAllowedChat(t *testing.T) {
	srv := newFakeTelegram(t)
	b := newOfflineBot(t, srv)

	if err := b.Notify(context.Background(), "✅ Finished:\ntest"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	sent := srv.callsFor("sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sent))
	}
	if got := sent[0].form["chat_id"]; got != "42" {
		t.Errorf("chat_id = %q, want 42", got)
	}
	if got := sent[0].form["text"]; got != "✅ Finished:\ntest" {
		t.Errorf("text = %q", got)
	}
}

func TestBotNotifyErrorSurfaces(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer failing.Close()

	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	b, err := New("123:testtoken", []int64{testChatID}, h,
		bot.WithSkipGetMe(), bot.WithServerURL(failing.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Notify(context.Background(), "hi"); err == nil {
		t.Fatal("Notify: want error when Telegram rejects the message, got nil")
	}
}

func TestBotNotifyFansOutToEveryAllowedChat(t *testing.T) {
	// Subscriptions belong to the install, not to whoever created them, so a
	// completed download is announced in every allowed chat.
	srv := newFakeTelegram(t)
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	b, err := New("123:testtoken", []int64{42, -1001234567890}, h,
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Notify(context.Background(), "hi"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	sent := srv.callsFor("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", len(sent))
	}
	got := []string{sent[0].form["chat_id"], sent[1].form["chat_id"]}
	if want := []string{"42", "-1001234567890"}; !slices.Equal(got, want) {
		t.Errorf("chat_ids = %v, want %v", got, want)
	}
}

func TestBotNotifyPartialFailureStillDelivers(t *testing.T) {
	// One unreachable chat — bot blocked, group deleted — must not silence the
	// others, and must not be reported as a failed notification.
	// The failure is keyed on the chat, not on call order, so the test keeps
	// meaning if the sends are ever reordered.
	var mu sync.Mutex
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chat := ""
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			if vs := r.MultipartForm.Value["chat_id"]; len(vs) > 0 {
				chat = vs[0]
			}
		}
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			mu.Lock()
			tried = append(tried, chat)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if chat == "42" {
			w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":7}}}`))
	}))
	defer srv.Close()

	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	b, err := New("123:testtoken", []int64{42, 7}, h,
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Notify(context.Background(), "hi"); err != nil {
		t.Errorf("Notify: want nil when one of two chats succeeded, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(tried)
	if want := []string{"42", "7"}; !slices.Equal(tried, want) {
		t.Errorf("chats attempted = %v, want %v (a failed chat must not stop the rest)", tried, want)
	}
}

func TestBotRunExitsOnCancel(t *testing.T) {
	srv := newFakeTelegram(t)
	b := newOfflineBot(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.Run(ctx)
		close(done)
	}()

	// Run registers the menu before polling, so a getUpdates call proves both
	// have reached the server.
	srv.awaitCall(t, "getUpdates")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}

	if len(srv.callsFor("setMyCommands")) == 0 {
		t.Error("Run should register the command menu via setMyCommands")
	}
}
