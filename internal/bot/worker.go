package bot

// worker.go runs every download in a process of its own.
//
// Why a process and not a goroutine: the downloader prints with fmt.Print* from
// hundreds of places and keeps state in package-level variables (the HTTP client,
// for one), so two downloads cannot share a process; and it has no way to be
// interrupted, because its context.Background() calls are buried deep in the
// code. A separate process fixes both: downloads run truly in parallel, and
// /cancel kills the process (and the ffmpeg it started) outright.
//
// The bot starts itself again as "amdl-bot -worker -config <file>". The request
// goes in on stdin as JSON, progress comes back as text lines on stdout/stderr,
// and the result is written as JSON to a file named in the request.

import (
	"bufio"
	"bytes"
	"amdl/internal/app"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// workerRequest is what the bot sends to a download process on stdin.
type workerRequest struct {
	URL        string `json:"url"`
	Mode       Mode   `json:"mode"`
	Token      string `json:"token,omitempty"`       // Apple Music token from the cache lookup
	SkipTracks []int  `json:"skip_tracks,omitempty"` // positions already served from cache
	ResultPath string `json:"result_path"`
}

// workerResult is what a download process writes to ResultPath when it finishes.
type workerResult struct {
	AddedTracks []app.AddedTrack `json:"added_tracks"`
	Error       string           `json:"error,omitempty"`
}

// maxLogLines is how many trailing lines of a download's output are kept for
// error messages.
const maxLogLines = 200

// RunWorker is the entry point of a download process. It returns the process
// exit code: 0 once a result was written (even when the download itself failed,
// which is reported inside the result), 2 when the request could not be handled.
func RunWorker(configPath string, stdin io.Reader) int {
	var req workerRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "worker: unreadable request:", err)
		return 2
	}
	cfg, err := LoadBotConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: config:", err)
		return 2
	}

	res := runDownload(RunnerConfig{
		AppConfig:  cfg.Bot,
		Token:      req.Token,
		SkipTracks: req.SkipTracks,
	}, req.URL, req.Mode)

	out := workerResult{AddedTracks: res.AddedTracks}
	if res.Err != nil {
		out.Error = res.Err.Error()
	}
	data, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: encoding result:", err)
		return 2
	}
	if err := os.WriteFile(req.ResultPath, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "worker: writing result:", err)
		return 2
	}
	return 0
}

// workerCommand builds the command that starts a download process. It is a
// variable so tests can start a stand-in instead.
var workerCommand = func(configPath string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, "-worker", "-config", configPath), nil
}

// runDownloadProcess runs one download in a separate process and returns its
// result. Every line the process prints is passed to onProgress. When ctx ends
// the process, and everything it started, is killed and the result carries
// context.Canceled.
func runDownloadProcess(ctx context.Context, configPath string, req workerRequest, onProgress func(string)) DownloadResult {
	var res DownloadResult
	if err := ctx.Err(); err != nil {
		res.Err = err
		return res
	}

	dir, err := os.MkdirTemp("", "amdl-job-*")
	if err != nil {
		res.Err = fmt.Errorf("creating job directory: %w", err)
		return res
	}
	defer os.RemoveAll(dir)
	req.ResultPath = filepath.Join(dir, "result.json")

	cmd, err := workerCommand(configPath)
	if err != nil {
		res.Err = fmt.Errorf("locating the download program: %w", err)
		return res
	}
	setProcessGroup(cmd)

	// stdout and stderr go through one pipe so their lines stay in order.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.WaitDelay = 3 * time.Second // do not hang on pipes a leftover child still holds
	stdin, err := cmd.StdinPipe()
	if err != nil {
		res.Err = fmt.Errorf("preparing the download process: %w", err)
		return res
	}

	if err := cmd.Start(); err != nil {
		pw.Close()
		res.Err = fmt.Errorf("starting the download process: %w", err)
		return res
	}

	logged := make(chan []string, 1)
	go func() { logged <- readLines(pr, onProgress) }()

	_ = json.NewEncoder(stdin).Encode(req)
	stdin.Close()

	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(cmd)
		case <-exited:
		}
	}()

	waitErr := cmd.Wait()
	close(exited)
	pw.Close()
	res.Log = strings.Join(<-logged, "\n")

	if ctx.Err() != nil {
		res.Err = context.Canceled
		return res
	}

	data, readErr := os.ReadFile(req.ResultPath)
	if readErr != nil {
		res.Err = fmt.Errorf("the download process ended without a result (%v)", waitErr)
		return res
	}
	var wr workerResult
	if err := json.Unmarshal(data, &wr); err != nil {
		res.Err = fmt.Errorf("unreadable result from the download process: %w", err)
		return res
	}
	res.AddedTracks = wr.AddedTracks
	if wr.Error != "" {
		res.Err = errors.New(wr.Error)
	}
	return res
}

// splitFrames splits on "\n" and on "\r", keeping the terminator on each token.
// The downloader redraws its progress bar by writing "\r" and only ends the line
// with "\n" when the bar is complete, so splitting on "\n" alone would show
// nothing until a track had finished downloading.
func splitFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// readLines passes every non-empty line or progress frame of r to onLine, with
// colour codes removed, and returns the last maxLogLines finished lines. A frame
// that a progress bar will redraw (ended by "\r") is progress, not log output,
// and is left out of the returned tail, unless a "\n" follows it at once: that is
// a "\r\n" line ending, so the frame was a finished line after all.
func readLines(r io.Reader, onLine func(string)) []string {
	var tail []string
	appendTail := func(line string) {
		tail = append(tail, line)
		if len(tail) > maxLogLines {
			tail = tail[len(tail)-maxLogLines:]
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(splitFrames)

	lastFrame := "" // the most recent "\r"-ended frame, until the next token shows what it was
	for sc.Scan() {
		tok := sc.Text()
		endsWithCR := strings.HasSuffix(tok, "\r")
		line := strings.TrimSpace(stripANSI(strings.TrimRight(tok, "\r\n")))

		if line == "" {
			if strings.HasSuffix(tok, "\n") && lastFrame != "" {
				appendTail(lastFrame) // "\r" then "\n": a Windows line ending
			}
			lastFrame = ""
			continue
		}
		if endsWithCR {
			lastFrame = line
		} else {
			lastFrame = ""
			appendTail(line)
		}
		if onLine != nil {
			onLine(line)
		}
	}
	// If the scanner gave up on an over-long line, keep draining so the process
	// never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
	return tail
}
