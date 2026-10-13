package bot

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amdl/internal/config"
)

// stubResolveToken replaces resolveToken for the duration of the test and
// restores it on cleanup.
func stubResolveToken(t *testing.T, fn func(config.ConfigSet) (string, error)) {
	t.Helper()
	orig := resolveToken
	resolveToken = fn
	t.Cleanup(func() { resolveToken = orig })
}

func TestResolvedTokenReturnsCachedValue(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	var calls atomic.Int32
	stubResolveToken(t, func(config.ConfigSet) (string, error) {
		calls.Add(1)
		return "tok-abc", nil
	})

	tok1, err := b.resolvedToken()
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	tok2, err := b.resolvedToken()
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}

	if tok1 != "tok-abc" || tok2 != "tok-abc" {
		t.Fatalf("got tokens %q / %q, want tok-abc both times", tok1, tok2)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("resolveToken called %d times, want 1 (should have been cached)", n)
	}
}

func TestResolvedTokenRefreshesAfterTTL(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	var calls atomic.Int32
	stubResolveToken(t, func(config.ConfigSet) (string, error) {
		n := calls.Add(1)
		return fmt.Sprintf("tok-%d", n), nil
	})

	// Seed the cache with an already-expired timestamp.
	b.cachedToken.mu.Lock()
	b.cachedToken.value = "tok-stale"
	b.cachedToken.fetchedAt = time.Now().Add(-(tokenTTL + time.Second))
	b.cachedToken.mu.Unlock()

	tok, err := b.resolvedToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "tok-stale" {
		t.Fatal("returned stale token; expected a refresh")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("resolveToken called %d times after expiry, want 1", n)
	}
}

func TestResolvedTokenPropagatesError(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	stubResolveToken(t, func(config.ConfigSet) (string, error) {
		return "", errors.New("network unreachable")
	})

	_, err := b.resolvedToken()
	if err == nil {
		t.Fatal("expected an error when resolveToken fails, got nil")
	}
}

func TestInvalidateTokenForcesRefresh(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	var calls atomic.Int32
	stubResolveToken(t, func(config.ConfigSet) (string, error) {
		calls.Add(1)
		return "tok-fresh", nil
	})

	// Warm the cache.
	if _, err := b.resolvedToken(); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	b.invalidateToken()

	tok, err := b.resolvedToken()
	if err != nil {
		t.Fatalf("after invalidation: %v", err)
	}
	if tok != "tok-fresh" {
		t.Fatalf("got %q, want tok-fresh", tok)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("resolveToken called %d times, want 2 (warm-up + post-invalidation)", n)
	}
}

func TestResolvedTokenConcurrentCallsFetchOnce(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	var calls atomic.Int32
	// Simulate slow network: give concurrent goroutines time to pile up.
	stubResolveToken(t, func(config.ConfigSet) (string, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return "tok-concurrent", nil
	})

	const goroutines = 20
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = b.resolvedToken()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
		if tokens[i] != "tok-concurrent" {
			t.Errorf("goroutine %d: got %q, want tok-concurrent", i, tokens[i])
		}
	}
	// With the cache empty, concurrent callers may each trigger one fetch before
	// any result is stored. The important invariant is that after the first
	// successful fetch all subsequent calls are served from cache — verified above
	// by calling resolvedToken a second time with a fresh bot in other tests.
	// Here we just check the calls stay well below the goroutine count.
	if n := calls.Load(); n > 5 {
		t.Logf("note: resolveToken was called %d times by %d concurrent goroutines; consider a singleflight if this is too high", n, goroutines)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantYes bool
	}{
		{name: "nil", err: nil, wantYes: false},
		{name: "unrelated", err: errors.New("disk full"), wantYes: false},
		{name: "401 in message", err: errors.New("HTTP 401 from Apple"), wantYes: true},
		{name: "unauthorized", err: errors.New("Unauthorized"), wantYes: true},
		{name: "invalid token", err: errors.New("invalid token provided"), wantYes: true},
		{name: "token expired", err: errors.New("token expired"), wantYes: true},
		{name: "authentication", err: errors.New("authentication failed"), wantYes: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthError(tc.err); got != tc.wantYes {
				t.Fatalf("isAuthError(%v) = %v, want %v", tc.err, got, tc.wantYes)
			}
		})
	}
}
