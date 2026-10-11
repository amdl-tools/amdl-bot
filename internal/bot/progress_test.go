package bot

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// text returns the current message text.
func (p *Progress) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.render()
}

func withProgressInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := progressInterval
	progressInterval = d
	t.Cleanup(func() { progressInterval = old })
}

type editLog struct {
	mu    sync.Mutex
	texts []string
}

func (e *editLog) add(s string) {
	e.mu.Lock()
	e.texts = append(e.texts, s)
	e.mu.Unlock()
}

func (e *editLog) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.texts...)
}

func (e *editLog) anyContains(sub string) bool {
	for _, t := range e.all() {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q in:\n%s", w, text)
		}
	}
}

// ---- reading what the downloader prints -------------------------------------------

func TestProgressStartsIndeterminate(t *testing.T) {
	text := NewProgress(ModeALAC, nil).text()
	mustContain(t, text, "Downloading [alac]", "working...")
	if strings.Contains(text, "Track") {
		t.Fatalf("no track has started yet:\n%s", text)
	}
}

func TestProgressShowsTheTrackAndItsProgress(t *testing.T) {
	p := NewProgress(ModeALAC, nil)
	p.Feed("Track 3 of 10: songs")
	p.Feed("Decrypting... 45% (12 MB/26 MB, 3.2 MB/s)")

	// Two tracks are done and the third is 45% through: 2.45 of 10.
	mustContain(t, p.text(),
		"[####----------------] 24%",
		"Track 3 of 10",
		"Decrypting 45% at 3.2 MB/s")
}

func TestProgressIgnoresTerminalColourCodes(t *testing.T) {
	p := NewProgress(ModeALAC, nil)
	p.Feed("\x1b[1;32mTrack 2 of 5: songs\x1b[0m")
	p.Feed("\x1b[2K\x1b[33mDownloading...\x1b[0m 10%")

	mustContain(t, p.text(), "Track 2 of 5", "Downloading 10%")
}

// A track may draw one bar after another, each from 0 to 100. The bar of the
// whole download must not jump backwards when the next one starts.
func TestProgressOfATrackNeverGoesBackwards(t *testing.T) {
	p := NewProgress(ModeALAC, nil)
	p.Feed("Track 1 of 4: songs")
	p.Feed("Downloading... 60%")
	p.Feed("Decrypting... 10%")

	mustContain(t, p.text(), "[###-----------------] 15%", "Decrypting 60%")
}

func TestProgressStartsOverWithEachTrack(t *testing.T) {
	p := NewProgress(ModeALAC, nil)
	p.Feed("Track 2 of 4: songs")
	p.Feed("Decrypting... 80% (8 MB/10 MB, 2.0 MB/s)")
	p.Feed("Track 3 of 4: songs")

	text := p.text()
	mustContain(t, text, "Track 3 of 4")
	if strings.Contains(text, "Decrypting") || strings.Contains(text, "MB/s") {
		t.Fatalf("the previous track's progress must not carry over:\n%s", text)
	}
}

func TestProgressIgnoresLinesItDoesNotUnderstand(t *testing.T) {
	initial := NewProgress(ModeALAC, nil).text()

	p := NewProgress(ModeALAC, nil)
	for _, line := range []string{"", "   ", "Unavailable", "Decrypting... 250%"} {
		p.Feed(line)
	}
	if p.text() != initial {
		t.Fatalf("none of these lines should change the message:\n%s", p.text())
	}
}

// 29/100*100 is 28.999999999999996 in floating point and used to show as 28%.
func TestProgressPercentagesAreExact(t *testing.T) {
	for pct := 0; pct <= 100; pct++ {
		p := NewProgress(ModeALAC, nil)
		p.Feed("Track 1 of 1: songs")
		p.Feed(fmt.Sprintf("Decrypting... %d%%", pct))
		want := fmt.Sprintf("Decrypting %d%%", pct)
		if !strings.Contains(p.text(), want) || !strings.Contains(p.text(), fmt.Sprintf("] %d%%", pct)) {
			t.Fatalf("at %d%% the message reads:\n%s", pct, p.text())
		}
	}
}

// ---- counting only what has to be downloaded ---------------------------------------

func TestProgressDoesNotCountTracksSentFromCache(t *testing.T) {
	p := NewProgress(ModeALAC, nil)
	p.SetPlan(cachePlan{Total: 10, Skip: []int{1, 2, 3, 4, 5}, Delivered: 5})

	p.Feed("Track 6 of 10: songs")
	mustContain(t, p.text(), "[--------------------] 0%", "5 track(s) already sent from cache")

	// Positions 6 and 7 are finished, so 2 of the 5 tracks to download are done.
	p.Feed("Track 8 of 10: songs")
	mustContain(t, p.text(), "[########------------] 40%", "Track 8 of 10")
}

