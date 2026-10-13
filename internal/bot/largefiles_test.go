package bot

import (
	"amdl/internal/app"
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// isolateTemp points the temporary directory at a fresh, empty one and returns
// it, so a test can check that nothing is left behind.
func isolateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", size)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isEmptyDir(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries) == 0
}

// ---- the archive ---------------------------------------------------------------

func TestBuildStoredZipStoresWithoutCompression(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "Album", "01. One.m4a")
	b := filepath.Join(root, "Album", "02. Two.m4a")
	writeSized(t, a, 150000) // very compressible, so compression would show
	writeSized(t, b, 90000)

	zipPath, name, err := buildStoredZip(context.Background(), []app.AddedTrack{
		{Path: a, Artist: "Artist", Album: "Album"},
		{Path: b, Artist: "Artist", Album: "Album"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipPath)

	if name != "Artist - Album.zip" {
		t.Errorf("name = %q", name)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	if len(zr.File) != 2 {
		t.Fatalf("archive has %d entries, want 2", len(zr.File))
	}
	want := map[string]int{"Album/01. One.m4a": 150000, "Album/02. Two.m4a": 90000}
	for _, f := range zr.File {
		size, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected entry %q", f.Name)
			continue
		}
		if f.Method != zip.Store {
			t.Errorf("%s uses method %d, want stored (%d)", f.Name, f.Method, zip.Store)
		}
		if f.CompressedSize64 != f.UncompressedSize64 || f.UncompressedSize64 != uint64(size) {
			t.Errorf("%s: compressed %d, uncompressed %d, want both %d", f.Name, f.CompressedSize64, f.UncompressedSize64, size)
		}
	}
}

func TestBuildStoredZipKeepsUnicodeNames(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Mân Côi - Single", "01. Mân Côi.m4a")
	writeSized(t, p, 100)

	zipPath, _, err := buildStoredZip(context.Background(), []app.AddedTrack{{Path: p}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if got := zr.File[0].Name; got != "Mân Côi - Single/01. Mân Côi.m4a" {
		t.Fatalf("entry name = %q", got)
	}
}

func TestBuildStoredZipNumbersDuplicateNames(t *testing.T) {
	root := t.TempDir()
	x := filepath.Join(root, "x", "Album", "01.m4a")
	y := filepath.Join(root, "y", "Album", "01.m4a")
	writeSized(t, x, 10)
	writeSized(t, y, 10)

	zipPath, _, err := buildStoredZip(context.Background(), []app.AddedTrack{{Path: x}, {Path: y}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != 2 || zr.File[0].Name != "Album/01.m4a" || zr.File[1].Name != "Album/01 (2).m4a" {
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		t.Fatalf("entries = %q", names)
	}
}

func TestBuildStoredZipLeavesNothingBehindOnFailure(t *testing.T) {
	tmp := isolateTemp(t)

	if _, _, err := buildStoredZip(context.Background(), []app.AddedTrack{{Path: filepath.Join(t.TempDir(), "gone.m4a")}}); err == nil {
		t.Fatal("a missing source file must be an error")
	}
	if !isEmptyDir(t, tmp) {
		t.Fatal("a failed build must remove its temporary archive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := filepath.Join(t.TempDir(), "a.m4a")
	writeSized(t, p, 10)
	if _, _, err := buildStoredZip(ctx, []app.AddedTrack{{Path: p}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if !isEmptyDir(t, tmp) {
		t.Fatal("a cancelled build must remove its temporary archive")
	}
}

func TestBuildStoredZipRejectsAnEmptyList(t *testing.T) {
	if _, _, err := buildStoredZip(context.Background(), nil); err == nil {
		t.Fatal("zipping nothing should be an error")
	}
}

func TestZipFileName(t *testing.T) {
	cases := []struct {
		name   string
		tracks []app.AddedTrack
		want   string
	}{
		{"one album", []app.AddedTrack{{Artist: "Linh Cao", Album: "Mân Côi - Single"}, {Artist: "Linh Cao", Album: "Mân Côi - Single"}}, "Linh Cao - Mân Côi - Single.zip"},
		{"several albums", []app.AddedTrack{{Artist: "A", Album: "X"}, {Artist: "A", Album: "Y"}}, "amdl-download.zip"},
		{"several artists", []app.AddedTrack{{Artist: "A", Album: "X"}, {Artist: "B", Album: "X"}}, "amdl-download.zip"},
		{"unsafe characters", []app.AddedTrack{{Artist: "AC/DC", Album: "Back in Black?"}}, "AC_DC - Back in Black_.zip"},

		// Music video: single track, no album — named after the song.
		{"mv with artist and song", []app.AddedTrack{{Artist: "Sơn Tùng M-TP", Song: "Muộn Rồi Mà Sao Còn"}}, "Sơn Tùng M-TP - Muộn Rồi Mà Sao Còn.zip"},
		{"mv song only no artist", []app.AddedTrack{{Song: "Dynamite"}}, "Dynamite.zip"},
		{"mv artist only no song", []app.AddedTrack{{Artist: "BTS"}}, "amdl-download.zip"},
		{"mv all empty", []app.AddedTrack{{}}, "amdl-download.zip"},
		{"mv unsafe chars in song", []app.AddedTrack{{Artist: "AC/DC", Song: "Back in Black?"}}, "AC_DC - Back in Black_.zip"},

		// Single track that does have an album should still use artist+album.
		{"single track with album", []app.AddedTrack{{Artist: "A", Album: "X", Song: "Y"}}, "A - X.zip"},
	}
	for _, tc := range cases {
		if got := zipFileName(tc.tracks); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName(`a/b\c:d*e?f"g<h>i|j`); got != "a_b_c_d_e_f_g_h_i_j" {
		t.Errorf("got %q", got)
	}
	if got := safeFileName("  ..  "); got != "" {
		t.Errorf("only spaces and dots should leave nothing, got %q", got)
	}
	if got := safeFileName("tab\there"); got != "tabhere" {
		t.Errorf("control characters should be dropped, got %q", got)
	}
	if got := []rune(safeFileName(strings.Repeat("a", 150))); len(got) != 100 {
		t.Errorf("long names are cut to 100 characters, got %d", len(got))
	}
}

// ---- delivering large files ----------------------------------------------------

// fakeUploader stands in for a file host. It opens the archive it is given so
// the test can see what would have been uploaded.
type fakeUploader struct {
	mu      sync.Mutex
	calls   int
	name    string
	entries []string
	methods []uint16
	url     string
	err     error
	started chan struct{} // closed when Upload begins, if set
	block   bool          // wait for ctx to end instead of finishing
}

func (f *fakeUploader) Name() string { return "FakeHost" }

func (f *fakeUploader) Upload(ctx context.Context, path, name string, progress func(sent, total int64)) (string, error) {
	f.mu.Lock()
	f.calls++
	f.name = name
	f.mu.Unlock()

	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	for _, e := range zr.File {
		f.entries = append(f.entries, e.Name)
		f.methods = append(f.methods, e.Method)
	}
	f.mu.Unlock()
	zr.Close()

	if f.started != nil {
		close(f.started)
	}
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if progress != nil {
		progress(10, 10)
	}
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

func allTexts(f *fakeTG) string {
	var sb strings.Builder
	for _, m := range []string{"sendMessage", "editMessageText"} {
		for _, c := range f.callsTo(m) {
			sb.WriteString(c.Form.Get("text"))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// largeTracks creates one small and two oversized tracks of one album (limit 1 MB).
func largeTracks(t *testing.T) (small, big1, big2 app.AddedTrack) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Album")
	mk := func(name string, size int, id string) app.AddedTrack {
		p := filepath.Join(dir, name)
		writeSized(t, p, size)
		return app.AddedTrack{Path: p, TrackID: id, Artist: "Artist", Album: "Album", Song: name}
	}
	return mk("01. Small.m4a", 10, "1"), mk("02. Big.m4a", 2*1024*1024, "2"), mk("03. Bigger.m4a", 3*1024*1024, "3")
}

func largeFilesBot(t *testing.T, f *fakeTG, up Uploader) *Bot {
	t.Helper()
	b := newTestBot(t, f, func(c *BotConfig) {
		c.MaxFileSizeMB = 1
		c.DeleteMode = "on_upload"
	})
	b.uploader = up
	return b
}

func TestSendFilesZipsOversizedTracksAndSendsTheLink(t *testing.T) {
	tmp := isolateTemp(t)
	small, big1, big2 := largeTracks(t)
	f := newFakeTG(t)
	up := &fakeUploader{url: "https://gofile.io/d/abc"}
	b := largeFilesBot(t, f, up)

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"), []app.AddedTrack{small, big1, big2})

	if got := len(f.callsTo("sendAudio")); got != 1 {
		t.Fatalf("the small track should still go through Telegram, got %d sendAudio calls", got)
	}
	if up.calls != 1 {
		t.Fatalf("the host was called %d times, want once for all large files together", up.calls)
	}
	if up.name != "Artist - Album.zip" {
		t.Errorf("uploaded as %q", up.name)
	}
	if len(up.entries) != 2 || up.entries[0] != "Album/02. Big.m4a" || up.entries[1] != "Album/03. Bigger.m4a" {
		t.Errorf("the archive should hold exactly the two large tracks, got %q", up.entries)
	}
	for _, m := range up.methods {
		if m != zip.Store {
			t.Errorf("an entry is compressed (method %d); it must be stored", m)
		}
	}
	if !strings.Contains(allTexts(f), "https://gofile.io/d/abc") {
		t.Fatalf("the user never received the link:\n%s", allTexts(f))
	}
	for _, tr := range []app.AddedTrack{small, big1, big2} {
		if !fileGone(tr.Path) {
			t.Errorf("%s should be deleted once delivered (on_upload mode)", filepath.Base(tr.Path))
		}
	}
	if !isEmptyDir(t, tmp) {
		t.Error("the temporary archive must be removed after the upload")
	}
}

func TestSendFilesKeepsLargeFilesWhenTheUploadFails(t *testing.T) {
	tmp := isolateTemp(t)
	_, big1, big2 := largeTracks(t)
	f := newFakeTG(t)
	up := &fakeUploader{err: errors.New("host is down")}
	b := largeFilesBot(t, f, up)

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"), []app.AddedTrack{big1, big2})

	texts := allTexts(f)
	if !strings.Contains(texts, "failed") || !strings.Contains(texts, "host is down") {
		t.Fatalf("the user should be told why it failed:\n%s", texts)
	}
	if strings.Contains(texts, "https://") {
		t.Fatal("no link may be sent when the upload failed")
	}
	if fileGone(big1.Path) || fileGone(big2.Path) {
		t.Fatal("files that were not delivered must not be deleted")
	}
	if !isEmptyDir(t, tmp) {
		t.Error("the temporary archive must be removed even when the upload fails")
	}
}

func TestSendFilesWithoutAHostStillReportsLargeFiles(t *testing.T) {
	_, big1, _ := largeTracks(t)
	f := newFakeTG(t)
	b := largeFilesBot(t, f, nil)

	b.sendFiles(newJob(1, 100, 5, songURL, ModeALAC, "u"), []app.AddedTrack{big1})

	if !strings.Contains(allTexts(f), "too large") {
		t.Fatalf("the user should be told the file is too large:\n%s", allTexts(f))
	}
	if fileGone(big1.Path) {
		t.Fatal("an undelivered file must not be deleted")
	}
}

func TestSendFilesCancelDuringUploadKeepsEverything(t *testing.T) {
	tmp := isolateTemp(t)
	_, big1, _ := largeTracks(t)
	f := newFakeTG(t)
	up := &fakeUploader{block: true, started: make(chan struct{})}
	b := largeFilesBot(t, f, up)
	job := newJob(1, 100, 5, songURL, ModeALAC, "u")

	done := make(chan struct{})
	go func() {
		b.sendFiles(job, []app.AddedTrack{big1})
		close(done)
	}()
	<-up.started
	job.cancel()
	<-done

	if !strings.Contains(allTexts(f), "Cancelled.") {
		t.Fatalf("the user should see the job was cancelled:\n%s", allTexts(f))
	}
	if fileGone(big1.Path) {
		t.Fatal("a cancelled upload must not delete the file")
	}
	if !isEmptyDir(t, tmp) {
		t.Error("the temporary archive must be removed after a cancel")
	}
}
