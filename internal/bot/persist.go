package bot

// persist.go saves the bot's small JSON state files (the file_id cache and the
// statistics) so that a crash can not corrupt them and bursts of changes do not
// rewrite them over and over.
//
//   - Writes are atomic: the data goes to a temporary file in the same directory,
//     is flushed to disk, and replaces the real file with a rename. A crash leaves
//     either the old file or the new one, never half of one.
//   - Writes are coalesced: the first change schedules a write persistDelay later,
//     and every change made in the meantime rides along. A hard kill can therefore
//     lose up to persistDelay of changes; a normal shutdown calls Flush and loses none.
//   - A file that cannot be parsed is moved aside instead of being overwritten.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// persistDelay is how long a change waits for further changes before the state
// file is written; 0 writes immediately. persistRetry is how long to wait before
// trying again after a failed write. Tests replace them.
var (
	persistDelay = 2 * time.Second
	persistRetry = 30 * time.Second
)

// writeFileAtomic replaces the file at path with data, so a crash leaves either
// the old contents or the new ones.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil { // the data must be on disk before the rename publishes it
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	_ = os.Chmod(name, perm) // CreateTemp makes the file private (0600)
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// removeStaleTemps deletes temporary files left by a write that a crash cut short.
func removeStaleTemps(path string) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".tmp-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// loadJSON reads the JSON file at path into v. A missing file is not an error.
// A file that cannot be parsed is renamed to <path>.corrupt-<time> rather than
// left to be overwritten by the next save, so whatever it still holds can be
// recovered by hand; the returned error says where it went. In that case nothing
// was loaded and v is still empty.
func loadJSON(path string, v any) (loaded bool, err error) {
	removeStaleTemps(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
		if renameErr := os.Rename(path, aside); renameErr != nil {
			return false, fmt.Errorf("%s is unreadable (%v) and could not be moved aside (%v)", path, err, renameErr)
		}
		return false, fmt.Errorf("%s is unreadable (%v); it was moved to %s", path, err, aside)
	}
	return true, nil
}

// persister writes one state file. Its owner calls markDirty after every change
// (without holding its own lock: snapshot takes that lock) and Flush on shutdown.
// A nil *persister does nothing, which is what an owner without a file uses.
type persister struct {
	path     string
	snapshot func() ([]byte, error) // JSON of the owner's current state
	delay    time.Duration
	retry    time.Duration

	writeMu sync.Mutex // one write at a time

	mu      sync.Mutex // guards the fields below
	dirty   bool
	timer   *time.Timer
	lastErr string
}

func newPersister(path string, snapshot func() ([]byte, error)) *persister {
	if path == "" {
		return nil
	}
	return &persister{path: path, snapshot: snapshot, delay: persistDelay, retry: persistRetry}
}

// markDirty records that the state changed and makes sure a write follows.
func (p *persister) markDirty() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.dirty = true
	immediate := p.delay <= 0
	if !immediate && p.timer == nil {
		p.timer = time.AfterFunc(p.delay, p.onTimer)
	}
	p.mu.Unlock()

	if immediate {
		p.write()
	}
}

func (p *persister) onTimer() {
	p.mu.Lock()
	p.timer = nil
	p.mu.Unlock()
	p.write()
}

// Flush writes any change that is still waiting. Call it before the process exits.
func (p *persister) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
	p.write()
}

// write saves the current state if anything changed since the last save.
func (p *persister) write() {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	// Cleared before the snapshot: a change made while saving marks it dirty again
	// and gets its own write, so no change is ever lost.
	p.dirty = false
	p.mu.Unlock()

	data, err := p.snapshot()
	if err == nil {
		err = writeFileAtomic(p.path, data, 0o644)
	}
	if err != nil {
		p.failed(err)
		return
	}
	p.mu.Lock()
	p.lastErr = ""
	p.mu.Unlock()
}

// failed keeps the change pending and tries again later, logging a persistent
// problem once rather than at every attempt.
func (p *persister) failed(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	if msg := err.Error(); msg != p.lastErr {
		p.lastErr = msg
		log.Printf("state: cannot save %s: %v (will retry)", p.path, err)
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(p.retry, p.onTimer)
	}
}