func TestProgressShowsTheTitleAndTrackNames(t *testing.T) {
	p := NewProgress(ModeAtmos, nil)
	p.SetPlan(cachePlan{Total: 10, Title: "Linh Cao - Mân Côi", Titles: map[int]string{3: "Song Three", 4: strings.Repeat("x", 80)}})

	mustContain(t, p.text(), "Downloading [atmos]", "Linh Cao - Mân Côi")

	p.Feed("Track 3 of 10: songs")
	mustContain(t, p.text(), "Track 3 of 10: Song Three")

	p.Feed("Track 4 of 10: songs")
	mustContain(t, p.text(), strings.Repeat("x", 47)+"...")
}

// ---- time ----------------------------------------------------------------------------

func TestProgressShowsElapsedTimeAndAnEstimate(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	p := NewProgress(ModeALAC, nil)
	p.now = clock.Now
	p.startedAt = clock.Now()

	p.Feed("Track 1 of 4: songs")
	p.Feed("Downloading... 50%") // 0.5 of 4 tracks: one eighth of the work

	clock.Set(clock.Now().Add(5 * time.Second))
	text := p.text()
	mustContain(t, text, "5s elapsed")
	if strings.Contains(text, "left") {
		t.Fatalf("five seconds of data is too little for an estimate:\n%s", text)
	}

	clock.Set(clock.Now().Add(55 * time.Second)) // 60 s for 1/8 of the work: 7 minutes to go
	mustContain(t, p.text(), "1m 00s elapsed", "about 7m left")
}

func TestProgressGivesNoEstimateWhenItWouldBeGuesswork(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	p := NewProgress(ModeALAC, nil)
	p.now = clock.Now
	p.startedAt = clock.Now()
	clock.Set(clock.Now().Add(2 * time.Minute))

	p.Feed("Track 1 of 100: songs")
	p.Feed("Downloading... 1%") // 0.01% of the work
	if strings.Contains(p.text(), "left") {
		t.Fatalf("a tiny fraction of the work gives no usable estimate:\n%s", p.text())
	}

	q := NewProgress(ModeALAC, nil)
	q.now = clock.Now
	q.startedAt = clock.Now().Add(-2 * time.Minute)
	q.Feed("Track 4 of 4: songs")
	q.Feed("Downloading... 100%")
	if strings.Contains(q.text(), "left") {
		t.Fatalf("a finished download has nothing left:\n%s", q.text())
	}
}

func TestFormatElapsedAndRemaining(t *testing.T) {
	elapsed := map[time.Duration]string{
		45 * time.Second:         "45s",
		125 * time.Second:        "2m 05s",
		3720 * time.Second:       "1h 02m",
		59600 * time.Millisecond: "1m 00s", // rounded to the nearest second first
	}
	for d, want := range elapsed {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
	if got := formatRemaining(30 * time.Second); got != "under a minute left" {
		t.Errorf("got %q", got)
	}
	if got := formatRemaining(7 * time.Minute); got != "about 7m left" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo wörld", 8); got != "héllo..." {
		t.Errorf("got %q", got)
	}
	if got := truncateRunes("short", 8); got != "short" {
		t.Errorf("got %q", got)
	}
}

// ---- redrawing -------------------------------------------------------------------------

func TestProgressRedrawsOnATimer(t *testing.T) {
	withProgressInterval(t, 20*time.Millisecond)
	var log editLog
	p := NewProgress(ModeALAC, log.add)

	p.Start()
	waitFor(t, "the first drawing", func() bool { return len(log.all()) >= 1 })
	if !strings.Contains(log.all()[0], "working...") {
		t.Fatalf("the bar should appear as soon as the download starts, got %q", log.all()[0])
	}

	p.Feed("Track 1 of 2: songs")
	waitFor(t, "a redraw showing the new track", func() bool { return log.anyContains("Track 1 of 2") })
	p.Done("final")
}

func TestProgressDoesNotEditWhenNothingChanged(t *testing.T) {
	withProgressInterval(t, 10*time.Millisecond)
	var log editLog
	p := NewProgress(ModeALAC, log.add)
	frozen := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return frozen }

	p.Start()
	time.Sleep(120 * time.Millisecond) // a dozen ticks
	if got := len(log.all()); got != 1 {
		t.Fatalf("got %d edits for an unchanged message, want 1", got)
	}

	p.Feed("Track 1 of 2: songs")
	waitFor(t, "an edit after a change", func() bool { return len(log.all()) == 2 })
	p.Done("final")
}

