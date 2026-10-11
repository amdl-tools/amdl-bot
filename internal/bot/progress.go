package bot

// progress.go builds the progress message shown while a download runs.
//
// It reads what the downloader prints: a "Track X of Y: ..." line when a track
// starts, and the frames of the downloader's own per-track progress bar, such as
// "Decrypting... 45% (12 MB/26 MB, 3.2 MB/s)". From those it shows overall
// progress, the track in progress, its speed, the time elapsed and an estimate of
// the time left.
//
// Reading and drawing are separate: Feed only updates state and never waits on
// the network, while a goroutine edits the Telegram message at a fixed interval.
// Editing from Feed would stall the pipe the download process writes to whenever
// Telegram is slow, and stall the download with it.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	barWidth = 20 // characters wide
	barFill  = "#"
	barEmpty = "-"
)

// progressInterval is how often the message is redrawn; tests shorten it.
var progressInterval = 3 * time.Second

var (
	// A terminal colour or cursor sequence such as ESC[32m or ESC[2K.
	reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// The Runner's "Track X of Y: <type>" line.
	reTrack = regexp.MustCompile(`Track\s+(\d+)\s+of\s+(\d+)`)
	// A percentage in a progress bar frame.
	rePercent = regexp.MustCompile(`(\d{1,3})\s*%`)
	// A transfer rate such as "3.2 MB/s".
	reRate = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*([KMGT]?i?B)/s`)
)

// stripANSI removes terminal colour and cursor sequences from a line.
func stripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

// Progress tracks one job's download and draws it into a Telegram message.
type Progress struct {
	editFn   func(text string)
	interval time.Duration
	now      func() time.Time // replaced by tests

	mu        sync.Mutex // guards everything below
	mode      Mode
	title     string         // "Artist - Album"; empty when unknown
	total     int            // tracks in the link; 0 when unknown
	delivered int            // tracks already sent from the cache
	skip      map[int]bool   // positions that were sent from the cache
	titles    map[int]string // position -> track name
	current   int            // position of the track being downloaded; 0 before the first
	pct       int            // progress of that track in percent, never going backwards
	havePct   bool           // a progress frame was seen for that track
	phase     string         // what it is doing, such as "Decrypting"
	rate      string         // such as "3.2 MB/s"
	startedAt time.Time
	lastSent  string

	stop     chan struct{} // closed to end the drawing goroutine; nil until Start
	stopped  chan struct{} // closed when that goroutine has exited
	stopOnce sync.Once
}

// NewProgress creates a tracker. editFn replaces the text of the Telegram message.
func NewProgress(mode Mode, editFn func(text string)) *Progress {
	return &Progress{
		mode:     mode,
		editFn:   editFn,
		interval: progressInterval,
		now:      time.Now,
	}
}

// SetPlan gives the tracker what the cache lookup learned about the link: how many
// tracks it holds, which of them were already sent, and their names.
func (p *Progress) SetPlan(plan cachePlan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.title = plan.Title
	p.total = plan.Total
	p.delivered = plan.Delivered
	p.titles = plan.Titles
	p.skip = make(map[int]bool, len(plan.Skip))
	for _, pos := range plan.Skip {
		p.skip[pos] = true
	}
}

// Start begins redrawing the message every interval. It draws once at once, so
// the user sees the bar as soon as the download starts.
func (p *Progress) Start() {
	p.mu.Lock()
	if p.stop != nil || p.editFn == nil {
		p.mu.Unlock()
		return
	}
	p.startedAt = p.now()
	p.stop = make(chan struct{})
	p.stopped = make(chan struct{})
	stop, stopped := p.stop, p.stopped
	p.mu.Unlock()

	go func() {
		defer close(stopped)
		p.tick()
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.tick()
			}
		}
	}()
}

// tick edits the message when the text has changed since the last edit.
func (p *Progress) tick() {
	p.mu.Lock()
	text := p.render()
	changed := text != p.lastSent
	if changed {
		p.lastSent = text
	}
	fn := p.editFn
	p.mu.Unlock()

	if changed && fn != nil {
		fn(text) // outside the lock: this is a network call
	}
}

// Elapsed is how long the download has been running, or 0 before Start.
func (p *Progress) Elapsed() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startedAt.IsZero() {
		return 0
	}
	return p.now().Sub(p.startedAt)
}

// Done stops the redrawing and replaces the message with the final text. It waits
// for an edit that is still in flight first, so a late progress frame can never
// overwrite the final text.
func (p *Progress) Done(text string) {
	p.mu.Lock()
	stop, stopped := p.stop, p.stopped
	fn := p.editFn
	p.mu.Unlock()

	if stop != nil {
		p.stopOnce.Do(func() { close(stop) })
		select {
		case <-stopped:
		case <-time.After(10 * time.Second): // never hang the job on a stuck network call
		}
	}
	if fn != nil {
		fn(text)
	}
}

// Feed accepts one line, or one redrawn frame of a progress bar, printed by the
// download, and updates the state. It does no I/O and never blocks.
func (p *Progress) Feed(line string) {
	line = strings.TrimSpace(stripANSI(line))
	if line == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if m := reTrack.FindStringSubmatch(line); m != nil {
		cur, _ := strconv.Atoi(m[1])
		tot, _ := strconv.Atoi(m[2])
		if cur > 0 && tot > 0 {
			p.current, p.total = cur, tot
			p.pct, p.havePct, p.phase, p.rate = 0, false, "", ""
		}
		return
	}

	if loc := rePercent.FindStringSubmatchIndex(line); loc != nil {
		pct, err := strconv.Atoi(line[loc[2]:loc[3]])
		if err != nil || pct < 0 || pct > 100 {
			return
		}
		// A track can show more than one bar one after the other (each running from
		// 0 to 100); the track's own progress must still only move forward.
		if pct > p.pct {
			p.pct = pct
		}
		p.havePct = true
		p.phase = strings.TrimRight(strings.TrimSpace(line[:loc[0]]), ". ")
		p.rate = ""
		if r := reRate.FindStringSubmatch(line); r != nil {
			p.rate = r[1] + " " + r[2] + "/s"
		}
	}
}

// work reports how much of the download is done, counted in tracks (the current
// track counts by its fraction), and how many tracks the download has in all.
// Tracks that were sent from the cache are not part of either number. total is 0
// when the number of tracks is not known.
func (p *Progress) work() (done float64, total int) {
	total = p.total - len(p.skip)
	if p.total <= 0 || total <= 0 {
		return 0, 0
	}
	if p.current == 0 {
		return 0, total
	}
	before := 0
	for pos := 1; pos < p.current && pos <= p.total; pos++ {
		if !p.skip[pos] {
			before++
		}
	}
	done = float64(before) + float64(p.pct)/100
	if done > float64(total) {
		done = float64(total)
	}
	return done, total
}

// render returns the message text. The caller holds p.mu.
func (p *Progress) render() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Downloading [%s]\n", p.mode)
	if p.title != "" {
		sb.WriteString(truncateRunes(p.title, 60) + "\n")
	}

	done, total := p.work()
	if total > 0 {
		pct := percentOf(done, total)
		filled := pct * barWidth / 100
		fmt.Fprintf(&sb, "[%s%s] %d%%\n", strings.Repeat(barFill, filled), strings.Repeat(barEmpty, barWidth-filled), pct)
	} else {
		sb.WriteString("[" + strings.Repeat(barFill, barWidth/2) + strings.Repeat(barEmpty, barWidth/2) + "] working...\n")
	}

	if p.current > 0 {
		line := fmt.Sprintf("Track %d of %d", p.current, p.total)
		if name := p.titles[p.current]; name != "" {
			line += ": " + truncateRunes(name, 50)
		}
		sb.WriteString(line + "\n")
	}
	if p.havePct {
		phase := p.phase
		if phase == "" {
			phase = "Working"
		}
		line := fmt.Sprintf("%s %d%%", phase, p.pct)
		if p.rate != "" {
			line += " at " + p.rate
		}
		sb.WriteString(line + "\n")
	}

	if !p.startedAt.IsZero() {
		elapsed := p.now().Sub(p.startedAt)
		line := formatElapsed(elapsed) + " elapsed"
		// An estimate from a few seconds of data is noise; wait for some progress.
		if total > 0 && done > 0 && elapsed >= 10*time.Second {
			if frac := done / float64(total); frac >= 0.03 && frac < 1 {
				remaining := time.Duration(float64(elapsed) * (1 - frac) / frac)
				line += ", " + formatRemaining(remaining)
			}
		}
		sb.WriteString(line + "\n")
	}
	if p.delivered > 0 {
		fmt.Fprintf(&sb, "%d track(s) already sent from cache\n", p.delivered)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// percentOf is done/total as a whole percentage. The small allowance keeps a
// result such as 28.999999999999996 from showing as 28.
func percentOf(done float64, total int) int {
	return int(done/float64(total)*100 + 1e-9)
}

// truncateRunes shortens s to at most n characters, ending with "..." when cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

// formatElapsed writes a duration as "45s", "2m 05s" or "1h 02m".
func formatElapsed(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm %02ds", secs/60, secs%60)
	default:
		return fmt.Sprintf("%dh %02dm", secs/3600, secs%3600/60)
	}
}

// formatRemaining words an estimate of the time left.
func formatRemaining(d time.Duration) string {
	if d < time.Minute {
		return "under a minute left"
	}
	return "about " + formatDuration(d) + " left"
}
