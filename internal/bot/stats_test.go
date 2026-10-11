package bot

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatsPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	s := NewStats(path)
	s.RecordSuccess(1, "alice", 3, 1024)
	s.RecordFailure(1, "alice")
	s.RecordCacheHits(2, "bob", 2)
	s.RecordDeleted(4)

	r := NewStats(path)
	if r.TotalTracks != 3 || r.TotalFailures != 1 || r.TotalCacheHits != 2 ||
		r.TotalDeleted != 4 || r.TotalBytesRead != 1024 {
		t.Fatalf("counters lost on reload: tracks=%d failures=%d cache=%d deleted=%d bytes=%d",
			r.TotalTracks, r.TotalFailures, r.TotalCacheHits, r.TotalDeleted, r.TotalBytesRead)
	}
	u := r.ByUser[1]
	if u == nil || u.Username != "alice" || u.Tracks != 3 || u.Failures != 1 {
		t.Fatalf("per-user stats lost on reload: %+v", u)
	}
}

func TestStatsCacheHitsIgnoreNonPositive(t *testing.T) {
	s := NewStats("")
	s.RecordCacheHits(1, "a", 0)
	s.RecordCacheHits(1, "a", -3)
	if s.TotalCacheHits != 0 || len(s.ByUser) != 0 {
		t.Fatalf("non-positive hit counts must be ignored: %d hits, %d users", s.TotalCacheHits, len(s.ByUser))
	}
}

func TestStatsSummary(t *testing.T) {
	s := NewStats("")
	s.RecordSuccess(1, "alice", 3, 2048)
	s.RecordFailure(2, "bob")
	s.RecordCacheHits(1, "alice", 2)

	out := s.Summary()
	for _, want := range []string{
		"Tracks downloaded: 3",
		"Served from cache: 2",
		"Failed jobs: 1",
		"Data served: 2.0 KB",
		"alice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:               "0 B",
		1023:            "1023 B",
		1024:            "1.0 KB",
		1536:            "1.5 KB",
		5 * 1024 * 1024: "5.0 MB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestStatsWritesAreDelayedButFlushable(t *testing.T) {
	withPersistTiming(t, time.Hour, time.Hour)
	path := filepath.Join(t.TempDir(), "stats.json")
	s := NewStats(path)

	s.RecordFailure(1, "alice")
	if NewStats(path).TotalFailures != 0 {
		t.Fatal("a change should not be on disk before the delay has passed")
	}

	s.Flush()
	if NewStats(path).TotalFailures != 1 {
		t.Fatal("Flush must write a change that is still waiting")
	}
}

// The counters used to be reset to zero, silently, when the file was unreadable.
func TestStatsKeepsACorruptFileForRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(path, []byte(`{"total_tracks": 99,`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStats(path)
	if s.TotalTracks != 0 {
		t.Fatalf("an unreadable file starts from zero, got %d", s.TotalTracks)
	}

	found := false
	for _, name := range dirNames(t, dir) {
		if strings.HasPrefix(name, "stats.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unreadable file should have been kept, directory holds %q", dirNames(t, dir))
	}
}

func TestStatsNullUserMapDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path, []byte(`{"total_tracks":3,"by_user":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStats(path)
	s.RecordFailure(1, "alice") // must not write into a nil map
	if s.TotalTracks != 3 || s.TotalFailures != 1 {
		t.Fatalf("tracks=%d failures=%d", s.TotalTracks, s.TotalFailures)
	}
}

// Updates from many goroutines while the background save runs: nothing may be
// lost and the race detector must stay quiet.
func TestStatsConcurrentUpdatesAreAllSaved(t *testing.T) {
	withPersistTiming(t, 5*time.Millisecond, time.Hour)
	path := filepath.Join(t.TempDir(), "stats.json")
	s := NewStats(path)

	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				s.RecordSuccess(1, "u", 1, 10)
				s.RecordCacheHits(1, "u", 1)
			}
		}()
	}
	wg.Wait()
	s.Flush()

	r := NewStats(path)
	if r.TotalTracks != 500 || r.TotalCacheHits != 500 || r.TotalBytesRead != 5000 {
		t.Fatalf("saved tracks=%d cacheHits=%d bytes=%d, want 500, 500, 5000",
			r.TotalTracks, r.TotalCacheHits, r.TotalBytesRead)
	}
}
