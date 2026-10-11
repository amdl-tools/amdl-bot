package bot

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func audioEntry(fileID string) FileIDEntry {
	return FileIDEntry{FileID: fileID, Kind: "audio", Artist: "Artist", Song: "Song"}
}

func TestServeFromCacheSongHit(t *testing.T) {
	stubPause(t, func(time.Duration) {})
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("FID1"))

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	plan := b.serveFromCache(job, func(string) {})

	if !plan.fullyServed() || plan.Delivered != 1 || plan.Total != 1 {
		t.Fatalf("plan = %+v, want one track delivered", plan)
	}
	if len(plan.Names) != 1 || plan.Names[0] != "Artist - Song" {
		t.Fatalf("Names = %q", plan.Names)
	}
	calls := f.callsTo("sendAudio")
	if len(calls) != 1 {
		t.Fatalf("got %d sendAudio calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Form.Get("audio") != "FID1" || c.Form.Get("chat_id") != "100" ||
		c.Form.Get("performer") != "Artist" || c.Form.Get("title") != "Song" {
		t.Fatalf("unexpected send parameters: %v", c.Form)
	}
	if len(c.Files) != 0 {
		t.Fatalf("a cache hit must not upload anything, got file parts %v", c.Files)
	}
}

// The case from the bug report: an album link that points at one song.
func TestServeFromCacheAlbumLinkWithSongParameter(t *testing.T) {
	stubPause(t, func(time.Duration) {})
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("169962189", ModeALAC, audioEntry("FANTASY"))

	job := newJob(1, 100, 5, "https://music.apple.com/gb/album/fantasy/169962059?i=169962189", ModeALAC, "u")
	plan := b.serveFromCache(job, func(string) {})

	if !plan.fullyServed() {
		t.Fatalf("the song behind ?i= should be served from cache: %+v", plan)
	}
	if got := f.callsTo("sendAudio")[0].Form.Get("audio"); got != "FANTASY" {
		t.Fatalf("sent file_id %q, want FANTASY", got)
	}
}

func TestServeFromCacheSendsDocumentsAsDocuments(t *testing.T) {
	stubPause(t, func(time.Duration) {})
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeMV, FileIDEntry{FileID: "VID", Kind: "document", Artist: "A", Song: "S"})

	job := newJob(1, 100, 5, "https://music.apple.com/us/music-video/some-video/1234567890", ModeMV, "u")
	plan := b.serveFromCache(job, func(string) {})

	if !plan.fullyServed() {
		t.Fatalf("plan = %+v", plan)
	}
	if len(f.callsTo("sendDocument")) != 1 || len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("a document entry must be sent with sendDocument")
	}
}

func TestServeFromCacheMiss(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	plan := b.serveFromCache(job, func(string) {})

	if plan.fullyServed() || plan.Delivered != 0 || plan.Total != 1 {
		t.Fatalf("plan = %+v, want nothing delivered and one track to fetch", plan)
	}
	if len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("nothing should be sent on a miss")
	}
}

func TestServeFromCacheIsPerQuality(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("LOSSLESS"))

	job := newJob(1, 100, 5, songURL, ModeAtmos, "u")
	plan := b.serveFromCache(job, func(string) {})

	if plan.Delivered != 0 {
		t.Fatal("an ALAC upload must not be served for an Atmos request")
	}
}

func TestServeFromCacheEvictsStaleFileID(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("OLD"))
	f.setOverride(func(c tgCall) (int, string, bool) {
		if c.Method == "sendAudio" {
			return http.StatusBadRequest, staleFileIDJSON, true
		}
		return 0, "", false
	})

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	plan := b.serveFromCache(job, func(string) {})

	if plan.Delivered != 0 {
		t.Fatal("a rejected file_id must not count as delivered")
	}
	if _, ok := b.fileIDCache.Get("1234567890", ModeALAC); ok {
		t.Fatal("a file_id Telegram rejected must be evicted")
	}
}

func TestServeFromCacheKeepsEntryOnTransientError(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("GOOD"))
	f.setOverride(func(c tgCall) (int, string, bool) {
		if c.Method == "sendAudio" {
			return http.StatusInternalServerError,
				`{"ok":false,"error_code":500,"description":"Internal Server Error","parameters":{}}`, true
		}
		return 0, "", false
	})

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	plan := b.serveFromCache(job, func(string) {})

	if plan.Delivered != 0 {
		t.Fatal("a failed send must not count as delivered")
	}
	if _, ok := b.fileIDCache.Get("1234567890", ModeALAC); !ok {
		t.Fatal("a transient error must not throw away a valid cache entry")
	}
}

