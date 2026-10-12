package bot

import (
	"amdl/internal/app"
	"context"
	"errors"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

func runFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v failed: %v\n%s", args, err, out)
	}
}

// makeMedia creates a 2 second m4a without artwork, the same m4a with a 2000x2000
// cover embedded, and that cover as a standalone jpg.
func makeMedia(t *testing.T) (plain, withCover, cover string) {
	t.Helper()
	dir := t.TempDir()
	plain = filepath.Join(dir, "plain.m4a")
	withCover = filepath.Join(dir, "withcover.m4a")
	cover = filepath.Join(dir, "cover.jpg")

	runFFmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "aac", plain)
	runFFmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=2000x2000:rate=1", "-frames:v", "1", "-q:v", "2", cover)
	runFFmpeg(t, "-i", plain, "-i", cover, "-map", "0:a", "-map", "1:v",
		"-c", "copy", "-disposition:v:0", "attached_pic", withCover)
	return plain, withCover, cover
}

func jpegSize(t *testing.T, path string) (width, height int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("%s is not a valid JPEG: %v", path, err)
	}
	return cfg.Width, cfg.Height
}

func writeTrackFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileGone(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// ---- thumbnails ------------------------------------------------------------

// Telegram silently ignores a thumbnail above 320 px or 200 kB, which is what
// the raw album cover used to be.
func TestBuildThumbnailShrinksLargeCover(t *testing.T) {
	requireFFmpeg(t)
	plain, _, cover := makeMedia(t)

	thumb, cleanup := (&Bot{}).buildThumbnail(app.AddedTrack{Path: plain, CoverPath: cover})
	if thumb == "" {
		t.Fatal("expected a thumbnail")
	}
	info, err := os.Stat(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 || info.Size() > maxThumbBytes {
		t.Fatalf("thumbnail is %d bytes, want 1..%d", info.Size(), maxThumbBytes)
	}
	if w, h := jpegSize(t, thumb); w > 320 || h > 320 {
		t.Fatalf("thumbnail is %dx%d, Telegram allows at most 320x320", w, h)
	}

	cleanup()
	if !fileGone(thumb) {
		t.Error("cleanup should remove the temporary thumbnail")
	}
}

func TestBuildThumbnailFallsBackToEmbeddedArtwork(t *testing.T) {
	requireFFmpeg(t)
	_, withCover, _ := makeMedia(t)

	missing := filepath.Join(t.TempDir(), "missing.jpg")
	thumb, cleanup := (&Bot{}).buildThumbnail(app.AddedTrack{Path: withCover, CoverPath: missing})
	defer cleanup()

	if thumb == "" {
		t.Fatal("the artwork embedded in the file should be used when the cover file is gone")
	}
	if w, h := jpegSize(t, thumb); w > 320 || h > 320 {
		t.Fatalf("thumbnail is %dx%d", w, h)
	}
}

func TestBuildThumbnailWithoutAnyArtwork(t *testing.T) {
	requireFFmpeg(t)
	plain, _, _ := makeMedia(t)

	thumb, cleanup := (&Bot{}).buildThumbnail(app.AddedTrack{Path: plain})
	cleanup() // must be safe to call even when nothing was produced

	if thumb != "" {
		t.Fatalf("no artwork exists, but got thumbnail %q", thumb)
	}
}

// ---- sendFiles ---------------------------------------------------------------

// The downloaded file used to survive in on_upload mode whenever the track was
// answered from the cache.
func TestSendFilesServesCachedTrackAndCleansUp(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f, func(c *BotConfig) { c.DeleteMode = "on_upload" })
	b.fileIDCache.Set("555", ModeALAC, audioEntry("CACHED"))
	path := writeTrackFile(t, "01. Song.m4a", "audio")

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: path, TrackID: "555", Artist: "Artist", Song: "Song"}})

	calls := f.callsTo("sendAudio")
	if len(calls) != 1 || calls[0].Form.Get("audio") != "CACHED" || len(calls[0].Files) != 0 {
		t.Fatalf("expected one send by file_id and no upload, got %+v", calls)
	}
	if !fileGone(path) {
		t.Error("the downloaded file should be deleted after a cached send in on_upload mode")
	}
}

// A thumbnail-less upload must not be cached, or every later request for the
// track would be answered with that thumbnail-less copy forever.
func TestSendFilesDoesNotCacheUploadsWithoutThumbnail(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f, func(c *BotConfig) { c.DeleteMode = "on_upload" })
	path := writeTrackFile(t, "01. Song.m4a", "this is not real audio")

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: path, TrackID: "777", Artist: "A", Song: "S"}})

	calls := f.callsTo("sendAudio")
	if len(calls) != 1 {
		t.Fatalf("got %d sendAudio calls, want 1", len(calls))
	}
	if len(calls[0].Files) != 1 {
		t.Fatalf("expected only the audio file to be uploaded, got parts %v", calls[0].Files)
	}
	if b.fileIDCache.Len() != 0 {
		t.Fatal("an upload without a thumbnail must not be cached")
	}
	if !fileGone(path) {
		t.Error("the file should still be deleted after a successful upload")
	}
}

