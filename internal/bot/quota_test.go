package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// fakeClock lets a test move the day boundary.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// newTestQuota returns a quota whose clock starts at 09:00 UTC on 10 Oct 2026.
func newTestQuota(cfg QuotaConfig, exempt ...int64) (*Quota, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	q := NewQuota(cfg, "", exempt)
	q.now = clock.Now
	return q, clock
}

func mustReserve(t *testing.T, q *Quota, user int64, tracks int) *Reservation {
	t.Helper()
	r, err := q.Reserve(user, tracks)
	if err != nil {
		t.Fatalf("Reserve(%d, %d) refused: %v", user, tracks, err)
	}
	return r
}

func mustRefuse(t *testing.T, q *Quota, user int64, tracks int) string {
	t.Helper()
	r, err := q.Reserve(user, tracks)
	if err == nil {
		r.Release()
		t.Fatalf("Reserve(%d, %d) should have been refused", user, tracks)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("expected a *QuotaError, got %T: %v", err, err)
	}
	return err.Error()
}

// ---- the tracker ----------------------------------------------------------------

func TestNoLimitsMeansNoQuota(t *testing.T) {
	if NewQuota(QuotaConfig{}, "", nil) != nil {
		t.Fatal("with no limit set there is nothing to track")
	}

	var q *Quota // a nil quota must be safe to use everywhere
	r, err := q.Reserve(1, 1000)
	if err != nil || r == nil {
		t.Fatalf("a nil quota must allow everything, got %v", err)
	}
	r.Commit(5, 5)
	r.Release()
	q.Reset(1)
	q.Flush()
	if !strings.Contains(q.Report(1), "No download limits") {
		t.Fatalf("report = %q", q.Report(1))
	}
}

func TestQuotaTrackLimit(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 5})

	mustReserve(t, q, 1, 3).Commit(3, 0)
	second := mustReserve(t, q, 1, 2) // 3 used + 2 reserved = the whole allowance

	msg := mustRefuse(t, q, 1, 1)
	if !strings.Contains(msg, "Daily limit reached") || !strings.Contains(msg, "3 of 5") || !strings.Contains(msg, "2 more are being downloaded") {
		t.Fatalf("message = %q", msg)
	}

	second.Release()
	mustReserve(t, q, 1, 2) // the cancelled job gave its share back
}

func TestQuotaRefusesWhatDoesNotFit(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 5})
	mustReserve(t, q, 1, 3).Commit(3, 0)

	msg := mustRefuse(t, q, 1, 4)
	if !strings.Contains(msg, "needs 4 track(s)") || !strings.Contains(msg, "only 2 are left") || !strings.Contains(msg, "limit of 5") {
		t.Fatalf("message = %q", msg)
	}
}

// Two downloads of one user running at once must not both be allowed through
// when only one fits.
func TestQuotaParallelJobsCannotBothSlipThrough(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 5})

	mustReserve(t, q, 1, 3) // still running, not yet committed
	mustRefuse(t, q, 1, 3)
}

func TestQuotaCommitChargesTheRealCount(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 5})

	mustReserve(t, q, 1, 5).Commit(2, 0) // reserved a whole album, only 2 came down
	mustReserve(t, q, 1, 3)              // so 3 are still available
	mustRefuse(t, q, 1, 1)
}

func TestQuotaReleaseChargesNothingAndIsIdempotent(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 5})

	r := mustReserve(t, q, 1, 5)
	r.Release()
	r.Release()
	r.Commit(5, 100) // too late: the reservation is already settled

	mustReserve(t, q, 1, 5) // the whole allowance is intact
}

func TestQuotaDataLimitIsSoft(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyBytes: 1000})

	r := mustReserve(t, q, 1, 1)  // allowed: nothing used yet
	r.Commit(1, 5000)             // the job turned out to be much bigger than the allowance
	msg := mustRefuse(t, q, 1, 1) // but the next one is refused
	if !strings.Contains(msg, "Daily data limit reached") {
		t.Fatalf("message = %q", msg)
	}
}

