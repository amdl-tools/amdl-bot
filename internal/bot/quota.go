package bot

// quota.go limits how much each user may download per day, so a public bot cannot
// be used to fill the disk or monopolise the workers.
//
// What counts: the tracks and bytes a download actually fetched. Tracks served
// from the file_id cache cost the server nothing, so they are free and are still
// delivered to a user who has used up the day's quota. Admins are exempt.
//
// Reservations: a download reserves the tracks it is expected to fetch before it
// starts and settles with the real numbers when it ends. Without that, two jobs
// of the same user running at once could both pass the check.
//
// The byte limit is soft: the size of a download is not known in advance, so a
// job starts whenever some of the day's data allowance is left and may exceed it.
//
// The day is the server's local calendar day (set TZ to move the boundary).

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// QuotaConfig holds the daily limits per user; 0 means no limit.
type QuotaConfig struct {
	DailyTracks int64
	DailyBytes  int64
}

// QuotaError is returned when a download is refused; Msg is written for the user.
type QuotaError struct{ Msg string }

func (e *QuotaError) Error() string { return e.Msg }

type userUsage struct {
	Day    string `json:"day"` // the calendar day this usage belongs to
	Tracks int64  `json:"tracks"`
	Bytes  int64  `json:"bytes"`
}

// Quota tracks daily usage per user. A nil *Quota means "no limits": every
// method is safe to call on it.
type Quota struct {
	cfg    QuotaConfig
	exempt map[int64]bool
	now    func() time.Time // replaced by tests
	p      *persister

	mu      sync.Mutex
	usage   map[int64]*userUsage
	pending map[int64]int // tracks reserved by downloads that are still running
}

// NewQuota returns the quota tracker, or nil when no limit is configured. Usage is
// kept in path (when not empty) so that restarting the bot does not hand everyone a
// fresh allowance. Users in exempt are never limited.
func NewQuota(cfg QuotaConfig, path string, exempt []int64) *Quota {
	if cfg.DailyTracks <= 0 && cfg.DailyBytes <= 0 {
		return nil
	}
	q := &Quota{
		cfg:     cfg,
		exempt:  make(map[int64]bool, len(exempt)),
		now:     time.Now,
		usage:   make(map[int64]*userUsage),
		pending: make(map[int64]int),
	}
	for _, id := range exempt {
		q.exempt[id] = true
	}

	if path != "" {
		if _, err := loadJSON(path, &q.usage); err != nil {
			log.Printf("quota: %v; starting with no recorded usage", err)
			q.usage = make(map[int64]*userUsage)
		}
		if q.usage == nil { // the file held the JSON value null
			q.usage = make(map[int64]*userUsage)
		}
		today := q.today()
		for id, u := range q.usage {
			if u == nil || u.Day != today {
				delete(q.usage, id) // an earlier day: the allowance has reset
			}
		}
	}
	q.p = newPersister(path, q.snapshot)
	return q
}

func (q *Quota) today() string { return q.now().Format("2006-01-02") }

// untilReset is how long remains until the next local midnight.
func (q *Quota) untilReset() time.Duration {
	t := q.now()
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
	return next.Sub(t)
}

// current returns the user's usage for today, starting a fresh record when the
// stored one is from an earlier day. The caller holds q.mu.
func (q *Quota) current(user int64) *userUsage {
	day := q.today()
	u := q.usage[user]
	if u == nil || u.Day != day {
		u = &userUsage{Day: day}
		q.usage[user] = u
	}
	return u
}

// snapshot is the JSON the persister writes: today's usage only.
func (q *Quota) snapshot() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	today := q.today()
	keep := make(map[int64]*userUsage)
	for id, u := range q.usage {
		if u.Day == today {
			keep[id] = u
		}
	}
	return json.Marshal(keep)
}

// Flush writes any usage that is still waiting to be saved.
func (q *Quota) Flush() {
	if q != nil {
		q.p.Flush()
	}
}

// Reservation is a claim on part of a user's daily allowance, held while a
// download runs. Commit settles it with what was really used; Release gives it
// back unused. Whichever comes first wins, so deferring Release is always safe.
type Reservation struct {
	q      *Quota
	user   int64
	tracks int
	done   bool
}

