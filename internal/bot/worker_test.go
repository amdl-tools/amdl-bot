package bot

import (
	"amdl/internal/app"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperWorker is not a real test. When AMDL_TEST_WORKER is set, the test
// binary has been started by useHelperWorker as a stand-in for a download
// process; it plays the scenario named by the variable and exits.
func TestHelperWorker(t *testing.T) {
	scenario := os.Getenv("AMDL_TEST_WORKER")
	if scenario == "" {
		return // an ordinary test run
	}

	var req workerRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "helper: unreadable request:", err)
		os.Exit(2)
	}
	writeResult := func(r workerResult) {
		data, _ := json.Marshal(r)
		if err := os.WriteFile(req.ResultPath, data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "helper: writing result:", err)
			os.Exit(2)
		}
	}

	switch scenario {
	case "success":
		fmt.Println("Track 1 of 2: ALAC")
		fmt.Fprintln(os.Stderr, "Track 2 of 2: ALAC") // stderr must be captured as well
		// The request is echoed back through the track fields so the test can check it arrived intact.
		writeResult(workerResult{AddedTracks: []app.AddedTrack{{
			Path:      "x.m4a",
			TrackID:   "42",
			Artist:    string(req.Mode),
			Album:     fmt.Sprint(req.SkipTracks),
			Song:      req.URL,
			CoverPath: req.Token,
		}}})
	case "error":
		fmt.Println("something went wrong")
		writeResult(workerResult{Error: "boom"})
	case "partial":
		// Two tracks came down before the download failed.
		fmt.Println("Track 2 of 5: ALAC")
		writeResult(workerResult{
			AddedTracks: []app.AddedTrack{{Path: "a.m4a", TrackID: "1"}, {Path: "b.m4a", TrackID: "2"}},
			Error:       "boom",
		})
	case "crash":
		fmt.Println("panic: something")
		os.Exit(3)
	case "hang":
		fmt.Println("Track 1 of 2: ALAC")
		time.Sleep(time.Hour)
	case "frames":
		// Like the downloader's own progress bar: frames separated by a carriage
		// return, no newline until the bar is complete (which never happens here).
		fmt.Println("Track 1 of 2: songs")
		fmt.Print("Decrypting... 30% (3 MB/10 MB, 1.5 MB/s)\r")
		fmt.Print("Decrypting... 60% (6 MB/10 MB, 2.5 MB/s)\r")
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// useHelperWorker makes runDownloadProcess start this test binary in the given
// scenario instead of the real bot.
func useHelperWorker(t *testing.T, scenario string) {
	t.Helper()
	orig := workerCommand
	workerCommand = func(configPath string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWorker$")
		cmd.Env = append(os.Environ(), "AMDL_TEST_WORKER="+scenario)
		return cmd, nil
	}
	t.Cleanup(func() { workerCommand = orig })
}

func TestRunDownloadProcessReturnsTheWorkersResult(t *testing.T) {
	useHelperWorker(t, "success")

	var mu sync.Mutex
	var lines []string
	res := runDownloadProcess(context.Background(), "unused.yaml",
		workerRequest{URL: "https://example.test/x", Mode: ModeAtmos, Token: "tok", SkipTracks: []int{1, 3}},
		func(line string) {
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		})

	if res.Err != nil {
		t.Fatalf("unexpected error: %v\nlog:\n%s", res.Err, res.Log)
	}
	if len(res.AddedTracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(res.AddedTracks))
	}
	tr := res.AddedTracks[0]
	if tr.TrackID != "42" || tr.Artist != "atmos" || tr.Album != "[1 3]" ||
		tr.Song != "https://example.test/x" || tr.CoverPath != "tok" {
		t.Fatalf("the request did not reach the worker intact: %+v", tr)
	}

	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "Track 1 of 2") || !strings.Contains(joined, "Track 2 of 2") {
		t.Fatalf("progress lines from stdout and stderr should both arrive, got:\n%s", joined)
	}
	if !strings.Contains(res.Log, "Track 2 of 2") {
		t.Fatalf("the log should keep the output, got %q", res.Log)
	}
}

func TestRunDownloadProcessReportsTheWorkersError(t *testing.T) {
	useHelperWorker(t, "error")

	res := runDownloadProcess(context.Background(), "unused.yaml", workerRequest{URL: "u", Mode: ModeALAC}, nil)

	if res.Err == nil || res.Err.Error() != "boom" {
		t.Fatalf("got error %v, want boom", res.Err)
	}
	if !strings.Contains(res.Log, "something went wrong") {
		t.Fatalf("log = %q", res.Log)
	}
}

func TestRunDownloadProcessReportsACrash(t *testing.T) {
	useHelperWorker(t, "crash")

	res := runDownloadProcess(context.Background(), "unused.yaml", workerRequest{URL: "u", Mode: ModeALAC}, nil)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "without a result") {
		t.Fatalf("a process that dies without a result should say so, got %v", res.Err)
	}
	if !strings.Contains(res.Log, "panic: something") {
		t.Fatalf("the crash output should be kept for the error message, log = %q", res.Log)
	}
}