func TestServeFromCacheStopsWhenCancelled(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("FID1"))

	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	job.cancel()
	plan := b.serveFromCache(job, func(string) {})

	if plan.Delivered != 0 || len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("a cancelled job must not send anything")
	}
}

func TestProcessJobFullyCachedDownloadsNothing(t *testing.T) {
	stubPause(t, func(time.Duration) {})
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("1234567890", ModeALAC, audioEntry("FID1"))

	job := newJob(1, 100, 5, songURL, ModeALAC, "alice")
	b.processJob(job)

	if len(f.callsTo("sendAudio")) != 1 {
		t.Fatalf("got %d sendAudio calls, want 1", len(f.callsTo("sendAudio")))
	}
	edits := f.callsTo("editMessageText")
	if len(edits) == 0 {
		t.Fatal("the progress message should have been edited")
	}
	if last := edits[len(edits)-1].Form.Get("text"); !strings.Contains(last, "from cache") {
		t.Fatalf("final message should say the tracks came from cache, got %q", last)
	}
	if job.Status != JobDone {
		t.Fatalf("job status = %q, want %q", job.Status, JobDone)
	}
	if b.stats.TotalCacheHits != 1 {
		t.Fatalf("TotalCacheHits = %d, want 1", b.stats.TotalCacheHits)
	}
}

func TestProcessJobCancelledBeforeStartDownloadsNothing(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	job := newJob(1, 100, 5, songURL, ModeALAC, "alice")
	job.cancel()
	b.processJob(job)

	edits := f.callsTo("editMessageText")
	if len(edits) == 0 || edits[len(edits)-1].Form.Get("text") != "Cancelled." {
		t.Fatalf("a cancelled job should end with \"Cancelled.\", edits: %v", edits)
	}
}

func TestSendRetryWaitsOutFloodLimit(t *testing.T) {
	var slept []time.Duration
	stubPause(t, func(d time.Duration) { slept = append(slept, d) })

	f := newFakeTG(t)
	b := newTestBot(t, f)
	var attempts int32
	f.setOverride(func(c tgCall) (int, string, bool) {
		if c.Method == "sendMessage" && atomic.AddInt32(&attempts, 1) == 1 {
			return http.StatusTooManyRequests,
				`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`, true
		}
		return 0, "", false
	})

	if _, err := b.sendRetry(tgbotapi.NewMessage(1, "hi")); err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	if got := len(f.callsTo("sendMessage")); got != 2 {
		t.Fatalf("got %d attempts, want 2", got)
	}
	if len(slept) != 1 || slept[0] != 4*time.Second {
		t.Fatalf("slept %v, want one 4s wait (retry_after 3 plus a margin)", slept)
	}
}

func TestSendRetryGivesUpOnVeryLongWaits(t *testing.T) {
	stubPause(t, func(time.Duration) { t.Error("must not wait this long") })

	f := newFakeTG(t)
	b := newTestBot(t, f)
	f.setOverride(func(c tgCall) (int, string, bool) {
		return http.StatusTooManyRequests,
			`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 600","parameters":{"retry_after":600}}`, true
	})

	if _, err := b.sendRetry(tgbotapi.NewMessage(1, "hi")); err == nil {
		t.Fatal("a 10 minute flood wait should be reported, not slept through")
	}
	if got := len(f.callsTo("sendMessage")); got != 1 {
		t.Fatalf("got %d attempts, want 1", got)
	}
}

func TestSendRetryDoesNotRetryOtherErrors(t *testing.T) {
	stubPause(t, func(time.Duration) { t.Error("must not wait") })

	f := newFakeTG(t)
	b := newTestBot(t, f)
	f.setOverride(func(c tgCall) (int, string, bool) {
		return http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: chat not found","parameters":{}}`, true
	})

	if _, err := b.sendRetry(tgbotapi.NewMessage(1, "hi")); err == nil {
		t.Fatal("expected the error to be returned")
	}
	if got := len(f.callsTo("sendMessage")); got != 1 {
		t.Fatalf("got %d attempts, want 1", got)
	}
}

func TestIsStaleFileID(t *testing.T) {
	stale := []string{
		"Bad Request: wrong file identifier/HTTP URL specified",
		"Bad Request: wrong remote file identifier specified",
		"Bad Request: invalid file_id",
	}
	for _, msg := range stale {
		if !isStaleFileID(&testErr{msg}) {
			t.Errorf("%q should be treated as a stale file_id", msg)
		}
	}
	transient := []string{
		"Too Many Requests: retry after 5",
		"Internal Server Error",
		"Post \"https://api.telegram.org\": dial tcp: i/o timeout",
		"Bad Request: chat not found",
	}
	for _, msg := range transient {
		if isStaleFileID(&testErr{msg}) {
			t.Errorf("%q must not evict a cache entry", msg)
		}
	}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