// Reserve claims `tracks` tracks of the user's allowance, or returns a
// *QuotaError saying why the download may not start. The reservation it returns
// is never nil, and is a no-op for users who are not limited.
func (q *Quota) Reserve(user int64, tracks int) (*Reservation, error) {
	if q == nil || q.exempt[user] {
		return &Reservation{}, nil
	}
	if tracks < 1 {
		tracks = 1
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	u := q.current(user)
	inProgress := q.pending[user]

	if limit := q.cfg.DailyTracks; limit > 0 {
		left := limit - u.Tracks - int64(inProgress)
		switch {
		case left <= 0:
			msg := fmt.Sprintf("Daily limit reached: you have used %d of %d track(s) today", u.Tracks, limit)
			if inProgress > 0 {
				msg += fmt.Sprintf(" and %d more are being downloaded", inProgress)
			}
			return nil, &QuotaError{fmt.Sprintf("%s. Your limit resets in %s.", msg, formatDuration(q.untilReset()))}
		case int64(tracks) > left:
			return nil, &QuotaError{fmt.Sprintf(
				"This needs %d track(s) but only %d are left of your daily limit of %d. Your limit resets in %s.",
				tracks, left, limit, formatDuration(q.untilReset()))}
		}
	}
	if limit := q.cfg.DailyBytes; limit > 0 && u.Bytes >= limit {
		return nil, &QuotaError{fmt.Sprintf(
			"Daily data limit reached: you have used %s of %s today. Your limit resets in %s.",
			humanBytes(u.Bytes), humanBytes(limit), formatDuration(q.untilReset()))}
	}

	q.pending[user] += tracks
	return &Reservation{q: q, user: user, tracks: tracks}, nil
}

// Commit ends the reservation and charges the user for what the download really
// fetched, which may be more or less than was reserved.
func (r *Reservation) Commit(tracks int, bytes int64) {
	if r == nil || r.q == nil || r.done {
		return
	}
	r.done = true
	q := r.q

	q.mu.Lock()
	q.unreserve(r.user, r.tracks)
	u := q.current(r.user)
	u.Tracks += int64(tracks)
	u.Bytes += bytes
	q.mu.Unlock()

	q.p.markDirty()
}

// Release ends the reservation without charging anything (the download failed
// before fetching anything, or was cancelled).
func (r *Reservation) Release() {
	if r == nil || r.q == nil || r.done {
		return
	}
	r.done = true

	r.q.mu.Lock()
	r.q.unreserve(r.user, r.tracks)
	r.q.mu.Unlock()
}

// unreserve gives back reserved tracks. The caller holds q.mu.
func (q *Quota) unreserve(user int64, tracks int) {
	q.pending[user] -= tracks
	if q.pending[user] <= 0 {
		delete(q.pending, user)
	}
}

// Reset clears a user's usage for today.
func (q *Quota) Reset(user int64) {
	if q == nil {
		return
	}
	q.mu.Lock()
	delete(q.usage, user)
	q.mu.Unlock()
	q.p.markDirty()
}

// Report describes a user's allowance and what is left of it.
func (q *Quota) Report(user int64) string {
	if q == nil {
		return "No download limits are configured."
	}
	if q.exempt[user] {
		return "Exempt from the download limits."
	}

	q.mu.Lock()
	u := *q.current(user)
	inProgress := q.pending[user]
	q.mu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Daily download quota (resets in %s)\n", formatDuration(q.untilReset()))
	if q.cfg.DailyTracks > 0 {
		fmt.Fprintf(&sb, "Tracks: %d of %d used\n", u.Tracks, q.cfg.DailyTracks)
	} else {
		fmt.Fprintf(&sb, "Tracks: %d used, no limit\n", u.Tracks)
	}
	if q.cfg.DailyBytes > 0 {
		fmt.Fprintf(&sb, "Data: %s of %s used\n", humanBytes(u.Bytes), humanBytes(q.cfg.DailyBytes))
	} else {
		fmt.Fprintf(&sb, "Data: %s used, no limit\n", humanBytes(u.Bytes))
	}
	if inProgress > 0 {
		fmt.Fprintf(&sb, "In progress: %d track(s)\n", inProgress)
	}
	sb.WriteString("Tracks sent from the cache are free and not counted.")
	return sb.String()
}

// formatDuration writes a duration the way a person would say it, rounding up to
// whole minutes.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	mins := int((d + time.Minute - 1) / time.Minute)
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh %dm", h, m)
	}
}
