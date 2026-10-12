package bot

// upload_progress.go draws the progress of the upload to Telegram into the job's
// own message, taking over from the download's progress, and ends it with the
// final summary.
//
// The Telegram client library has no progress callback, so each file is handed to
// it through a reader that counts the bytes it hands over (see uploadSource). That
// count is the data given to the network, which runs a little ahead of the data
// Telegram has received; when a file's last byte has been handed over the message
// says it is waiting for Telegram, instead of claiming the file has arrived.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// UploadProgress tracks the files being uploaded for one job. A nil
// *UploadProgress does nothing, so callers do not have to check for it.
type UploadProgress struct {
	r   *redrawer
	now func() time.Time // replaced by tests

	mu           sync.Mutex // guards everything below
	mode         Mode
	files        int       // files to upload
	totalBytes   int64     // their size
	doneFiles    int       // files finished, whether or not they arrived
	doneBytes    int64     // size of the finished files
	active       bool      // a file is being uploaded
	curName      string    // that file
	curSize      int64     // its size
	curSent      int64     // bytes of it handed to the network
	curFirstByte time.Time // when its first byte went out
	firstByte    time.Time // when the first byte of any file went out
	startedAt    time.Time // when the upload phase began
	cancelled    bool
	completed    bool
	summary      string        // the download's result list, for the final text
	downloadTime time.Duration // how long the download took, for the final text
}

// NewUploadProgress creates a tracker. editFn replaces the text of the message.
func NewUploadProgress(mode Mode, editFn func(text string)) *UploadProgress {
	u := &UploadProgress{mode: mode, now: time.Now}
	u.r = newRedrawer(editFn, progressInterval, u.currentText)
	return u
}

func (u *UploadProgress) currentText() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.render()
}

// SetFinal supplies what the final message is built from: the list of downloaded
// tracks and how long the download took.
func (u *UploadProgress) SetFinal(summary string, downloadTime time.Duration) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.summary, u.downloadTime = summary, downloadTime
	u.mu.Unlock()
}

// Plan states how many files, and how many bytes in all, are going to be
// uploaded. When there are some, the message starts showing the upload.
func (u *UploadProgress) Plan(files int, bytes int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.files, u.totalBytes = files, bytes
	u.mu.Unlock()
	if files > 0 {
		u.start()
	}
}

// AddUnplanned counts a file that was not part of the plan, for example one whose
// cached copy turned out to be unusable and had to be uploaded after all.
func (u *UploadProgress) AddUnplanned(size int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.files++
	u.totalBytes += size
	u.mu.Unlock()
	u.start()
}

func (u *UploadProgress) start() {
	u.mu.Lock()
	if u.startedAt.IsZero() {
		u.startedAt = u.now()
	}
	u.mu.Unlock()
	u.r.Start()
}

// Begin marks the start of one file's upload.
func (u *UploadProgress) Begin(name string, size int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.active = true
	u.curName, u.curSize, u.curSent = name, size, 0
	u.curFirstByte = time.Time{}
	u.mu.Unlock()
}

// Sent records how many bytes of the current file have been handed to the
// network so far (a running total, not an increment). It is called from the
// goroutine that streams the file, and never blocks.
func (u *UploadProgress) Sent(sent int64) {
	if u == nil || sent <= 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	now := u.now()
	if u.curFirstByte.IsZero() {
		u.curFirstByte = now
	}
	if u.firstByte.IsZero() {
		u.firstByte = now
	}
	u.curSent = sent
	if done := u.doneBytes + sent; done > u.totalBytes {
		u.totalBytes = done // the file turned out larger than planned
	}
}

// Finish marks the current file as dealt with, whether it arrived or failed.
func (u *UploadProgress) Finish() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.active {
		u.doneFiles++
		u.doneBytes += u.curSize
	}
	u.active = false
	u.curName, u.curSize, u.curSent = "", 0, 0
}

// Cancel records that the job was cancelled, which changes the final message.
func (u *UploadProgress) Cancel() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.cancelled = true
	u.mu.Unlock()
}

// Complete stops the drawing and writes the final message. It can be called any
// number of times; only the first one has an effect.
func (u *UploadProgress) Complete() {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.completed {
		u.mu.Unlock()
		return
	}
	u.completed = true
	text := u.finalText()
	u.mu.Unlock()

	u.r.Done(text) // not under the lock: it waits for the drawing goroutine, which takes it
}

// finalText is the message that ends the job. The caller holds u.mu.
func (u *UploadProgress) finalText() string {
	files := u.files
	if u.doneFiles > files {
		files = u.doneFiles
	}
	switch {
	case u.cancelled && files == 0:
		return "Cancelled."
	case u.cancelled:
		return fmt.Sprintf("Cancelled while uploading: %d of %d file(s) were sent.", u.doneFiles, files)
	case files == 0:
		// Nothing needed uploading (it all came from the cache, or was too large).
		return fmt.Sprintf("Download complete in %s:\n\n%s", formatElapsed(u.downloadTime), u.summary)
	default:
		return fmt.Sprintf("Download complete in %s, upload in %s:\n\n%s",
			formatElapsed(u.downloadTime), formatElapsed(u.now().Sub(u.startedAt)), u.summary)
	}
}

// render returns the message while uploading. The caller holds u.mu.
func (u *UploadProgress) render() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Uploading to Telegram [%s]\n", u.mode)

	done := u.doneBytes + u.curSent
	if u.totalBytes > 0 {
		pct := int(done * 100 / u.totalBytes)
		if pct > 100 {
			pct = 100
		}
		filled := pct * barWidth / 100
		fmt.Fprintf(&sb, "[%s%s] %d%%\n", strings.Repeat(barFill, filled), strings.Repeat(barEmpty, barWidth-filled), pct)
	} else {
		sb.WriteString("[" + strings.Repeat(barFill, barWidth/2) + strings.Repeat(barEmpty, barWidth/2) + "] working...\n")
	}

	if u.active {
		fmt.Fprintf(&sb, "File %d of %d: %s\n", u.doneFiles+1, u.files, truncateRunes(u.curName, 50))
		switch {
		case u.curSent == 0:
			sb.WriteString("Preparing...\n")
		case u.curSize > 0 && u.curSent >= u.curSize:
			fmt.Fprintf(&sb, "%s sent, waiting for Telegram\n", humanBytes(u.curSize))
		default:
			line := fmt.Sprintf("%s of %s", humanBytes(u.curSent), humanBytes(u.curSize))
			// A rate from under a second of data is mostly noise.
			if d := u.now().Sub(u.curFirstByte); !u.curFirstByte.IsZero() && d >= time.Second {
				line += " at " + humanBytes(int64(float64(u.curSent)/d.Seconds())) + "/s"
			}
			sb.WriteString(line + "\n")
		}
	}

	if !u.startedAt.IsZero() {
		line := formatElapsed(u.now().Sub(u.startedAt)) + " elapsed"
		// Judge the time left by the pace since data started flowing, and only once
		// there is enough of it to be more than a guess.
		if !u.firstByte.IsZero() && u.totalBytes > 0 && done > 0 && done < u.totalBytes {
			if flowing := u.now().Sub(u.firstByte); flowing >= 10*time.Second && done*100 >= u.totalBytes*3 {
				remaining := time.Duration(float64(flowing) * float64(u.totalBytes-done) / float64(done))
				line += ", " + formatRemaining(remaining)
			}
		}
		sb.WriteString(line + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
