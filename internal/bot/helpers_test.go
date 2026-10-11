package bot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	songURL = "https://music.apple.com/us/song/some-song/1234567890"

	getMeJSON = `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"testbot"}}`

	// A message that carries both an audio and a document, so the same reply
	// works for sendAudio and sendDocument.
	okMessageJSON = `{"ok":true,"result":{"message_id":7,"date":1,"chat":{"id":1,"type":"private"},` +
		`"audio":{"file_id":"NEWFID","file_unique_id":"u","duration":1},` +
		`"document":{"file_id":"NEWFID","file_unique_id":"u"}}}`

	staleFileIDJSON = `{"ok":false,"error_code":400,"description":"Bad Request: wrong file identifier/HTTP URL specified","parameters":{}}`
)

// tgCall is one request received by the fake Telegram server.
type tgCall struct {
	Method string
	Form   url.Values
	Files  []string // names of multipart file parts; empty for plain (file_id) sends
}

// fakeTG stands in for api.telegram.org. It records every call and answers with a
// canned success reply unless a test overrides the answer for some call.
type fakeTG struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []tgCall
	override func(tgCall) (status int, body string, handled bool)
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	call := tgCall{Method: parts[len(parts)-1]}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(64 << 20)
		if r.MultipartForm != nil {
			for name := range r.MultipartForm.File {
				call.Files = append(call.Files, name)
			}
		}
		sort.Strings(call.Files)
	} else {
		_ = r.ParseForm()
	}
	call.Form = r.Form

	f.mu.Lock()
	f.calls = append(f.calls, call)
	override := f.override
	f.mu.Unlock()

	status, body := http.StatusOK, okMessageJSON
	if call.Method == "getMe" {
		body = getMeJSON
	}
	if override != nil {
		if s, b, handled := override(call); handled {
			status, body = s, b
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeTG) setOverride(fn func(tgCall) (int, string, bool)) {
	f.mu.Lock()
	f.override = fn
	f.mu.Unlock()
}

func (f *fakeTG) callsTo(method string) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// newTestBot builds a Bot wired to the fake Telegram server, without starting
// the queue, monitor or cleaner goroutines that New() would start.
func newTestBot(t *testing.T, f *fakeTG, mutate ...func(*BotConfig)) *Bot {
	t.Helper()
	api, err := tgbotapi.NewBotAPIWithAPIEndpoint("TOKEN", f.srv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("connecting to the fake Telegram server: %v", err)
	}
	cfg := BotConfig{SendAsDocument: true, MaxFileSizeMB: 50, DeleteMode: "never"}
	for _, m := range mutate {
		m(&cfg)
	}
	return &Bot{
		api:         api,
		cfg:         cfg,
		cleaner:     NewCleaner(cfg),
		stats:       NewStats(""),
		fileIDCache: NewFileIDCache(filepath.Join(t.TempDir(), "cache.json")),
		sessions:    NewSessionStore(time.Minute),
		locks:       newKeyedLock(),
	}
}

// stubPause replaces the package-level pause so tests do not wait for real.
func stubPause(t *testing.T, fn func(time.Duration)) {
	t.Helper()
	orig := pause
	pause = fn
	t.Cleanup(func() { pause = orig })
}

// waitFor polls cond until it returns true or two seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 2*time.Second, what, cond)
}

// waitForWithin is waitFor with a caller-chosen deadline, for steps that start a process.
func waitForWithin(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