func TestSendFilesCachesUploadsThatHaveAThumbnail(t *testing.T) {
	requireFFmpeg(t)
	_, withCover, cover := makeMedia(t)
	f := newFakeTG(t)
	b := newTestBot(t, f)

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: withCover, CoverPath: cover, TrackID: "888", Artist: "Artist", Song: "Song"}})

	calls := f.callsTo("sendAudio")
	if len(calls) != 1 {
		t.Fatalf("got %d sendAudio calls, want 1", len(calls))
	}
	if len(calls[0].Files) != 2 {
		t.Fatalf("expected the audio and its thumbnail to be uploaded, got parts %v", calls[0].Files)
	}
	if calls[0].Form.Get("performer") != "Artist" || calls[0].Form.Get("title") != "Song" {
		t.Fatalf("performer/title not set: %v", calls[0].Form)
	}

	entry, ok := b.fileIDCache.Get("888", ModeALAC)
	if !ok || entry.FileID != "NEWFID" || entry.Kind != "audio" {
		t.Fatalf("upload with a thumbnail should be cached, got %+v (found=%v)", entry, ok)
	}
}

func TestSendFilesSendsVideosAsDocuments(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	path := writeTrackFile(t, "clip.m4v", "not really a video")

	b.sendFiles(newJob(1, 100, 5, songURL, ModeMV, "u"),
		[]app.AddedTrack{{Path: path, TrackID: "111", Artist: "A", Song: "S"}})

	if len(f.callsTo("sendDocument")) != 1 || len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("a .m4v file must be sent as a document")
	}
}

func TestSendFilesFallsBackToUploadWhenCachedIDIsStale(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("999", ModeALAC, audioEntry("STALE"))
	f.setOverride(func(c tgCall) (int, string, bool) {
		if c.Method == "sendAudio" && c.Form.Get("audio") == "STALE" {
			return http.StatusBadRequest, staleFileIDJSON, true
		}
		return 0, "", false
	})
	path := writeTrackFile(t, "01. Song.m4a", "this is not real audio")

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: path, TrackID: "999", Artist: "A", Song: "S"}})

	calls := f.callsTo("sendAudio")
	if len(calls) != 2 {
		t.Fatalf("got %d sendAudio calls, want the rejected cached send plus an upload", len(calls))
	}
	if len(calls[1].Files) != 1 {
		t.Fatalf("the second attempt should upload the file, got parts %v", calls[1].Files)
	}
	if _, ok := b.fileIDCache.Get("999", ModeALAC); ok {
		t.Fatal("the rejected file_id should have been evicted")
	}
}

func TestSendFilesSkipsOversizedFiles(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f, func(c *BotConfig) { c.MaxFileSizeMB = 1 })
	path := writeTrackFile(t, "big.m4a", strings.Repeat("x", 2*1024*1024))

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: path, TrackID: "222", Artist: "A", Song: "S"}})

	if len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("a file over the limit must not be uploaded")
	}
	msgs := f.callsTo("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Form.Get("text"), "too large") {
		t.Fatalf("the user should be told the file is too large, got %+v", msgs)
	}
}

// ---- upload progress ---------------------------------------------------------------

