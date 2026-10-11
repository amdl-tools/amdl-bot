package bot

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Mode represents the download quality/format the user requested.
type Mode string

const (
	ModeALAC  Mode = "alac"
	ModeAtmos Mode = "atmos"
	ModeAAC   Mode = "aac"
	ModeMV    Mode = "mv"
)

// JobStatus tracks the lifecycle of a download job.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Job represents a single download request from a Telegram user.
type Job struct {
	ID        string
	UserID    int64
	ChatID    int64
	MessageID int    // Telegram message to edit with progress updates
	URL       string
	Mode      Mode
	Status    JobStatus
	CreatedAt time.Time
	username  string // Telegram username, used for stats

	// Populated after completion
	FilePaths []string
	ErrMsg    string

	cancel context.CancelFunc
	ctx    context.Context
}

// jobSeq makes job IDs unique even when two jobs are created within the clock's
// resolution, which on Windows can be several milliseconds.
var jobSeq uint64

func newJob(userID, chatID int64, messageID int, url string, mode Mode, username string) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{
		ID:        fmt.Sprintf("%d-%d-%d", userID, time.Now().UnixNano(), atomic.AddUint64(&jobSeq, 1)),
		UserID:    userID,
		ChatID:    chatID,
		MessageID: messageID,
		URL:       url,
		Mode:      mode,
		Status:    JobPending,
		CreatedAt: time.Now(),
		username:  username,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Queue is a bounded, concurrent-safe job queue backed by worker goroutines.
type Queue struct {
	mu          sync.Mutex
	jobs        map[string]*Job      // all jobs by ID
	userJobs    map[int64][]string   // active job IDs per user
	ch          chan *Job
	workerCount int
	maxPerUser  int
	processor   func(job *Job)       // injected by Bot

	// onCancelled, when set, is called for a job that was cancelled while it
	// was still waiting, so the user's progress message can be updated: such a
	// job never reaches the processor. Set it before Start.
	onCancelled func(job *Job)
}

func NewQueue(workers, maxPerUser int) *Queue {
	return &Queue{
		jobs:        make(map[string]*Job),
		userJobs:    make(map[int64][]string),
		ch:          make(chan *Job, workers*4),
		workerCount: workers,
		maxPerUser:  maxPerUser,
	}
}

// SetOnCancelled registers the callback described on Queue.onCancelled.
func (q *Queue) SetOnCancelled(fn func(job *Job)) {
	q.onCancelled = fn
}

// Start launches the worker pool. processor is called for each job.
func (q *Queue) Start(processor func(job *Job)) {
	q.processor = processor
	for i := 0; i < q.workerCount; i++ {
		go q.worker()
	}
}

func (q *Queue) worker() {
	for job := range q.ch {
		q.mu.Lock()
		if job.Status == JobCancelled {
			// Cancelled while still waiting: never run it.
			q.forget(job)
			q.mu.Unlock()
			if q.onCancelled != nil {
				q.onCancelled(job)
			}
			continue
		}
		job.Status = JobRunning
		q.mu.Unlock()

		q.processor(job)

		q.mu.Lock()
		if job.Status == JobRunning {
			job.Status = JobDone
		}
		q.forget(job)
		q.mu.Unlock()
	}
}

// forget drops a finished or cancelled job from the per-user list and from the
// job map (which would otherwise grow for as long as the bot runs).
// The caller must hold q.mu.
func (q *Queue) forget(job *Job) {
	ids := q.userJobs[job.UserID]
	for i, id := range ids {
		if id == job.ID {
			q.userJobs[job.UserID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(q.userJobs[job.UserID]) == 0 {
		delete(q.userJobs, job.UserID)
	}
	delete(q.jobs, job.ID)
}

// Enqueue adds a job to the queue if the user is within their limit.
// Returns the job and an error if the user already has too many active jobs.
func (q *Queue) Enqueue(job *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	active := q.userJobs[job.UserID]
	// Count non-cancelled/non-done jobs
	alive := 0
	for _, id := range active {
		if j, ok := q.jobs[id]; ok {
			if j.Status == JobPending || j.Status == JobRunning {
				alive++
			}
		}
	}
	if alive >= q.maxPerUser {
		return fmt.Errorf("you already have %d active download(s). Use /cancel to free a slot", alive)
	}

	q.jobs[job.ID] = job
	q.userJobs[job.UserID] = append(q.userJobs[job.UserID], job.ID)
	q.ch <- job
	return nil
}

// Cancel cancels all pending/running jobs for a user.
// Returns number of jobs cancelled.
func (q *Queue) Cancel(userID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	count := 0
	for _, id := range q.userJobs[userID] {
		if j, ok := q.jobs[id]; ok {
			if j.Status == JobPending || j.Status == JobRunning {
				j.cancel()
				j.Status = JobCancelled
				count++
			}
		}
	}
	return count
}

// ActiveJobs returns a snapshot of pending/running jobs for a user.
func (q *Queue) ActiveJobs(userID int64) []*Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	var result []*Job
	for _, id := range q.userJobs[userID] {
		if j, ok := q.jobs[id]; ok {
			if j.Status == JobPending || j.Status == JobRunning {
				// Return a shallow copy to avoid data races on the caller side
				cp := *j
				result = append(result, &cp)
			}
		}
	}
	return result
}

// CancelAll cancels every pending and running job of every user (used when the
// bot shuts down) and returns how many it cancelled.
func (q *Queue) CancelAll() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	count := 0
	for _, j := range q.jobs {
		if j.Status == JobPending || j.Status == JobRunning {
			j.cancel()
			j.Status = JobCancelled
			count++
		}
	}
	return count
}

// ActiveCount returns how many jobs are queued or still running, including
// cancelled jobs whose process has not finished stopping.
func (q *Queue) ActiveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}
