package bot

// fileid_cache.go maps (Apple Music track ID, download mode) -> Telegram file_id.
//
// When a file is uploaded to Telegram for the first time, Telegram returns a
// file_id that permanently identifies that file on their servers. Subsequent
// sends can reference the file_id directly - no re-upload needed, no file on
// disk required, and no bandwidth used by the bot server.
//
// Cache key: "<trackID>:<mode>"  e.g. "1234567890:alac"
// Cache value: FileIDEntry holding the audio/document file_id and optionally
//              the thumbnail file_id.
//
// The cache is persisted to a JSON file so it survives bot restarts.

import (
	"encoding/json"
	"log"
	"sync"
)

// FileIDEntry holds the Telegram file IDs for a cached track.
type FileIDEntry struct {
	FileID  string `json:"file_id"`
	Kind    string `json:"kind"` // "audio" or "document": decides which send method a cached entry uses
	ThumbID string `json:"thumb_id,omitempty"` // legacy, no longer used
	Artist  string `json:"artist"`
	Song    string `json:"song"`
}

// FileIDCache is a thread-safe persistent map from cache key to FileIDEntry.
// Changes are written to disk shortly afterwards, atomically (see persist.go);
// Flush writes anything still waiting.
type FileIDCache struct {
	mu      sync.RWMutex
	entries map[string]FileIDEntry // key: "<trackID>:<mode>"
	p       *persister
}

// NewFileIDCache loads the cache from path. A missing file gives an empty cache;
// an unreadable one is moved aside (and logged) and also gives an empty cache.
func NewFileIDCache(path string) *FileIDCache {
	c := &FileIDCache{entries: make(map[string]FileIDEntry)}
	if path != "" {
		loaded, err := loadJSON(path, &c.entries)
		switch {
		case err != nil:
			log.Printf("fileid_cache: %v; starting with an empty cache", err)
			c.entries = make(map[string]FileIDEntry)
		case loaded:
			log.Printf("fileid_cache: loaded %d entries from %s", len(c.entries), path)
		}
	}
	if c.entries == nil { // the file held the JSON value null
		c.entries = make(map[string]FileIDEntry)
	}
	c.p = newPersister(path, c.snapshot)
	return c
}

// snapshot is the JSON the persister writes.
func (c *FileIDCache) snapshot() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return json.Marshal(c.entries)
}

// Flush writes any change that is still waiting to be saved.
func (c *FileIDCache) Flush() { c.p.Flush() }

// cacheSchema is bumped whenever the way files are uploaded changes in a way
// that makes previously cached file_ids undesirable (for example a bug that
// uploaded files with a wrong or missing thumbnail). Entries stored under an
// older schema are simply never matched again.
const cacheSchema = "v3"

// cacheKey builds the lookup key.
func cacheKey(trackID string, mode Mode) string {
	return cacheSchema + ":" + trackID + ":" + string(mode)
}

// Get returns the cached entry and true if found, otherwise zero value and false.
func (c *FileIDCache) Get(trackID string, mode Mode) (FileIDEntry, bool) {
	if trackID == "" {
		return FileIDEntry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[cacheKey(trackID, mode)]
	return e, ok
}

// Set stores an entry. It is saved to disk shortly afterwards.
func (c *FileIDCache) Set(trackID string, mode Mode, entry FileIDEntry) {
	if trackID == "" {
		return
	}
	key := cacheKey(trackID, mode)

	c.mu.Lock()
	old, existed := c.entries[key]
	c.entries[key] = entry
	c.mu.Unlock()

	if !existed || old != entry {
		c.p.markDirty()
	}
}

// Delete removes an entry (used when Telegram rejects a stored file_id).
func (c *FileIDCache) Delete(trackID string, mode Mode) {
	if trackID == "" {
		return
	}
	key := cacheKey(trackID, mode)

	c.mu.Lock()
	_, existed := c.entries[key]
	delete(c.entries, key)
	c.mu.Unlock()

	if existed {
		c.p.markDirty()
	}
}

// Len returns the number of cached entries.
func (c *FileIDCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