func TestQuotaResetsAtLocalMidnight(t *testing.T) {
	q, clock := newTestQuota(QuotaConfig{DailyTracks: 2})
	mustReserve(t, q, 1, 2).Commit(2, 0)

	clock.Set(time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC))
	if msg := mustRefuse(t, q, 1, 1); !strings.Contains(msg, "resets in 1h 30m") {
		t.Fatalf("the refusal should say when the limit resets: %q", msg)
	}

	clock.Set(time.Date(2026, 10, 11, 0, 5, 0, 0, time.UTC))
	mustReserve(t, q, 1, 2) // a new day
	if !strings.Contains(q.Report(1), "Tracks: 0 of 2 used") {
		t.Fatalf("usage should have reset:\n%s", q.Report(1))
	}
}

func TestQuotaIsPerUser(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 1})
	mustReserve(t, q, 1, 1).Commit(1, 0)

	mustRefuse(t, q, 1, 1)
	mustReserve(t, q, 2, 1) // someone else is unaffected
}

func TestQuotaExemptUsersAreNeverLimited(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 1}, 9)

	for i := 0; i < 3; i++ {
		mustReserve(t, q, 9, 100).Commit(100, 1<<40)
	}
	if !strings.Contains(q.Report(9), "Exempt") {
		t.Fatalf("report = %q", q.Report(9))
	}
}

func TestQuotaReset(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 1})
	mustReserve(t, q, 1, 1).Commit(1, 0)
	mustRefuse(t, q, 1, 1)

	q.Reset(1)
	mustReserve(t, q, 1, 1)
}

func TestQuotaConcurrentReservationsNeverExceedTheLimit(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 10})

	var granted int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.Reserve(1, 1); err == nil {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&granted); got != 10 {
		t.Fatalf("%d reservations were granted for a limit of 10", got)
	}
}

func TestQuotaReportDescribesUsage(t *testing.T) {
	q, _ := newTestQuota(QuotaConfig{DailyTracks: 20, DailyBytes: 2 << 30})
	mustReserve(t, q, 1, 7).Commit(7, 310*1024*1024)
	mustReserve(t, q, 1, 3) // running

	report := q.Report(1)
	for _, want := range []string{
		"resets in 15h",
		"Tracks: 7 of 20 used",
		"Data: 310.0 MB of 2.0 GB used",
		"In progress: 3 track(s)",
		"cache are free",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}

	onlyBytes, _ := newTestQuota(QuotaConfig{DailyBytes: 1 << 30})
	mustReserve(t, onlyBytes, 1, 2).Commit(2, 0)
	if !strings.Contains(onlyBytes.Report(1), "Tracks: 2 used, no limit") {
		t.Fatalf("report = %q", onlyBytes.Report(1))
	}
}

func TestQuotaSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	cfg := QuotaConfig{DailyTracks: 5}

	first := NewQuota(cfg, path, nil)
	mustReserve(t, first, 1, 3).Commit(3, 700)

	second := NewQuota(cfg, path, nil)
	if report := second.Report(1); !strings.Contains(report, "Tracks: 3 of 5 used") {
		t.Fatalf("usage was lost across a restart:\n%s", report)
	}
}

func TestQuotaDropsUsageFromEarlierDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	today := time.Now().Format("2006-01-02")
	data := fmt.Sprintf(`{"1":{"day":"2000-01-01","tracks":99,"bytes":9},"2":{"day":%q,"tracks":4,"bytes":0}}`, today)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	q := NewQuota(QuotaConfig{DailyTracks: 5}, path, nil)
	if !strings.Contains(q.Report(1), "Tracks: 0 of 5 used") {
		t.Fatalf("an earlier day's usage must not count:\n%s", q.Report(1))
	}
	if !strings.Contains(q.Report(2), "Tracks: 4 of 5 used") {
		t.Fatalf("today's usage must be kept:\n%s", q.Report(2))
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:                  "less than a minute",
		12 * time.Minute:                  "12m",
		5 * time.Hour:                     "5h",
		90 * time.Minute:                  "1h 30m",
		61*time.Minute + time.Second:      "1h 2m", // rounded up
		23*time.Hour + 59*time.Minute + 1: "24h",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// ---- inside a job ---------------------------------------------------------------

func quotaBot(t *testing.T, f *fakeTG, cfg QuotaConfig) *Bot {
	t.Helper()
	b := newTestBot(t, f, func(c *BotConfig) { c.AdminIDs = []int64{900} })
	b.quota = NewQuota(cfg, "", []int64{900})
	return b
}

func lastEditText(f *fakeTG) string {
	edits := f.callsTo("editMessageText")
	if len(edits) == 0 {
		return ""
	}
	return edits[len(edits)-1].Form.Get("text")
}

func TestProcessJobRefusesADownloadOverTheQuotaWithoutStartingOne(t *testing.T) {
	started := 0
	orig := workerCommand
	workerCommand = func(string) (*exec.Cmd, error) {
		started++
		return nil, errors.New("a download must not start")
	}
	t.Cleanup(func() { workerCommand = orig })

	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 1})
	mustReserve(t, b.quota, 1, 1).Commit(1, 0) // the day's only track is used

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	b.processJob(job)

	if started != 0 {
		t.Fatal("no download process may be started for a user over the limit")
	}
	if !strings.Contains(lastEditText(f), "Daily limit reached") {
		t.Fatalf("the user should be told why, got %q", lastEditText(f))
	}
	if job.Status != JobFailed {
		t.Fatalf("job status = %q", job.Status)
	}
}

// Tracks from the cache cost the server nothing, so a user who has used up the
// day's allowance still gets them.
func TestProcessJobServesCachedTracksEvenOverTheQuota(t *testing.T) {
	stubPause(t, func(time.Duration) {})
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 1})
	mustReserve(t, b.quota, 1, 1).Commit(1, 0)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("FID1"))

	b.processJob(newJob(1, 100, 5, songURL, ModeALAC, "u"))

	if len(f.callsTo("sendAudio")) != 1 {
		t.Fatal("the cached track should have been sent")
	}
	if strings.Contains(lastEditText(f), "limit") {
		t.Fatalf("a cache hit must not mention the limit: %q", lastEditText(f))
	}
	if !strings.Contains(b.quota.Report(1), "Tracks: 1 of 1 used") {
		t.Fatalf("serving from the cache must not be charged:\n%s", b.quota.Report(1))
	}
}

func TestProcessJobChargesWhatWasDownloaded(t *testing.T) {
	useHelperWorker(t, "success")
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 3})

	b.processJob(newJob(1, 100, 5, songURL, ModeALAC, "u"))

	if !strings.Contains(b.quota.Report(1), "Tracks: 1 of 3 used") {
		t.Fatalf("the downloaded track should be charged:\n%s", b.quota.Report(1))
	}
	if strings.Contains(b.quota.Report(1), "In progress") {
		t.Fatal("the reservation must be settled once the job is done")
	}
}

func TestProcessJobChargesTracksThatCameDownBeforeAFailure(t *testing.T) {
	useHelperWorker(t, "partial")
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 10})

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	b.processJob(job)

	if job.Status != JobFailed {
		t.Fatalf("job status = %q, want failed", job.Status)
	}
	if !strings.Contains(b.quota.Report(1), "Tracks: 2 of 10 used") {
		t.Fatalf("the two tracks that came down must be charged even though the job failed:\n%s", b.quota.Report(1))
	}
}

