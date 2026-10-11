package bot

import (
	"amdl/internal/app"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
