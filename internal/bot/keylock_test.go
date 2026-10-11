package bot

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedLockExcludesTheSameKey(t *testing.T) {
	k := newKeyedLock()
	unlock1, err := k.Lock(context.Background(), "album:1", nil)
	if err != nil {
		t.Fatal(err)
	}

	var waited int32
	acquired := make(chan struct{})
	go func() {
		unlock2, err := k.Lock(context.Background(), "album:1", func() { atomic.AddInt32(&waited, 1) })
		if err == nil {
			close(acquired)
			unlock2()
		}
	}()

	waitFor(t, "the second job to start waiting", func() bool { return atomic.LoadInt32(&waited) == 1 })
	select {
	case <-acquired:
		t.Fatal("the second job got the lock while it was still held")
	default:
	}

	unlock1()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the second job never got the lock after it was released")
	}
	if got := atomic.LoadInt32(&waited); got != 1 {
		t.Fatalf("onWait was called %d times, want once", got)
	}
}

func TestKeyedLockDifferentKeysDoNotBlockEachOther(t *testing.T) {
	k := newKeyedLock()
	unlockA, err := k.Lock(context.Background(), "album:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockA()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlockB, err := k.Lock(ctx, "album:2", nil)
	if err != nil {
		t.Fatalf("a different album must not wait: %v", err)
	}
	unlockB()
}

func TestKeyedLockWaiterCanBeCancelled(t *testing.T) {
	k := newKeyedLock()
	unlock, err := k.Lock(context.Background(), "album:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	var waiting int32
	go func() {
		_, err := k.Lock(ctx, "album:1", func() { atomic.StoreInt32(&waiting, 1) })
		result <- err
	}()

	waitFor(t, "the job to start waiting", func() bool { return atomic.LoadInt32(&waiting) == 1 })
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled job kept waiting for the lock")
	}
}

func TestKeyedLockRefusesAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newKeyedLock().Lock(ctx, "album:1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestKeyedLockEmptyKeyIsNeverContended(t *testing.T) {
	k := newKeyedLock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := k.Lock(ctx, "", nil); err != nil {
			t.Fatalf("an unkeyed job must never wait: %v", err)
		}
	}
}

func TestKeyedLockUnlockIsIdempotent(t *testing.T) {
	k := newKeyedLock()
	unlock, err := k.Lock(context.Background(), "album:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock() // a second call must neither panic nor free someone else's lock

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	again, err := k.Lock(ctx, "album:1", nil)
	if err != nil {
		t.Fatalf("the key should be free again: %v", err)
	}
	again()
}

func TestJobKey(t *testing.T) {
	cases := []struct{ url, want string }{
		// An album link, with or without ?i=, is keyed by the album being written.
		{"https://music.apple.com/gb/album/fantasy/169962059?i=169962189", "album:169962059"},
		{"https://music.apple.com/us/album/some-album/1234567890", "album:1234567890"},
		{songURL, "song:1234567890"},
		{"https://music.apple.com/us/playlist/todays-hits/pl.f4d106fed2bd41149aaacabb233eb5eb", "playlist:pl.f4d106fed2bd41149aaacabb233eb5eb"},
		{"https://music.apple.com/us/station/some-station/ra.985484166", "station:ra.985484166"},
		{"https://music.apple.com/us/music-video/some-video/1234567890", "mv:1234567890"},
		{"https://example.com/x", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := jobKey(tc.url); got != tc.want {
			t.Errorf("jobKey(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