func TestProcessJobCancelReleasesTheReservation(t *testing.T) {
	useHelperWorker(t, "hang")
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 3})
	job := newJob(1, 100, 5, songURL, ModeALAC, "u")

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
	if !strings.Contains(b.quota.Report(1), "In progress: 1 track(s)") {
		t.Fatalf("a running download should hold a reservation:\n%s", b.quota.Report(1))
	}

	job.cancel()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the job did not stop")
	}

	report := b.quota.Report(1)
	if strings.Contains(report, "In progress") || !strings.Contains(report, "Tracks: 0 of 3 used") {
		t.Fatalf("a cancelled download must give its reservation back and cost nothing:\n%s", report)
	}
}

// ---- commands -------------------------------------------------------------------

// commandMessage builds the message Telegram would deliver for a command.
func commandMessage(t *testing.T, from int64, text string) *tgbotapi.Message {
	t.Helper()
	length := len(text)
	if i := strings.Index(text, " "); i >= 0 {
		length = i
	}
	raw, err := json.Marshal(map[string]any{
		"message_id": 1,
		"from":       map[string]any{"id": from, "is_bot": false, "first_name": "t"},
		"chat":       map[string]any{"id": from, "type": "private"},
		"text":       text,
		"entities":   []map[string]any{{"type": "bot_command", "offset": 0, "length": length}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var msg tgbotapi.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	return &msg
}

func lastReply(f *fakeTG) string {
	msgs := f.callsTo("sendMessage")
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Form.Get("text")
}

func TestQuotaCommand(t *testing.T) {
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 5})
	mustReserve(t, b.quota, 7, 2).Commit(2, 0)
	mustReserve(t, b.quota, 8, 4).Commit(4, 0)

	b.cmdQuota(commandMessage(t, 7, "/quota"))
	if !strings.Contains(lastReply(f), "Tracks: 2 of 5 used") {
		t.Fatalf("a user should see their own usage, got %q", lastReply(f))
	}

	b.cmdQuota(commandMessage(t, 7, "/quota 8"))
	if !strings.Contains(lastReply(f), "Only admins") {
		t.Fatalf("a normal user must not look at someone else's quota, got %q", lastReply(f))
	}

	b.cmdQuota(commandMessage(t, 900, "/quota 8"))
	if reply := lastReply(f); !strings.Contains(reply, "User 8") || !strings.Contains(reply, "Tracks: 4 of 5 used") {
		t.Fatalf("an admin should see the other user's usage, got %q", reply)
	}

	b.cmdQuota(commandMessage(t, 900, "/quota abc"))
	if !strings.Contains(lastReply(f), "Usage") {
		t.Fatalf("got %q", lastReply(f))
	}
}

func TestQuotaCommandWithoutLimits(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f) // b.quota is nil: no limits configured

	b.cmdQuota(commandMessage(t, 7, "/quota"))
	if !strings.Contains(lastReply(f), "No download limits") {
		t.Fatalf("got %q", lastReply(f))
	}
}

func TestResetQuotaCommand(t *testing.T) {
	f := newFakeTG(t)
	b := quotaBot(t, f, QuotaConfig{DailyTracks: 1})
	mustReserve(t, b.quota, 8, 1).Commit(1, 0)
	mustRefuse(t, b.quota, 8, 1)

	b.cmdResetQuota(commandMessage(t, 7, "/resetquota 8"))
	if !strings.Contains(lastReply(f), "admins only") {
		t.Fatalf("a normal user must not reset quotas, got %q", lastReply(f))
	}
	mustRefuse(t, b.quota, 8, 1) // still limited

	b.cmdResetQuota(commandMessage(t, 900, "/resetquota"))
	if !strings.Contains(lastReply(f), "Usage") {
		t.Fatalf("got %q", lastReply(f))
	}

	b.cmdResetQuota(commandMessage(t, 900, "/resetquota 8"))
	if !strings.Contains(lastReply(f), "has been reset") {
		t.Fatalf("got %q", lastReply(f))
	}
	mustReserve(t, b.quota, 8, 1)
}