// Feed runs on the goroutine that reads the download's output. If it waited for
// Telegram, a slow network would block the pipe and so the download itself.
func TestProgressFeedNeverWaitsForTheNetwork(t *testing.T) {
	withProgressInterval(t, 10*time.Millisecond)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	p := NewProgress(ModeALAC, func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // a Telegram call that hangs
	})

	p.Start()
	<-entered

	fed := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			p.Feed("Decrypting... 50% (1 MB/2 MB, 1.0 MB/s)")
		}
		close(fed)
	}()
	select {
	case <-fed:
	case <-time.After(2 * time.Second):
		t.Fatal("Feed blocked behind a slow edit")
	}

	close(release)
	p.Done("final")
}

// A progress frame still on its way must never land after the final text.
func TestProgressDoneWaitsForAnEditInFlight(t *testing.T) {
	withProgressInterval(t, 10*time.Millisecond)
	var log editLog
	inFlight := make(chan struct{})
	var once sync.Once
	p := NewProgress(ModeALAC, func(text string) {
		if text != "final" {
			once.Do(func() { close(inFlight) })
			time.Sleep(100 * time.Millisecond) // a slow Telegram call
		}
		log.add(text)
	})

	p.Start()
	<-inFlight
	p.Done("final")

	texts := log.all()
	if texts[len(texts)-1] != "final" {
		t.Fatalf("the final text must be last, got %q", texts)
	}
	n := len(texts)
	time.Sleep(100 * time.Millisecond)
	if len(log.all()) != n {
		t.Fatal("nothing may be edited after Done returns")
	}
}

func TestProgressDoneBeforeStartAndTwice(t *testing.T) {
	var log editLog
	p := NewProgress(ModeALAC, log.add)
	p.Done("a")
	p.Done("b")
	if got := log.all(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("edits = %q", got)
	}

	withProgressInterval(t, 10*time.Millisecond)
	var log2 editLog
	q := NewProgress(ModeALAC, log2.add)
	q.Start()
	q.Done("a")
	q.Done("b") // a second Done after the loop has ended must not panic
	if got := log2.all(); got[len(got)-1] != "b" {
		t.Fatalf("edits = %q", got)
	}
}

func TestProgressStartIsIdempotentAndNilEditIsSafe(t *testing.T) {
	withProgressInterval(t, 10*time.Millisecond)
	var log editLog
	p := NewProgress(ModeALAC, log.add)
	frozen := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return frozen }
	p.Start()
	p.Start()
	time.Sleep(60 * time.Millisecond)
	if got := len(log.all()); got != 1 {
		t.Fatalf("got %d edits, want 1", got)
	}
	p.Done("x")

	silent := NewProgress(ModeALAC, nil)
	silent.Start()
	silent.Feed("Track 1 of 2: songs")
	silent.Done("x")
}

// ---- the process output reader -----------------------------------------------------------

func TestReadLinesDeliversProgressFramesAsTheyArrive(t *testing.T) {
	var seen []string
	tail := readLines(strings.NewReader("Decrypting... 10%\rDecrypting... 50%\rDecrypting... 100%\nnext line\n"),
		func(l string) { seen = append(seen, l) })

	if len(seen) != 4 || seen[0] != "Decrypting... 10%" || seen[1] != "Decrypting... 50%" {
		t.Fatalf("every frame should reach the callback, got %q", seen)
	}
	if len(tail) != 2 || tail[0] != "Decrypting... 100%" || tail[1] != "next line" {
		t.Fatalf("only finished lines belong in the log tail, got %q", tail)
	}
}

func TestReadLinesStripsColourCodesAndHandlesWindowsLineEndings(t *testing.T) {
	var seen []string
	tail := readLines(strings.NewReader("\x1b[31mred\x1b[0m\r\nplain\r\nno newline at the end"),
		func(l string) { seen = append(seen, l) })

	want := []string{"red", "plain", "no newline at the end"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("callback saw %q, want %q", seen, want)
	}
	// "\r\n" ends a line; it is not a redrawn frame, so these belong in the log.
	if strings.Join(tail, "|") != strings.Join(want, "|") {
		t.Fatalf("log tail is %q, want %q", tail, want)
	}
}

func TestReadLinesDoesNotLogAProgressBarThatClearsItself(t *testing.T) {
	// A bar that finishes by erasing itself leaves nothing worth logging.
	tail := readLines(strings.NewReader("Downloading... 99%\rDownloading... 100%\r\x1b[2K\r\nafter\n"), nil)
	if len(tail) != 1 || tail[0] != "after" {
		t.Fatalf("log tail = %q, want only the line after the bar", tail)
	}
}