func TestUploadSourceCountsBytesAndKeepsTheFileName(t *testing.T) {
	path := tempUpload(t, 100000)
	var lastSent, lastTotal int64
	src, closeSrc, err := uploadSource(context.Background(), path, func(sent, total int64) {
		lastSent, lastTotal = sent, total
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSrc()

	if !src.NeedsUpload() {
		t.Fatal("a file must be uploaded, not sent by id")
	}
	name, reader, err := src.UploadData()
	if err != nil {
		t.Fatal(err)
	}
	if name != "upload.zip" {
		t.Errorf("file name = %q, want the base name", name)
	}
	data, err := io.ReadAll(reader)
	if err != nil || len(data) != 100000 {
		t.Fatalf("read %d bytes (%v), want 100000", len(data), err)
	}
	if lastSent != 100000 || lastTotal != 100000 {
		t.Errorf("progress ended at %d of %d", lastSent, lastTotal)
	}
}

// A cancelled job must stop the transfer, not wait for the file to finish.
func TestUploadSourceStopsWhenTheJobIsCancelled(t *testing.T) {
	path := tempUpload(t, 100000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src, closeSrc, err := uploadSource(ctx, path, func(sent, total int64) { cancel() })
	if err != nil {
		t.Fatal(err)
	}
	defer closeSrc()

	_, reader, _ := src.UploadData()
	data, err := io.ReadAll(reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(data) >= 100000 {
		t.Fatal("the transfer should have stopped part way")
	}
}

func TestUploadSourceMissingFile(t *testing.T) {
	if _, _, err := uploadSource(context.Background(), filepath.Join(t.TempDir(), "gone.m4a"), nil); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestSendTracksShowsTheUploadAndEndsWithTheSummary(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "01. A.m4a"), filepath.Join(dir, "02. B.m4a")
	writeSized(t, first, 1<<20)
	writeSized(t, second, 2<<20)

	edits := &editLog{}
	up := NewUploadProgress(ModeALAC, edits.add)
	up.SetFinal("SUMMARY", 90*time.Second)

	b.sendTracks(newJob(1, 100, 5, songURL, ModeALAC, "u"),
		[]app.AddedTrack{{Path: first, TrackID: "1"}, {Path: second, TrackID: "2"}}, up)

	if got := len(f.callsTo("sendAudio")); got != 2 {
		t.Fatalf("got %d sendAudio calls, want 2", got)
	}
	texts := edits.all()
	last := texts[len(texts)-1]
	if !strings.HasPrefix(last, "Download complete in 1m 30s, upload in ") || !strings.HasSuffix(last, ":\n\nSUMMARY") {
		t.Fatalf("final text = %q", last)
	}
	if !edits.anyContains("Uploading to Telegram [alac]") {
		t.Fatal("the upload was never shown")
	}
	if up.doneFiles != 2 || up.doneBytes != 3<<20 {
		t.Fatalf("counted %d files and %d bytes, want 2 and %d", up.doneFiles, up.doneBytes, 3<<20)
	}
}

func TestSendTracksCountsAnUploadWhoseCachedCopyWentStale(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	b.fileIDCache.Set("9", ModeALAC, audioEntry("STALE"))
	f.setOverride(func(c tgCall) (int, string, bool) {
		if c.Method == "sendAudio" && c.Form.Get("audio") == "STALE" {
			return http.StatusBadRequest, staleFileIDJSON, true
		}
		return 0, "", false
	})
	path := writeTrackFile(t, "01. Song.m4a", "this is not real audio")

	up := NewUploadProgress(ModeALAC, nil)
	b.sendTracks(newJob(1, 100, 5, songURL, ModeALAC, "u"), []app.AddedTrack{{Path: path, TrackID: "9"}}, up)

	// It was cached, so the plan had no upload; the fallback added one.
	if up.files != 1 || up.doneFiles != 1 {
		t.Fatalf("files planned %d, finished %d; want 1 and 1", up.files, up.doneFiles)
	}
}

func TestSendTracksCancelledBeforeUploadingEndsWithCancelled(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)
	path := writeTrackFile(t, "01. Song.m4a", "x")
	edits := &editLog{}
	up := NewUploadProgress(ModeALAC, edits.add)
	job := newJob(1, 100, 5, songURL, ModeALAC, "u")
	job.cancel()

	b.sendTracks(job, []app.AddedTrack{{Path: path, TrackID: "1"}}, up)

	texts := edits.all()
	if got := texts[len(texts)-1]; got != "Cancelled while uploading: 0 of 1 file(s) were sent." {
		t.Fatalf("final text = %q", got)
	}
	if len(f.callsTo("sendAudio")) != 0 {
		t.Fatal("a cancelled job must not upload anything")
	}
}

// One message follows the job from the cache check to the end: the upload takes
// over from the download in the same message and the summary is written there.
func TestProcessJobShowsTheUploadInTheSameMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "01. Song.m4a")
	writeSized(t, path, 2<<20)
	t.Setenv("AMDL_TEST_FILE", path)
	useHelperWorker(t, "file")

	f := newFakeTG(t)
	b := newTestBot(t, f)
	job := newJob(1, 100, 5, songURL, ModeALAC, "alice")
	b.processJob(job)

	edits := f.callsTo("editMessageText")
	sawUpload := false
	for _, c := range edits {
		if c.Form.Get("message_id") != "5" {
			t.Fatalf("an edit went to message %q, want the job's message 5", c.Form.Get("message_id"))
		}
		if strings.Contains(c.Form.Get("text"), "Uploading to Telegram [alac]") {
			sawUpload = true
		}
	}
	if !sawUpload {
		t.Fatal("the upload was never shown in the message")
	}
	last := edits[len(edits)-1].Form.Get("text")
	mustContain(t, last, "Download complete in", ", upload in ", "A - B (1 track(s))")
	if len(f.callsTo("sendAudio")) != 1 {
		t.Fatalf("got %d sendAudio calls, want 1", len(f.callsTo("sendAudio")))
	}
	if job.Status != JobDone {
		t.Fatalf("job status = %q", job.Status)
	}
}
