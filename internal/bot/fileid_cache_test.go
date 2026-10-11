package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileIDCacheSetGetDelete(t *testing.T) {
	c := NewFileIDCache(filepath.Join(t.TempDir(), "c.json"))
	if _, ok := c.Get("42", ModeALAC); ok {
		t.Fatal("an empty cache must miss")
	}

	c.Set("42", ModeALAC, FileIDEntry{FileID: "F", Kind: "audio", Artist: "A", Song: "S"})
	e, ok := c.Get("42", ModeALAC)
	if !ok || e.FileID != "F" || e.Kind != "audio" || e.Artist != "A" || e.Song != "S" {
		t.Fatalf("unexpected entry %+v (found=%v)", e, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}

	c.Delete("42", ModeALAC)
	if _, ok := c.Get("42", ModeALAC); ok {
		t.Fatal("entry should be gone after Delete")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestFileIDCacheSeparatesModes(t *testing.T) {
	c := NewFileIDCache(filepath.Join(t.TempDir(), "c.json"))
	c.Set("42", ModeALAC, FileIDEntry{FileID: "LOSSLESS"})
	if _, ok := c.Get("42", ModeAtmos); ok {
		t.Fatal("an ALAC upload must not satisfy an Atmos request")
	}
	if _, ok := c.Get("42", ModeAAC); ok {
		t.Fatal("an ALAC upload must not satisfy an AAC request")
	}
}

func TestFileIDCacheIgnoresEmptyTrackID(t *testing.T) {
	c := NewFileIDCache(filepath.Join(t.TempDir(), "c.json"))
	c.Set("", ModeALAC, FileIDEntry{FileID: "X"})
	if c.Len() != 0 {
		t.Fatal("an entry without a track ID must not be stored")
	}
	if _, ok := c.Get("", ModeALAC); ok {
		t.Fatal("an empty track ID must never hit")
	}
}

func TestFileIDCachePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	NewFileIDCache(path).Set("42", ModeAtmos, FileIDEntry{FileID: "F", Kind: "document"})

	e, ok := NewFileIDCache(path).Get("42", ModeAtmos)
	if !ok || e.FileID != "F" || e.Kind != "document" {
		t.Fatalf("entry did not survive a reload: %+v (found=%v)", e, ok)
	}
}

func TestFileIDCacheIgnoresOldSchemaKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	old := `{"42:alac":{"file_id":"OLD"},"v2:42:alac":{"file_id":"OLDER"}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := NewFileIDCache(path).Get("42", ModeALAC); ok {
		t.Fatal("entries stored under an older schema must be ignored")
	}
}

func TestFileIDCacheSurvivesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewFileIDCache(path)
	if c.Len() != 0 {
		t.Fatal("a corrupt file should give an empty cache")
	}
	c.Set("42", ModeALAC, FileIDEntry{FileID: "F"})
	if _, ok := NewFileIDCache(path).Get("42", ModeALAC); !ok {
		t.Fatal("the cache should be writable again after a corrupt file")
	}
}

func TestCacheKeyCarriesSchemaTrackAndMode(t *testing.T) {
	key := cacheKey("42", ModeAtmos)
	if !strings.HasPrefix(key, cacheSchema+":") || !strings.HasSuffix(key, ":42:atmos") {
		t.Fatalf("unexpected key %q", key)
	}
}

func TestFileIDCacheWritesAreDelayedButFlushable(t *testing.T) {
	withPersistTiming(t, time.Hour, time.Hour)
	path := filepath.Join(t.TempDir(), "c.json")
	c := NewFileIDCache(path)

	c.Set("42", ModeALAC, FileIDEntry{FileID: "F"})
	if _, ok := NewFileIDCache(path).Get("42", ModeALAC); ok {
		t.Fatal("a change should not be on disk before the delay has passed")
	}

	c.Flush()
	if _, ok := NewFileIDCache(path).Get("42", ModeALAC); !ok {
		t.Fatal("Flush must write a change that is still waiting")
	}
}

func TestFileIDCacheSavesManyChangesTogether(t *testing.T) {
	withPersistTiming(t, 50*time.Millisecond, time.Hour)
	path := filepath.Join(t.TempDir(), "c.json")
	c := NewFileIDCache(path)

	for i := 0; i < 300; i++ {
		c.Set(fmt.Sprint(i), ModeALAC, FileIDEntry{FileID: "F"})
	}
	waitFor(t, "all 300 entries on disk", func() bool { return NewFileIDCache(path).Len() == 300 })
}

// A change that changes nothing must not cost a write.
func TestFileIDCacheSkipsWritesWhenNothingChanged(t *testing.T) {
	withPersistTiming(t, time.Hour, time.Hour)
	path := filepath.Join(t.TempDir(), "c.json")
	c := NewFileIDCache(path)
	entry := FileIDEntry{FileID: "F", Kind: "audio"}

	c.Set("42", ModeALAC, entry)
	c.Flush()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	c.Set("42", ModeALAC, entry)      // the same entry again
	c.Delete("missing", ModeALAC)     // deleting what is not there
	c.Flush()
	if fileExists(path) {
		t.Fatal("neither call changed the cache, so nothing should have been written")
	}

	c.Set("42", ModeALAC, FileIDEntry{FileID: "OTHER"}) // a real change
	c.Flush()
	if !fileExists(path) {
		t.Fatal("a real change must be written")
	}
}

func TestFileIDCacheNullFileGivesAWorkingCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewFileIDCache(path)
	c.Set("42", ModeALAC, FileIDEntry{FileID: "F"}) // must not panic on a nil map
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
}

func TestFileIDCacheKeepsACorruptFileForRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	if NewFileIDCache(path).Len() != 0 {
		t.Fatal("an unreadable file gives an empty cache")
	}

	found := false
	for _, name := range dirNames(t, dir) {
		if strings.HasPrefix(name, "c.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unreadable file should have been kept, directory holds %q", dirNames(t, dir))
	}
}
