package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const gofileOK = `{"status":"ok","data":{"downloadPage":"https://gofile.io/d/abc","guestToken":"guest-1"}}`

type gofileCall struct {
	method        string
	path          string
	auth          string
	fileName      string
	content       []byte
	contentLength int64
	chunked       bool
}

// fakeGofile records what the uploader sends and answers with a canned reply.
type fakeGofile struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []gofileCall
	handler func(gofileCall) (status int, body string)
}

func newFakeGofile(t *testing.T) *fakeGofile {
	t.Helper()
	f := &fakeGofile{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := gofileCall{
			method:        r.Method,
			path:          r.URL.Path,
			auth:          r.Header.Get("Authorization"),
			contentLength: r.ContentLength,
			chunked:       len(r.TransferEncoding) > 0,
		}
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			if headers := r.MultipartForm.File["file"]; len(headers) == 1 {
				call.fileName = headers[0].Filename
				if fh, err := headers[0].Open(); err == nil {
					call.content, _ = io.ReadAll(fh)
					fh.Close()
				}
			}
		}

		f.mu.Lock()
		f.calls = append(f.calls, call)
		handler := f.handler
		f.mu.Unlock()

		status, body := http.StatusOK, gofileOK
		if handler != nil {
			status, body = handler(call)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGofile) setHandler(h func(gofileCall) (int, string)) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fakeGofile) snapshot() []gofileCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gofileCall(nil), f.calls...)
}

func uploaderFor(f *fakeGofile, token, tokenFile string) *gofileUploader {
	g := newGofileUploader(token, tokenFile)
	g.endpoint = f.srv.URL + "/uploadfile"
	return g
}

func tempUpload(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload.zip")
	writeSized(t, p, size)
	return p
}