// This is the point of the whole change: cancelling must stop the work, not just
// change a label.
func TestRunDownloadProcessKillsTheProcessOnCancel(t *testing.T) {
	useHelperWorker(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var once sync.Once
	finished := make(chan DownloadResult, 1)
	go func() {
		finished <- runDownloadProcess(ctx, "unused.yaml", workerRequest{URL: "u", Mode: ModeALAC},
			func(string) { once.Do(func() { close(started) }) })
	}()

	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the download process never reported progress")
	}
	cancel()

	select {
	case res := <-finished:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("got error %v, want context.Canceled", res.Err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelling did not stop the download process")
	}
}

func TestRunDownloadProcessDoesNotStartWhenAlreadyCancelled(t *testing.T) {
	orig := workerCommand
	calls := 0
	workerCommand = func(string) (*exec.Cmd, error) {
		calls++
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { workerCommand = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := runDownloadProcess(ctx, "unused.yaml", workerRequest{URL: "u", Mode: ModeALAC}, nil)

	if calls != 0 {
		t.Fatal("no process should be started for a job that is already cancelled")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", res.Err)
	}
}

func TestRunDownloadProcessReportsAProcessThatCannotStart(t *testing.T) {
	orig := workerCommand
	workerCommand = func(string) (*exec.Cmd, error) {
		return exec.Command(filepath.Join(t.TempDir(), "no-such-program")), nil
	}
	t.Cleanup(func() { workerCommand = orig })

	res := runDownloadProcess(context.Background(), "unused.yaml", workerRequest{URL: "u", Mode: ModeALAC}, nil)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "starting the download process") {
		t.Fatalf("got %v", res.Err)
	}
}

// Cancelling a running job through the bot ends in "Cancelled." and uploads nothing.
func TestProcessJobCancelKillsTheDownloadAndSendsNothing(t *testing.T) {
	useHelperWorker(t, "hang")
	f := newFakeTG(t)
	b := newTestBot(t, f)
	job := newJob(1, 100, 5, songURL, ModeALAC, "alice")

	finished := make(chan struct{})
	go func() {
		b.processJob(job)
		close(finished)
	}()

	waitForWithin(t, 20*time.Second, "the download to report progress", func() bool {
		for _, c := range f.callsTo("editMessageText") {
			if strings.Contains(c.Form.Get("text"), "Track 1 of 2") {
				return true
			}
		}
		return false
	})
	job.cancel() // what /cancel does through the queue

	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the job did not stop after being cancelled")
	}

	edits := f.callsTo("editMessageText")
	if last := edits[len(edits)-1].Form.Get("text"); last != "Cancelled." {
		t.Fatalf("final message = %q, want %q", last, "Cancelled.")
	}
	if len(f.callsTo("sendAudio")) != 0 || len(f.callsTo("sendDocument")) != 0 {
		t.Fatal("a cancelled job must not send anything")
	}
}

// ---- the worker side ---------------------------------------------------------

func TestRunWorkerRejectsAnUnreadableRequest(t *testing.T) {
	if got := RunWorker("unused.yaml", strings.NewReader("not json")); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}

func TestRunWorkerRejectsAMissingConfig(t *testing.T) {
	req := `{"url":"u","mode":"alac","result_path":"r.json"}`
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if got := RunWorker(missing, strings.NewReader(req)); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
}

// A link the downloader does not recognise fails without touching the network,
// which makes it a safe way to exercise the worker end to end.
func TestRunWorkerWritesTheResultFile(t *testing.T) {
	cfgPath := writeConfig(t, "telegram:\n  token: \"abc\"\n")
	resultPath := filepath.Join(t.TempDir(), "result.json")
	req, err := json.Marshal(workerRequest{
		URL: "https://example.test/not-apple-music", Mode: ModeALAC, Token: "tok", ResultPath: resultPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := RunWorker(cfgPath, strings.NewReader(string(req))); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("no result file: %v", err)
	}
	var wr workerResult
	if err := json.Unmarshal(data, &wr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wr.Error, "unrecognised Apple Music URL") {
		t.Fatalf("result error = %q", wr.Error)
	}
}

func TestReadLinesKeepsOnlyTheTailOfFinishedLines(t *testing.T) {
	var in strings.Builder
	for i := 0; i < maxLogLines+50; i++ {
		fmt.Fprintf(&in, "line %d\n", i)
	}
	in.WriteString("\n   \n")          // blank lines are dropped
	in.WriteString("10%\r50%\r100%\n") // a redrawn bar: only its finished last frame is log output
	var seen []string
	tail := readLines(strings.NewReader(in.String()), func(l string) { seen = append(seen, l) })

	if len(tail) != maxLogLines {
		t.Fatalf("tail has %d lines, want %d", len(tail), maxLogLines)
	}
	if tail[len(tail)-1] != "100%" {
		t.Fatalf("last line = %q, want the final progress frame", tail[len(tail)-1])
	}
	if tail[0] != fmt.Sprintf("line %d", 51) {
		t.Fatalf("first kept line = %q", tail[0])
	}
	// Every non-empty line and frame reaches the callback: the 250 lines, the two
	// redrawn frames "10%" and "50%", and the finished "100%".
	if len(seen) != maxLogLines+53 {
		t.Fatalf("callback saw %d lines, want %d", len(seen), maxLogLines+53)
	}
}

// The downloader's progress bar ends no line until it is complete, so a reader
// that waits for a newline shows nothing for a whole track. The frames must reach
// the message while the track is still downloading.
func TestProcessJobShowsLiveProgressFromTheDownloadersBar(t *testing.T) {
	useHelperWorker(t, "frames")
	f := newFakeTG(t)
	b := newTestBot(t, f)
	job := newJob(1, 100, 5, songURL, ModeALAC, "alice")

	finished := make(chan struct{})
	go func() {
		b.processJob(job)
		close(finished)
	}()

	waitForWithin(t, 20*time.Second, "the bar's frames to appear in the message", func() bool {
		for _, c := range f.callsTo("editMessageText") {
			text := c.Form.Get("text")
			if strings.Contains(text, "Decrypting 60% at 2.5 MB/s") && strings.Contains(text, "Track 1 of 2") {
				return true
			}
		}
		return false
	})

	job.cancel()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the job did not stop")
	}
}
