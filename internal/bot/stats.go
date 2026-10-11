package bot

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Stats holds all counters persisted across bot restarts.
type Stats struct {
	mu      sync.Mutex
	persist *persister // saves the counters to disk; nil when there is no file

	TotalTracks    int64            `json:"total_tracks"`
	TotalFailures  int64            `json:"total_failures"`
	TotalBytesRead int64            `json:"total_bytes_read"`
	TotalDeleted   int64            `json:"total_deleted"`
	TotalCacheHits int64            `json:"total_cache_hits"`
	ByUser         map[int64]*UserStat `json:"by_user"`
	StartedAt      time.Time        `json:"started_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// UserStat holds per-user counters.
type UserStat struct {
	Username   string    `json:"username,omitempty"`
	Tracks     int64     `json:"tracks"`
	Failures   int64     `json:"failures"`
	BytesRead  int64     `json:"bytes_read"`
	LastActive time.Time `json:"last_active"`
}

// NewStats loads the counters from path. A missing file starts from zero; an
// unreadable one is moved aside (and logged) instead of being overwritten, and
// also starts from zero. An empty path keeps the counters in memory only.
func NewStats(path string) *Stats {
	s := &Stats{
		ByUser:    make(map[int64]*UserStat),
		StartedAt: time.Now(),
	}
	if path != "" {
		if _, err := loadJSON(path, s); err != nil {
			log.Printf("stats: %v; starting from zero", err)
			s = &Stats{ByUser: make(map[int64]*UserStat), StartedAt: time.Now()}
		}
		if s.ByUser == nil { // the file held "by_user": null
			s.ByUser = make(map[int64]*UserStat)
		}
	}
	s.persist = newPersister(path, s.snapshot)
	return s
}

// snapshot is the JSON the persister writes.
func (s *Stats) snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.MarshalIndent(s, "", "  ")
}

// Flush writes any change that is still waiting to be saved.
func (s *Stats) Flush() { s.persist.Flush() }

// RecordSuccess adds a completed job to the counters.
func (s *Stats) RecordSuccess(userID int64, username string, tracks int, bytesRead int64) {
	s.mu.Lock()
	s.TotalTracks += int64(tracks)
	s.TotalBytesRead += bytesRead
	u := s.user(userID, username)
	u.Tracks += int64(tracks)
	u.BytesRead += bytesRead
	u.LastActive = time.Now()
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.persist.markDirty()
}

// RecordFailure increments the failure counter.
func (s *Stats) RecordFailure(userID int64, username string) {
	s.mu.Lock()
	s.TotalFailures++
	u := s.user(userID, username)
	u.Failures++
	u.LastActive = time.Now()
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.persist.markDirty()
}

// RecordCacheHits counts tracks delivered from the file_id cache, i.e. tracks
// that needed neither a download nor an upload.
func (s *Stats) RecordCacheHits(userID int64, username string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.TotalCacheHits += int64(n)
	u := s.user(userID, username)
	u.LastActive = time.Now()
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.persist.markDirty()
}

// RecordDeleted increments the deleted files counter.
func (s *Stats) RecordDeleted(n int64) {
	s.mu.Lock()
	s.TotalDeleted += n
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.persist.markDirty()
}

// Summary returns a human-readable stats report for the /stats command.
func (s *Stats) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	uptime := time.Since(s.StartedAt).Round(time.Second)

	report := fmt.Sprintf(
		"Bot Statistics\n"+
			"Uptime: %s\n"+
			"Tracks downloaded: %d\n"+
			"Served from cache: %d\n"+
			"Failed jobs: %d\n"+
			"Data served: %s\n"+
			"Files auto-deleted: %d\n",
		uptime,
		s.TotalTracks,
		s.TotalCacheHits,
		s.TotalFailures,
		humanBytes(s.TotalBytesRead),
		s.TotalDeleted,
	)

	// Top 5 users by track count
	type ranked struct {
		id   int64
		stat *UserStat
	}
	var users []ranked
	for id, u := range s.ByUser {
		users = append(users, ranked{id, u})
	}
	sort.Slice(users, func(i, j int) bool {
		return users[i].stat.Tracks > users[j].stat.Tracks
	})

	if len(users) > 0 {
		report += "\nTop users:\n"
		limit := 5
		if len(users) < limit {
			limit = len(users)
		}
		for i, r := range users[:limit] {
			name := r.stat.Username
			if name == "" {
				name = fmt.Sprintf("id:%d", r.id)
			}
			report += fmt.Sprintf("  %d. %s - %d tracks, %s\n",
				i+1, name, r.stat.Tracks, humanBytes(r.stat.BytesRead))
		}
	}

	return report
}

// user returns (creating if needed) the UserStat for a userID.
// Caller must hold s.mu.
func (s *Stats) user(userID int64, username string) *UserStat {
	u, ok := s.ByUser[userID]
	if !ok {
		u = &UserStat{}
		s.ByUser[userID] = u
	}
	if username != "" {
		u.Username = username
	}
	return u
}

// humanBytes formats a byte count as a human-readable string.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
