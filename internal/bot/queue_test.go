package bot

import (
	"sync"
	"testing"
)

func TestQueueEnforcesPerUserLimit(t *testing.T) {
	release := make(chan struct{})
	q := NewQueue(1, 2)
	q.Start(func(job *Job) { <-release })
	defer close(release)

	for i := 0; i < 2; i++ {
		if err := q.Enqueue(newJob(1, 1, 1, "u", ModeALAC, "")); err != nil {
			t.Fatalf("job %d should be accepted: %v", i+1, err)
		}
	}
	if err := q.Enqueue(newJob(1, 1, 1, "u", ModeALAC, "")); err == nil {
		t.Fatal("a third active job for the same user should be rejected")
	}
	if err := q.Enqueue(newJob(2, 2, 1, "u", ModeALAC, "")); err != nil {
		t.Fatalf("another user must not be affected by the limit: %v", err)
	}
}

func TestQueueCancelFreesSlots(t *testing.T) {
	release := make(chan struct{})
	q := NewQueue(1, 1)
	q.Start(func(job *Job) { <-release })
	defer close(release)

	if err := q.Enqueue(newJob(1, 1, 1, "u", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	if n := q.Cancel(1); n != 1 {
		t.Fatalf("Cancel returned %d, want 1", n)
	}
	if got := len(q.ActiveJobs(1)); got != 0 {
		t.Fatalf("cancelled job still listed as active (%d)", got)
	}
	if err := q.Enqueue(newJob(1, 1, 1, "u", ModeALAC, "")); err != nil {
		t.Fatalf("a slot should be free after Cancel: %v", err)
	}
}

// A job cancelled while it was still waiting used to be flipped back to
// "running" by the worker and executed anyway.
func TestQueueCancelledPendingJobIsNotRun(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	ran := map[string]bool{}

	q := NewQueue(1, 5)
	q.Start(func(job *Job) {
		mu.Lock()
		ran[job.URL] = true
		mu.Unlock()
		if job.URL == "first" {
			<-release
		}
	})
	hasRun := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return ran[name]
	}

	if err := q.Enqueue(newJob(1, 1, 1, "first", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(newJob(1, 1, 2, "second", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first job to start", func() bool { return hasRun("first") })

	if n := q.Cancel(1); n != 2 {
		t.Fatalf("Cancel returned %d, want 2 (one running, one pending)", n)
	}
	close(release)

	// The single worker handles jobs in order, so once "marker" has run the
	// worker has already dealt with "second".
	if err := q.Enqueue(newJob(2, 2, 3, "marker", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "marker job to run", func() bool { return hasRun("marker") })

	if hasRun("second") {
		t.Fatal("a job cancelled while pending must never run")
	}
}

func TestQueueForgetsFinishedJobs(t *testing.T) {
	done := make(chan struct{}, 1)
	q := NewQueue(1, 5)
	q.Start(func(job *Job) { done <- struct{}{} })

	if err := q.Enqueue(newJob(1, 1, 1, "u", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	<-done
	waitFor(t, "finished job to be forgotten", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.jobs) == 0 && len(q.userJobs) == 0
	})
}

func TestNewJobIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 2000; i++ {
		id := newJob(1, 1, 1, "u", ModeALAC, "").ID
		if seen[id] {
			t.Fatalf("duplicate job ID %q", id)
		}
		seen[id] = true
	}
}

// A job cancelled while it was waiting never reaches the processor, so without
// this notification its progress message would stay stuck on "Queued".
func TestQueueNotifiesWhenAPendingJobIsCancelled(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var notified []string

	q := NewQueue(1, 5)
	q.SetOnCancelled(func(job *Job) {
		mu.Lock()
		notified = append(notified, job.URL)
		mu.Unlock()
	})
	q.Start(func(job *Job) {
		if job.URL == "first" {
			started <- struct{}{}
			<-release
		}
	})

	if err := q.Enqueue(newJob(1, 1, 1, "first", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(newJob(1, 1, 2, "second", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	<-started
	q.Cancel(1)
	close(release)

	waitFor(t, "the pending job to be reported as cancelled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notified) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if notified[0] != "second" {
		t.Fatalf("notified about %q, want the job that never ran", notified[0])
	}
}

func TestQueueCancelAllAndActiveCount(t *testing.T) {
	release := make(chan struct{})
	q := NewQueue(1, 5)
	q.Start(func(job *Job) { <-release })

	if err := q.Enqueue(newJob(1, 1, 1, "a", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(newJob(2, 2, 2, "b", ModeALAC, "")); err != nil {
		t.Fatal(err)
	}
	if got := q.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount = %d, want 2", got)
	}

	if got := q.CancelAll(); got != 2 {
		t.Fatalf("CancelAll cancelled %d jobs, want 2 (every user's)", got)
	}
	close(release)

	waitFor(t, "the queue to drain", func() bool { return q.ActiveCount() == 0 })
}
