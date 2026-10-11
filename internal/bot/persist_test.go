package bot

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withPersistTiming sets how long persisters created from now on wait before
// writing, and how long they wait before retrying a failed write.
func withPersistTiming(t *testing.T, delay, retry time.Duration) {
	t.Helper()
	oldDelay, oldRetry := persistDelay, persistRetry
	persistDelay, persistRetry = delay, retry
	t.Cleanup(func() { persistDelay, persistRetry = oldDelay, oldRetry })
}

// countingSnapshot returns a snapshot function whose output names how many
// snapshots have been taken, and the counter itself.
func countingSnapshot() (func() ([]byte, error), *int32) {
	var n int32
	return func() ([]byte, error) {
		v := atomic.AddInt32(&n, 1)
		return []byte(fmt.Sprintf(`{"version":%d}`, v)), nil
	}, &n
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// ---- atomic writes ------------------------------------------------------------

func TestWriteFileAtomicReplacesTheFileAndLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := writeFileAtomic(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(path); string(data) != "two" {
		t.Fatalf("file holds %q, want the second write", data)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "state.json" {
		t.Fatalf("directory holds %q, want only state.json", names)
	}
}

func TestWriteFileAtomicFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	// A directory sits where the file belongs, so the final rename cannot succeed.
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(target, []byte("x"), 0o644); err == nil {
		t.Fatal("replacing a directory with a file must fail")
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatal("the existing entry must be untouched")
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("a failed write left %q behind", names)
	}
}

// ---- loading --------------------------------------------------------------------

func TestLoadJSONMissingAndValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var v map[string]int

	loaded, err := loadJSON(path, &v)
	if loaded || err != nil {
		t.Fatalf("a missing file is not an error: loaded=%v err=%v", loaded, err)
	}

	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadJSON(path, &v)
	if !loaded || err != nil || v["a"] != 1 {
		t.Fatalf("valid file: loaded=%v err=%v v=%v", loaded, err, v)
	}
}

// Overwriting an unreadable file with an empty state would destroy whatever is
// still in it; it is moved aside instead.
func TestLoadJSONMovesACorruptFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	var v map[string]int
	loaded, err := loadJSON(path, &v)
	if loaded || err == nil || !strings.Contains(err.Error(), "moved to") {
		t.Fatalf("expected an error naming the new location, got loaded=%v err=%v", loaded, err)
	}
	if fileExists(path) {
		t.Fatal("the unreadable file must no longer sit at the original path")
	}

	var aside string
	for _, name := range dirNames(t, dir) {
		if strings.HasPrefix(name, "state.json.corrupt-") {
			aside = filepath.Join(dir, name)
		}
	}
	if aside == "" {
		t.Fatalf("no .corrupt- file in %q", dirNames(t, dir))
	}
	if data, _ := os.ReadFile(aside); string(data) != "{broken" {
		t.Fatalf("the moved file was altered: %q", data)
	}
}

func TestLoadJSONRemovesStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	stale := path + ".tmp-12345"
	unrelated := filepath.Join(dir, "other.json.tmp-1")
	for _, p := range []string{stale, unrelated} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var v map[string]int
	if _, err := loadJSON(path, &v); err != nil {
		t.Fatal(err)
	}

	if fileExists(stale) {
		t.Error("a temporary file left by an interrupted write should be removed")
	}
	if !fileExists(unrelated) {
		t.Error("another file's temporary must not be touched")
	}
}

// ---- the persister ----------------------------------------------------------------

func TestPersisterCoalescesABurstIntoOneWrite(t *testing.T) {
	withPersistTiming(t, 300*time.Millisecond, time.Hour)
	path := filepath.Join(t.TempDir(), "state.json")
	snap, n := countingSnapshot()
	p := newPersister(path, snap)

	for i := 0; i < 50; i++ {
		p.markDirty()
	}
	if fileExists(path) {
		t.Fatal("nothing should be written before the delay has passed")
	}

	waitFor(t, "the delayed write", func() bool { return fileExists(path) })
	time.Sleep(450 * time.Millisecond) // long enough for a second write to show up
	if got := atomic.LoadInt32(n); got != 1 {
		t.Fatalf("a burst of 50 changes caused %d writes, want 1", got)
	}
}

func TestPersisterWritesTheLatestStateWhenChangesContinue(t *testing.T) {
	withPersistTiming(t, 50*time.Millisecond, time.Hour)
	path := filepath.Join(t.TempDir(), "state.json")
	snap, _ := countingSnapshot()
	p := newPersister(path, snap)

	content := func() string {
		data, _ := os.ReadFile(path)
		return string(data)
	}

	p.markDirty()
	waitFor(t, "the first write", func() bool { return content() == `{"version":1}` })
	p.markDirty()
	waitFor(t, "a second write after a later change", func() bool { return content() == `{"version":2}` })
}

func TestPersisterFlushWritesAPendingChangeNow(t *testing.T) {
	withPersistTiming(t, time.Hour, time.Hour)
	path := filepath.Join(t.TempDir(), "state.json")
	snap, n := countingSnapshot()
	p := newPersister(path, snap)

	p.markDirty()
	if fileExists(path) {
		t.Fatal("the write is scheduled an hour away")
	}
	p.Flush()
	if data, _ := os.ReadFile(path); string(data) != `{"version":1}` {
		t.Fatalf("Flush did not write the pending change, file = %q", data)
	}

	p.Flush() // nothing changed since: no second write
	if got := atomic.LoadInt32(n); got != 1 {
		t.Fatalf("Flush with nothing pending took %d snapshots, want 1", got)
	}
}

func TestPersisterRetriesAfterAFailedWriteAndLogsOnce(t *testing.T) {
	withPersistTiming(t, 0, 20*time.Millisecond)
	var logs lockedBuffer
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })

	dir := filepath.Join(t.TempDir(), "not-created-yet")
	path := filepath.Join(dir, "state.json")
	snap, _ := countingSnapshot()
	p := newPersister(path, snap)

	p.markDirty() // writes at once, and fails: the directory does not exist
	if fileExists(path) {
		t.Fatal("the write cannot have succeeded")
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the retry to succeed", func() bool { return fileExists(path) })
	if got := strings.Count(logs.String(), "cannot save"); got != 1 {
		t.Fatalf("the failure was logged %d times, want once:\n%s", got, logs.String())
	}
}

func TestNilPersisterDoesNothing(t *testing.T) {
	var p *persister
	p.markDirty()
	p.Flush()

	if newPersister("", func() ([]byte, error) { return nil, nil }) != nil {
		t.Fatal("a persister without a path should be nil")
	}
}
