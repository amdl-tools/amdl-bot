package bot

// gofile.go uploads to Gofile (https://gofile.io).
//
// API as documented at https://gofile.io/api and by third-party clients:
//
//	POST https://upload.gofile.io/uploadfile      multipart: file (required), folderId (optional)
//	Authorization: Bearer <token>                 optional
//
// Without a token Gofile creates a guest account and returns its guestToken in
// the reply; that token is kept and reused, because the documentation asks
// clients not to create a new account per upload. The reply carries
// {"status":"ok","data":{"downloadPage":"https://gofile.io/d/..."}}.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
)

const gofileUploadURL = "https://upload.gofile.io/uploadfile"

type gofileUploader struct {
	endpoint  string // upload URL; a field so tests can point it elsewhere
	client    *http.Client
	tokenFile string // where a guest token is kept between restarts

	mu    sync.Mutex
	token string
	fixed bool // the token comes from the config and is never replaced or forgotten
}

// newGofileUploader returns an uploader that uses token when given, and
// otherwise a guest token remembered in tokenFile.
func newGofileUploader(token, tokenFile string) *gofileUploader {
	g := &gofileUploader{
		endpoint:  gofileUploadURL,
		client:    &http.Client{}, // no overall timeout: uploads of gigabytes take a long time
		tokenFile: tokenFile,
		token:     strings.TrimSpace(token),
	}
	if g.token != "" {
		g.fixed = true
	} else if tokenFile != "" {
		if data, err := os.ReadFile(tokenFile); err == nil {
			g.token = strings.TrimSpace(string(data))
		}
	}
	return g
}

func (g *gofileUploader) Name() string { return "Gofile" }

func (g *gofileUploader) currentToken() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.token
}

func (g *gofileUploader) rememberGuestToken(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fixed || token == "" || g.token == token {
		return
	}
	g.token = token
	if g.tokenFile != "" {
		_ = writeFileAtomic(g.tokenFile, []byte(token), 0o600)
	}
}

func (g *gofileUploader) forgetGuestToken() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fixed {
		return
	}
	g.token = ""
	if g.tokenFile != "" {
		_ = os.Remove(g.tokenFile)
	}
}

// Upload sends the file and returns its download page. A remembered guest token
// that Gofile rejects is dropped and the upload retried once as a new guest.
func (g *gofileUploader) Upload(ctx context.Context, path, name string, progress func(sent, total int64)) (string, error) {
	token := g.currentToken()
	link, status, err := g.uploadOnce(ctx, path, name, token, progress)
	if err != nil && token != "" && !g.fixed && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		g.forgetGuestToken()
		link, _, err = g.uploadOnce(ctx, path, name, "", progress)
	}
	return link, err
}

// uploadOnce performs one upload request. It also returns the HTTP status so the
// caller can tell an authentication failure from other errors.
func (g *gofileUploader) uploadOnce(ctx context.Context, path, name, token string, progress func(sent, total int64)) (link string, status int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}

	// Build the multipart framing by hand around the streamed file, so the request
	// has a known Content-Length (no chunked encoding) and the file is never held
	// in memory.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if _, err := mw.CreateFormFile("file", name); err != nil {
		return "", 0, err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"

	body := io.MultiReader(
		bytes.NewReader(head.Bytes()),
		&uploadReader{ctx: ctx, r: file, total: info.Size(), fn: progress},
		strings.NewReader(tail),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, body)
	if err != nil {
		return "", 0, err
	}
	req.ContentLength = int64(head.Len()) + info.Size() + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	link, guestToken, err := parseGofileReply(raw, resp.StatusCode)
	if err != nil {
		return "", resp.StatusCode, err
	}
	if token == "" {
		g.rememberGuestToken(guestToken)
	}
	return link, resp.StatusCode, nil
}

// parseGofileReply extracts the download page (and any guest token) from a reply
// or turns the failure into a readable error.
func parseGofileReply(raw []byte, httpStatus int) (link, guestToken string, err error) {
	var reply struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal(raw, &reply); jsonErr != nil {
		return "", "", fmt.Errorf("unexpected reply from Gofile (HTTP %d): %s", httpStatus, snippet(raw))
	}

	if reply.Status != "ok" {
		detail := reply.Error.Message
		if detail == "" {
			detail = reply.Error.Code
		}
		msg := fmt.Sprintf("Gofile refused the upload (HTTP %d, status %q", httpStatus, reply.Status)
		if detail != "" {
			msg += ": " + detail
		}
		msg += ")"
		if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
			msg += "; set telegram.gofile-token to a token from your Gofile profile"
		}
		return "", "", errors.New(msg)
	}

	var data struct {
		DownloadPage string `json:"downloadPage"`
		GuestToken   string `json:"guestToken"`
	}
	if err := json.Unmarshal(reply.Data, &data); err != nil || data.DownloadPage == "" {
		return "", "", fmt.Errorf("Gofile accepted the upload but returned no download page: %s", snippet(raw))
	}
	return data.DownloadPage, data.GuestToken, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// uploadReader reports progress and stops as soon as the context ends.
type uploadReader struct {
	ctx   context.Context
	r     io.Reader
	sent  int64
	total int64
	fn    func(sent, total int64)
}

func (u *uploadReader) Read(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := u.r.Read(p)
	u.sent += int64(n)
	if n > 0 && u.fn != nil {
		u.fn(u.sent, u.total)
	}
	return n, err
}

// newUploader returns the uploader selected by telegram.large-file-host, or nil
// when large files are not uploaded anywhere.
func newUploader(cfg BotConfig) Uploader {
	switch cfg.LargeFileHost {
	case "gofile":
		return newGofileUploader(cfg.GofileToken, cfg.GofileTokenFile)
	}
	return nil
}
