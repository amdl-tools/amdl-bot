package bot

import (
	"amdl/internal/app"
	"sync"
	"time"
)

// SearchSession holds temporary state while a user is browsing search results.
// It is created when the user runs /search and discarded after they pick an item
// or the session times out.
type SearchSession struct {
	SearchType string               // "album" | "song" | "artist"
	Query      string
	Items      []app.SearchResultItem
	Offset     int
	HasMore    bool
	ExpiresAt  time.Time
}

// SessionStore is a thread-safe in-memory store for per-user sessions.
type SessionStore struct {
	mu      sync.Mutex
	search  map[int64]*SearchSession
	ttl     time.Duration
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	s := &SessionStore{
		search: make(map[int64]*SearchSession),
		ttl:    ttl,
	}
	go s.gc()
	return s
}

// SetSearch stores (or replaces) the search session for a user.
func (s *SessionStore) SetSearch(userID int64, sess *SearchSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.ExpiresAt = time.Now().Add(s.ttl)
	s.search[userID] = sess
}

// GetSearch returns the search session for a user, or nil if none / expired.
func (s *SessionStore) GetSearch(userID int64) *SearchSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.search[userID]
	if !ok || time.Now().After(sess.ExpiresAt) {
		delete(s.search, userID)
		return nil
	}
	return sess
}

// ClearSearch removes the search session for a user.
func (s *SessionStore) ClearSearch(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.search, userID)
}

// gc periodically removes expired sessions.
func (s *SessionStore) gc() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, sess := range s.search {
			if now.After(sess.ExpiresAt) {
				delete(s.search, id)
			}
		}
		s.mu.Unlock()
	}
}
