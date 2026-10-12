package bot

import (
	"sync"
	"time"
)

// redrawer keeps one Telegram message up to date while a long step runs. It calls
// render at a fixed interval and edits the message when the text has changed.
//
// The drawing happens on its own goroutine, never on the goroutine that produces
// the progress: an edit is a network call, and a slow Telegram must not hold up
// the download or the upload being measured.
type redrawer struct {
	edit     func(text string)
	interval time.Duration
	render   func() string // takes whatever lock the owner's state needs

	mu       sync.Mutex // guards the fields below
	lastSent string
	stop     chan struct{} // closed to end the goroutine; nil until Start
	stopped  chan struct{} // closed when the goroutine has exited
	stopOnce sync.Once
}

func newRedrawer(edit func(text string), interval time.Duration, render func() string) *redrawer {
	return &redrawer{edit: edit, interval: interval, render: render}
}

// Start draws once at once, so the user sees the message as soon as the step
// begins, and then every interval. Calling it again, or with nothing to edit
// with, does nothing.
func (r *redrawer) Start() {
	r.mu.Lock()
	if r.stop != nil || r.edit == nil {
		r.mu.Unlock()
		return
	}
	r.stop, r.stopped = make(chan struct{}), make(chan struct{})
	stop, stopped := r.stop, r.stopped
	r.mu.Unlock()

	go func() {
		defer close(stopped)
		r.tick()
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.tick()
			}
		}
	}()
}

// tick edits the message when the text differs from what was last sent.
func (r *redrawer) tick() {
	text := r.render()

	r.mu.Lock()
	changed := text != r.lastSent
	if changed {
		r.lastSent = text
	}
	r.mu.Unlock()

	if changed {
		r.edit(text) // outside the lock: this is a network call
	}
}

// Stop ends the redrawing. It waits for an edit that is still in flight, so a
// late frame can never overwrite whatever is written next, but not for longer
// than ten seconds, so a stuck network call cannot hang the job.
func (r *redrawer) Stop() {
	r.mu.Lock()
	stop, stopped := r.stop, r.stopped
	r.mu.Unlock()
	if stop == nil {
		return
	}

	r.stopOnce.Do(func() { close(stop) })
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
	}
}

// Done stops the redrawing and replaces the message with the final text.
func (r *redrawer) Done(text string) {
	r.Stop()
	if r.edit != nil {
		r.edit(text)
	}
}
