package bot

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanerOnUploadedDeletesOnlyInOnUploadMode(t *testing.T) {
	cases := []struct {
		mode        string
		wantDeleted bool
	}{
		{"on_upload", true},
		{"never", false},
		{"disk_pressure", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.m4a")
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			NewCleaner(BotConfig{DeleteMode: tc.mode}).OnUploaded(path)

			_, err := os.Stat(path)
			if deleted := os.IsNotExist(err); deleted != tc.wantDeleted {
				t.Fatalf("mode %q: deleted=%v, want %v", tc.mode, deleted, tc.wantDeleted)
			}
		})
	}
}

func TestCleanerRecordsDeletionsInStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.m4a")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats := NewStats("")
	c := NewCleaner(BotConfig{DeleteMode: "on_upload"})
	c.SetStats(stats)
	c.OnUploaded(path)

	if stats.TotalDeleted != 1 {
		t.Fatalf("TotalDeleted = %d, want 1", stats.TotalDeleted)
	}
}

func TestCleanerIgnoresMissingFile(t *testing.T) {
	stats := NewStats("")
	c := NewCleaner(BotConfig{DeleteMode: "on_upload"})
	c.SetStats(stats)
	c.OnUploaded(filepath.Join(t.TempDir(), "never-existed.m4a"))

	if stats.TotalDeleted != 0 {
		t.Fatal("deleting a file that is already gone must not be counted")
	}
}

func TestRemoveEmptyDirsRemovesWholeEmptyChains(t *testing.T) {
	root := t.TempDir()
	mustMkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(filepath.Join("artist", "album")) // empty chain
	mustMkdir("keep")
	if err := os.WriteFile(filepath.Join(root, "keep", "song.m4a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeEmptyDirs(root)

	if _, err := os.Stat(filepath.Join(root, "artist")); !os.IsNotExist(err) {
		t.Error("artist/album is empty, so the whole artist directory should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "keep", "song.m4a")); err != nil {
		t.Error("a directory that still holds a file must be left alone")
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("the root directory itself must never be removed")
	}
}

func TestDiskUsagePctOnThisPlatform(t *testing.T) {
	pct, err := diskUsagePct(t.TempDir())
	if err != nil {
		t.Fatalf("diskUsagePct failed: %v", err)
	}
	if pct < 0 || pct > 100 {
		t.Fatalf("disk usage %.2f%% is outside [0, 100]", pct)
	}
}

// The app only creates AM-Atmos, AM-AAC and AM-MV on the first download of that
// kind. Checking a directory that does not exist yet used to log an error every
// few minutes.
func TestDiskPressureIgnoresDirectoriesThatDoNotExistYet(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "AM-Atmos") // never created
	c := NewCleaner(BotConfig{DeleteMode: "disk_pressure"})
	c.diskThresholdPct = -1 // any real directory would be cleaned
	c.SetWatchDirs([]string{missing})

	c.runDiskPressureCheck()
	c.runDiskPressureCheck()

	if len(c.lastErr) != 0 {
		t.Fatalf("a missing directory is not an error, but recorded %v", c.lastErr)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("the cleaner must not create the directory")
	}
}

// writeAged creates a file of the given size whose modification time is age in the past.
func writeAged(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// newPressureCleaner returns a cleaner for which the disk always counts as full,
// so only the safeguards decide what gets removed.
func newPressureCleaner(dirs ...string) *Cleaner {
	c := NewCleaner(BotConfig{DeleteMode: "disk_pressure"})
	c.diskThresholdPct = -1
	c.SetWatchDirs(dirs)
	return c
}

func TestDiskPressureStillCleansExistingDirectories(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "AM-Atmos")
	dir := t.TempDir()
	file := writeAged(t, dir, filepath.Join("album", "01. Song.m4a"), 10, 3*time.Hour)

	newPressureCleaner(missing, dir).runDiskPressureCheck()

	if !fileGone(file) {
		t.Fatal("a missing directory in the list must not stop the others from being cleaned")
	}
	if _, err := os.Stat(filepath.Join(dir, "album")); !os.IsNotExist(err) {
		t.Error("the now-empty album directory should be removed")
	}
}

// A file still being downloaded, or waiting for its upload, must survive no
// matter how full the disk is.
func TestDiskPressureNeverRemovesRecentFiles(t *testing.T) {
	dir := t.TempDir()
	old := writeAged(t, dir, "old.m4a", 10, 3*time.Hour)
	recent := writeAged(t, dir, "recent.m4a", 10, 5*time.Minute)
	justNow := writeAged(t, dir, "downloading.m4a", 10, 0)

	newPressureCleaner(dir).runDiskPressureCheck()

	if !fileGone(old) {
		t.Error("a file older than the minimum age should be removed")
	}
	if fileGone(recent) || fileGone(justNow) {
		t.Fatal("files younger than the minimum age must never be removed")
	}
}

func TestDiskPressureReportsWhenNothingOldEnoughIsLeft(t *testing.T) {
	var out strings.Builder
	orig := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(orig) })

	dir := t.TempDir()
	writeAged(t, dir, "recent.m4a", 10, 5*time.Minute)
	c := newPressureCleaner(dir)

	c.runDiskPressureCheck()
	c.runDiskPressureCheck()
	c.runDiskPressureCheck()

	if got := strings.Count(out.String(), "nothing else can be removed"); got != 1 {
		t.Fatalf("the \"nothing left to remove\" note appeared %d times, want exactly 1:\n%s", got, out.String())
	}
}

