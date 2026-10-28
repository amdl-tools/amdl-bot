package bot

// token.go holds the Bot-level Apple Music token cache.
//
// resolveToken (in runner.go) fetches a fresh token on every call — which meant
// each album/playlist precheck and every worker subprocess that did not receive a
// token from the precheck (songs, music videos, stations) paid a network round-trip
// to music.apple.com before doing any real work.
//
// resolvedToken caches the last good token in the Bot and returns it on subsequent
// calls without hitting the network. The token is refreshed automatically when:
//   - the cache is empty (first call, or after a process restart), or
//   - the cached token is older than tokenTTL, or
//   - the caller explicitly signals that the token was rejected (via invalidateToken).
//
// Apple Music tokens are JWT-signed bundles that expire after roughly 6 months,
// but the bot invalidates and refreshes on any download error that looks like an
// auth failure, so the TTL here is intentionally generous (24 hours) — it prevents
// needless re-fetches while still refreshing well before the real expiry.

import (
	"log"
	"strings"
	"sync"
	"time"
)

// tokenTTL is how long a cached token is considered fresh.
// Apple Music tokens are valid for ~6 months; 24 h is a conservative refresh window
// that avoids network round-trips without risking stale credentials.
const tokenTTL = 24 * time.Hour

// tokenState holds the cached token and the time it was fetched.
type tokenState struct {
	mu         sync.RWMutex
	refreshMu  sync.Mutex // serialises concurrent refresh calls
	value      string
	fetchedAt  time.Time
}

// resolvedToken returns the cached Apple Music token, fetching a fresh one when
// the cache is empty or older than tokenTTL. It is safe for concurrent use.
func (b *Bot) resolvedToken() (string, error) {
	b.cachedToken.mu.RLock()
	tok := b.cachedToken.value
	age := time.Since(b.cachedToken.fetchedAt)
	b.cachedToken.mu.RUnlock()

	if tok != "" && age < tokenTTL {
		return tok, nil
	}

	return b.refreshToken()
}

// invalidateToken clears the cached token so the next call to resolvedToken
// fetches a fresh one. Call this when a download fails with an auth error.
func (b *Bot) invalidateToken() {
	b.cachedToken.mu.Lock()
	b.cachedToken.value = ""
	b.cachedToken.mu.Unlock()
	log.Print("token: invalidated cached Apple Music token; next request will refresh it")
}

// refreshToken fetches a new token, stores it in the cache, and returns it.
// Only one goroutine fetches at a time; others wait and reuse the result.
func (b *Bot) refreshToken() (string, error) {
	b.cachedToken.refreshMu.Lock()
	defer b.cachedToken.refreshMu.Unlock()

	// Another goroutine may have already refreshed while we waited for the lock.
	b.cachedToken.mu.RLock()
	tok := b.cachedToken.value
	age := time.Since(b.cachedToken.fetchedAt)
	b.cachedToken.mu.RUnlock()
	if tok != "" && age < tokenTTL {
		return tok, nil
	}

	tok, err := resolveToken(b.appCfg)
	if err != nil {
		return "", err
	}

	b.cachedToken.mu.Lock()
	b.cachedToken.value = tok
	b.cachedToken.fetchedAt = time.Now()
	b.cachedToken.mu.Unlock()

	log.Print("token: refreshed Apple Music token")
	return tok, nil
}

// isAuthError reports whether err looks like an Apple Music authentication
// failure, meaning the cached token should be discarded.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "invalid token") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "authentication")
}
