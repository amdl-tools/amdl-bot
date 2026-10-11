package bot

import (
	"context"
	"sync"
)

// keyedLock serialises jobs that would write to the same files. Downloads now
// run in parallel, so two users asking for the same album at once must not
// both write it: the second one waits, and when it gets its turn the tracks
// are usually in the file_id cache already.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]chan struct{} // closed when the key is released
}

func newKeyedLock() *keyedLock {
	return &keyedLock{held: make(map[string]chan struct{})}
}

// Lock waits until key is free, then holds it until the returned function is
// called. onWait, when not nil, is called once before the first wait. An empty
// key is never contended. It returns ctx.Err() if ctx ends while waiting.
func (k *keyedLock) Lock(ctx context.Context, key string, onWait func()) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return func() {}, nil
	}

	waited := false
	for {
		k.mu.Lock()
		busy, taken := k.held[key]
		if !taken {
			done := make(chan struct{})
			k.held[key] = done
			k.mu.Unlock()

			var once sync.Once
			return func() {
				once.Do(func() {
					k.mu.Lock()
					delete(k.held, key)
					k.mu.Unlock()
					close(done)
				})
			}, nil
		}
		k.mu.Unlock()

		if !waited && onWait != nil {
			onWait()
		}
		waited = true

		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// jobKey names what a link writes to disk, so jobs that overlap can be
// serialised. A song link is keyed by the song itself; an album link, including
// one that points at a single song with ?i=, by the album, since that is the
// folder being written. It returns "" for links that cannot be keyed.
//
// A /song/ link and the /album/ link of the album it belongs to get different
// keys, because the album is not known from the song link alone.
func jobKey(rawURL string) string {
	kind := DetectURLType(rawURL)
	if kind == "" {
		return ""
	}
	_, id := detectURL(rawURL, kind)
	if id == "" {
		return ""
	}
	return kind + ":" + id
}