// The limit stops one check from deleting everything; the next check carries on.
func TestDiskPressureStopsAtThePerCheckLimit(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := 0; i < 5; i++ { // oldest first
		files = append(files, writeAged(t, dir, "song"+string(rune('a'+i))+".m4a", 1000, time.Duration(10-i)*time.Hour))
	}
	c := newPressureCleaner(dir)
	c.maxDeleteBytes = 2500

	c.runDiskPressureCheck()

	// 1000 + 1000 < 2500, so a third file is still taken, which reaches 3000.
	for i, f := range files {
		if gone := fileGone(f); gone != (i < 3) {
			t.Errorf("after the first check file %d gone=%v, want %v", i, gone, i < 3)
		}
	}

	c.runDiskPressureCheck() // a new check has a fresh allowance
	for i, f := range files {
		if !fileGone(f) {
			t.Errorf("after the second check file %d should be gone too", i)
		}
	}
}

// The allowance belongs to the whole check, not to each directory.
func TestDiskPressureLimitIsSharedAcrossDirectories(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	a1 := writeAged(t, first, "a1.m4a", 1000, 5*time.Hour)
	a2 := writeAged(t, first, "a2.m4a", 1000, 4*time.Hour)
	b1 := writeAged(t, second, "b1.m4a", 1000, 5*time.Hour)
	b2 := writeAged(t, second, "b2.m4a", 1000, 4*time.Hour)

	c := newPressureCleaner(first, second)
	c.maxDeleteBytes = 1500
	c.runDiskPressureCheck()

	if !fileGone(a1) || !fileGone(a2) {
		t.Error("the first directory should use up the allowance")
	}
	if fileGone(b1) || fileGone(b2) {
		t.Error("the second directory must be left alone once the allowance is spent")
	}
}

func TestNewCleanerSafetyLimitsAndDefaults(t *testing.T) {
	c := NewCleaner(BotConfig{DiskMinAgeMin: 10, DiskMaxDeleteMB: 5})
	if c.minAge != 10*time.Minute || c.maxDeleteBytes != 5*1024*1024 {
		t.Errorf("configured limits not applied: minAge=%v maxDeleteBytes=%d", c.minAge, c.maxDeleteBytes)
	}

	d := NewCleaner(BotConfig{})
	if d.minAge != time.Hour || d.maxDeleteBytes != 2048*1024*1024 {
		t.Errorf("defaults wrong: minAge=%v maxDeleteBytes=%d, want 1h and 2048 MB", d.minAge, d.maxDeleteBytes)
	}
}

func TestCleanerReportsAPersistentErrorOnlyOnce(t *testing.T) {
	var out strings.Builder
	orig := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(orig) })

	c := NewCleaner(BotConfig{DeleteMode: "disk_pressure"})
	c.reportError("dir", &testErr{"access denied"})
	c.reportError("dir", &testErr{"access denied"}) // same error at the next check
	if got := strings.Count(out.String(), "cannot check disk"); got != 1 {
		t.Fatalf("a persistent error was logged %d times, want 1:\n%s", got, out.String())
	}

	c.reportError("dir", &testErr{"disk offline"}) // a different problem deserves a new line
	if got := strings.Count(out.String(), "cannot check disk"); got != 2 {
		t.Fatalf("a changed error should be logged again, got %d lines", got)
	}
}