func TestGofileSendsTheFileWithAKnownLength(t *testing.T) {
	f := newFakeGofile(t)
	g := uploaderFor(f, "", filepath.Join(t.TempDir(), "token.txt"))
	path := tempUpload(t, 100000)

	// The callback runs on the HTTP transport's goroutine.
	var mu sync.Mutex
	var lastSent, lastTotal int64
	link, err := g.Upload(context.Background(), path, "My Album.zip", func(sent, total int64) {
		mu.Lock()
		lastSent, lastTotal = sent, total
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if link != "https://gofile.io/d/abc" {
		t.Fatalf("link = %q", link)
	}

	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d requests, want 1", len(calls))
	}
	c := calls[0]
	if c.method != http.MethodPost || c.path != "/uploadfile" {
		t.Errorf("request was %s %s", c.method, c.path)
	}
	if c.fileName != "My Album.zip" || len(c.content) != 100000 {
		t.Errorf("file part: name %q, %d bytes", c.fileName, len(c.content))
	}
	if c.chunked || c.contentLength <= 100000 {
		t.Errorf("the request needs a real Content-Length (got %d, chunked=%v)", c.contentLength, c.chunked)
	}
	if c.auth != "" {
		t.Errorf("a guest upload must not send credentials, got %q", c.auth)
	}
	if lastSent != 100000 || lastTotal != 100000 {
		t.Errorf("progress ended at %d of %d, want 100000 of 100000", lastSent, lastTotal)
	}
}

func TestGofileKeepsAndReusesTheGuestToken(t *testing.T) {
	f := newFakeGofile(t)
	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	g := uploaderFor(f, "", tokenFile)
	path := tempUpload(t, 10)

	if _, err := g.Upload(context.Background(), path, "a.zip", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(data)) != "guest-1" {
		t.Fatalf("the guest token should be saved, file = %q (%v)", data, err)
	}

	if _, err := g.Upload(context.Background(), path, "b.zip", nil); err != nil {
		t.Fatal(err)
	}
	// A new process starts from the saved token as well.
	if _, err := uploaderFor(f, "", tokenFile).Upload(context.Background(), path, "c.zip", nil); err != nil {
		t.Fatal(err)
	}

	calls := f.snapshot()
	if len(calls) != 3 {
		t.Fatalf("got %d requests, want 3", len(calls))
	}
	if calls[0].auth != "" || calls[1].auth != "Bearer guest-1" || calls[2].auth != "Bearer guest-1" {
		t.Fatalf("auth headers = %q, %q, %q; want none, then the saved guest token twice",
			calls[0].auth, calls[1].auth, calls[2].auth)
	}
}

func TestGofileUsesAConfiguredTokenAndNeverReplacesIt(t *testing.T) {
	f := newFakeGofile(t)
	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	g := uploaderFor(f, "mine", tokenFile)

	if _, err := g.Upload(context.Background(), tempUpload(t, 10), "a.zip", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshot()[0].auth; got != "Bearer mine" {
		t.Fatalf("auth = %q, want the configured token", got)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatal("a configured token must not be overwritten by a guest token file")
	}
}

func TestGofileRetriesOnceAsAGuestWhenTheSavedTokenIsRejected(t *testing.T) {
	f := newFakeGofile(t)
	f.setHandler(func(c gofileCall) (int, string) {
		if c.auth == "Bearer stale" {
			return http.StatusUnauthorized, `{"status":"error-token","error":{"code":"error-token","message":"invalid token"}}`
		}
		return http.StatusOK, `{"status":"ok","data":{"downloadPage":"https://gofile.io/d/new","guestToken":"fresh"}}`
	})
	tokenFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenFile, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := uploaderFor(f, "", tokenFile)

	link, err := g.Upload(context.Background(), tempUpload(t, 10), "a.zip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://gofile.io/d/new" {
		t.Fatalf("link = %q", link)
	}
	calls := f.snapshot()
	if len(calls) != 2 || calls[0].auth != "Bearer stale" || calls[1].auth != "" {
		t.Fatalf("expected the stale token then a guest retry, got %+v", calls)
	}
	if data, _ := os.ReadFile(tokenFile); strings.TrimSpace(string(data)) != "fresh" {
		t.Fatalf("the new guest token should replace the stale one, file = %q", data)
	}
}

func TestGofileDoesNotRetryWithAConfiguredToken(t *testing.T) {
	f := newFakeGofile(t)
	f.setHandler(func(gofileCall) (int, string) {
		return http.StatusUnauthorized, `{"status":"error-token","error":{"code":"error-token","message":"invalid token"}}`
	})
	g := uploaderFor(f, "mine", filepath.Join(t.TempDir(), "token.txt"))

	_, err := g.Upload(context.Background(), tempUpload(t, 10), "a.zip", nil)
	if err == nil {
		t.Fatal("a rejected configured token must be an error")
	}
	if len(f.snapshot()) != 1 {
		t.Fatalf("got %d requests, want exactly 1", len(f.snapshot()))
	}
	if !strings.Contains(err.Error(), "gofile-token") {
		t.Fatalf("the error should point at the setting to fix, got: %v", err)
	}
}

func TestGofileReportsFailuresReadably(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"status string", 200, `{"status":"error-quota","data":{}}`, "error-quota"},
		{"error object", 200, `{"status":"error","error":{"code":"x","message":"disk is full"}}`, "disk is full"},
		{"not json", 500, "<html>Bad gateway</html>", "HTTP 500"},
		{"ok without a page", 200, `{"status":"ok","data":{}}`, "no download page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGofile(t)
			f.setHandler(func(gofileCall) (int, string) { return tc.status, tc.body })
			g := uploaderFor(f, "", filepath.Join(t.TempDir(), "token.txt"))

			_, err := g.Upload(context.Background(), tempUpload(t, 10), "a.zip", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestGofileUploadStopsWhenCancelled(t *testing.T) {
	// A server that never answers: only the context can end the upload.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	g := newGofileUploader("", "")
	g.endpoint = srv.URL + "/uploadfile"

	path := tempUpload(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := g.Upload(ctx, path, "a.zip", nil)
		result <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a cancelled upload must return an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling did not stop the upload")
	}
}

func TestNewUploaderFollowsTheConfig(t *testing.T) {
	if up := newUploader(BotConfig{LargeFileHost: "gofile"}); up == nil || up.Name() != "Gofile" {
		t.Fatalf("gofile should select the Gofile uploader, got %v", up)
	}
	for _, host := range []string{"", "none"} {
		if up := newUploader(BotConfig{LargeFileHost: host}); up != nil {
			t.Fatalf("host %q should disable uploading, got %v", host, up)
		}
	}
}
