package bot

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DeleteMode controls when downloaded files are removed after upload.
type DeleteMode string

const (
	// DeleteNever keeps all files (default).
	DeleteNever DeleteMode = "never"
	// DeleteOnUpload removes a file immediately after it is sent successfully.
	DeleteOnUpload DeleteMode = "on_upload"
	// DeleteOnDiskPressure removes oldest files when disk usage exceeds DiskThresholdPct.
	DeleteOnDiskPressure DeleteMode = "disk_pressure"
)

// Defaults for the safety limits of the disk_pressure mode.
const (
	// A file modified more recently than this is never removed: it may still be
	// downloading, or waiting to be uploaded.
	defaultDiskMinAgeMin = 60
	// The most one check may remove, counted across every watched directory.
	defaultDiskMaxDeleteMB = 2048
)

// Cleaner manages post-upload file deletion.
type Cleaner struct {
	mode             DeleteMode
	diskThresholdPct float64 // 0–100, used with DeleteOnDiskPressure
	minAge           time.Duration
	maxDeleteBytes   int64
	watchDirs        []string
	checkInterval    time.Duration
	stats            *Stats // optional, may be nil

	// lastErr remembers the last disk error logged per directory, so a problem
	// that persists is reported once instead of at every check.
	lastErr map[string]string
}

// SetStats attaches a Stats instance so deletions are recorded.
func (c *Cleaner) SetStats(s *Stats) {
	c.stats = s
}

// NewCleaner creates a Cleaner from bot config values.
func NewCleaner(cfg BotConfig) *Cleaner {
	threshold := cfg.DiskThresholdPct
	if threshold <= 0 {
		threshold = 80
	}
	interval := time.Duration(cfg.DiskCheckIntervalMin) * time.Minute
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	minAgeMin := cfg.DiskMinAgeMin
	if minAgeMin <= 0 {
		minAgeMin = defaultDiskMinAgeMin
	}
	maxDeleteMB := cfg.DiskMaxDeleteMB
	if maxDeleteMB <= 0 {
		maxDeleteMB = defaultDiskMaxDeleteMB
	}
	return &Cleaner{
		mode:             DeleteMode(cfg.DeleteMode),
		diskThresholdPct: threshold,
		minAge:           time.Duration(minAgeMin) * time.Minute,
		maxDeleteBytes:   maxDeleteMB * 1024 * 1024,
		checkInterval:    interval,
	}
}

// SetWatchDirs tells the cleaner which directories to monitor and clean.
func (c *Cleaner) SetWatchDirs(dirs []string) {
	var valid []string
	for _, d := range dirs {
		if d != "" {
			valid = append(valid, d)
		}
	}
	c.watchDirs = valid
}

// Start launches the background disk-pressure monitor.
// Only active when mode is DeleteOnDiskPressure.
func (c *Cleaner) Start() {
	if c.mode != DeleteOnDiskPressure {
		return
	}
	go func() {
		ticker := time.NewTicker(c.checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			c.runDiskPressureCheck()
		}
	}()
}

// OnUploaded is called after a file has been sent to Telegram successfully.
// Deletes the file when mode is DeleteOnUpload.
func (c *Cleaner) OnUploaded(path string) {
	if c.mode != DeleteOnUpload {
		return
	}
	c.deleteFile(path)
}

// runDiskPressureCheck scans watchDirs, checks disk usage, and removes
// oldest files until usage drops below the threshold.
func (c *Cleaner) runDiskPressureCheck() {
	// Bytes removed during this check. It is shared by every watched directory,
	// so the limit bounds the whole check and not each directory separately.
	var freed int64

	for _, dir := range c.watchDirs {
		// The app creates an output directory on the first download of that kind,
		// so one that does not exist yet simply has nothing to clean.
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			delete(c.lastErr, dir)
			continue
		}
		if err != nil {
			c.reportError(dir, err)
			continue
		}
		if !info.IsDir() {
			continue
		}

		pct, err := diskUsagePct(dir)
		if err != nil {
			c.reportError(dir, err)
			continue
		}
		delete(c.lastErr, dir)

		if pct < c.diskThresholdPct {
			delete(c.lastErr, "note:"+dir)
			continue
		}
		log.Printf("cleaner: disk at %.1f%% (threshold %.0f%%), cleaning %s",
			pct, c.diskThresholdPct, dir)
		c.evictOldest(dir, &freed)
	}
}

// reportError logs a disk error for dir unless the same error was already logged
// for it on the previous check.
func (c *Cleaner) reportError(dir string, err error) {
	if c.lastErr == nil {
		c.lastErr = make(map[string]string)
	}
	msg := err.Error()
	if c.lastErr[dir] == msg {
		return
	}
	c.lastErr[dir] = msg
	log.Printf("cleaner: cannot check disk for %s: %v", dir, err)
}

// noteOnce logs a situation that will not change by itself, such as "nothing
// left to remove", once rather than at every check. The note is forgotten when
// the directory drops back under the threshold.
func (c *Cleaner) noteOnce(dir, msg string) {
	if c.lastErr == nil {
		c.lastErr = make(map[string]string)
	}
	key := "note:" + dir
	if c.lastErr[key] == msg {
		return
	}
	c.lastErr[key] = msg
	log.Print("cleaner: " + msg)
}

// evictOldest removes the oldest files under dir until disk usage drops below the
// threshold, with two safeguards:
//
//   - a file modified within minAge is never touched (it may be downloading or
//     waiting to be uploaded), and
//   - once *freed reaches maxDeleteBytes it stops, leaving the rest for the next
//     check. The last file may take the total slightly past the limit.
//
// *freed accumulates across the directories of one check.
func (c *Cleaner) evictOldest(dir string, freed *int64) {
	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}

	cutoff := time.Now().Add(-c.minAge)
	var files []entry
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil // too recent to remove
		}
		files = append(files, entry{path, info.Size(), info.ModTime()})
		return nil
	})

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	capped := false
	for _, f := range files {
		pct, err := diskUsagePct(dir)
		if err != nil || pct < c.diskThresholdPct {
			break
		}
		if *freed >= c.maxDeleteBytes {
			capped = true
			break
		}
		if c.deleteFile(f.path) {
			*freed += f.size
		}
	}

	// Remove empty subdirectories after cleanup
	removeEmptyDirs(dir)

	pct, err := diskUsagePct(dir)
	if err != nil || pct < c.diskThresholdPct {
		delete(c.lastErr, "note:"+dir)
		return
	}
	if capped {
		log.Printf("cleaner: reached the limit of %d MB for one check; %s is still above the threshold, continuing at the next check",
			c.maxDeleteBytes/(1024*1024), dir)
		return
	}
	c.noteOnce(dir, fmt.Sprintf(
		"%s is above the threshold but holds no more files older than %s, so nothing else can be removed",
		dir, c.minAge))
}

// deleteFile removes a single file, logs the result, and reports whether the
// file was actually removed.
func (c *Cleaner) deleteFile(path string) bool {
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("cleaner: failed to delete %s: %v", path, err)
		}
		return false
	}
	log.Printf("cleaner: deleted %s", path)
	if c.stats != nil {
		c.stats.RecordDeleted(1)
	}
	return true
}

// removeEmptyDirs removes empty subdirectories under root, deepest first, so a
// chain such as artist/album/ disappears completely once its files are gone.
// root itself is never removed.
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Walk visits a directory before its children, so reverse order handles children first.
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dirs[i])
		}
	}
}
