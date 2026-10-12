package bot

import (
	"strings"
	"testing"
	"time"
)

const mb = int64(1024 * 1024)

// text returns the current message text.
func (u *UploadProgress) text() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.render()
}

// newClockedUpload returns a tracker whose clock starts at 09:00 UTC and whose
// edits are recorded.
func newClockedUpload() (*UploadProgress, *fakeClock, *editLog) {
	clock := &fakeClock{t: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	edits := &editLog{}
	u := NewUploadProgress(ModeALAC, edits.add)
	u.now = clock.Now
	return u, clock, edits
}

func TestUploadProgressShowsTheFirstFile(t *testing.T) {
	u := NewUploadProgress(ModeALAC, nil)
	u.Plan(3, 3*mb)
	u.Begin("01. A.m4a", mb)

	mustContain(t, u.text(),
		"Uploading to Telegram [alac]",
		"[--------------------] 0%",
		"File 1 of 3: 01. A.m4a",
		"Preparing...")
}

func TestUploadProgressCountsBytesAcrossFiles(t *testing.T) {
	u := NewUploadProgress(ModeALAC, nil)
	u.Plan(2, 20*mb)

	u.Begin("A.m4a", 10*mb)
	u.Sent(5 * mb)
	mustContain(t, u.text(), "[#####---------------] 25%", "5.0 MB of 10.0 MB", "File 1 of 2: A.m4a")

	u.Finish()
	u.Begin("B.m4a", 10*mb)
	u.Sent(5 * mb)
	mustContain(t, u.text(), "[###############-----] 75%", "File 2 of 2: B.m4a")
}

// The byte count is what was handed to the network, which runs ahead of what
// Telegram has received, so the last byte must not be reported as "arrived".
func TestUploadProgressWaitsForTelegramAtTheEndOfAFile(t *testing.T) {
	u := NewUploadProgress(ModeALAC, nil)
	u.Plan(1, 10*mb)
	u.Begin("A.m4a", 10*mb)
	u.Sent(10 * mb)

	text := u.text()
	mustContain(t, text, "10.0 MB sent, waiting for Telegram")
	if strings.Contains(text, " at ") {
		t.Fatalf("a finished transfer shows no speed:\n%s", text)
	}
}

func TestUploadProgressShowsTheSpeedOfTheCurrentFile(t *testing.T) {
	u, clock, _ := newClockedUpload()
	u.Plan(1, 10*mb)
	u.Begin("A.m4a", 10*mb)

	u.Sent(2 * mb)
	if strings.Contains(u.text(), " at ") {
		t.Fatalf("a rate from no elapsed time is meaningless:\n%s", u.text())
	}

	clock.Set(clock.Now().Add(2 * time.Second))
	u.Sent(6 * mb)
	mustContain(t, u.text(), "6.0 MB of 10.0 MB at 3.0 MB/s")
}

func TestUploadProgressEstimatesTheTimeLeftOnlyWithEnoughData(t *testing.T) {
	u, clock, _ := newClockedUpload()
	u.Plan(1, 100*mb)
	u.Begin("A.m4a", 100*mb)

	u.Sent(10 * mb) // the first bytes go out now
	clock.Set(clock.Now().Add(5 * time.Second))
	u.Sent(20 * mb)
	if strings.Contains(u.text(), "left") {
		t.Fatalf("five seconds of data is too little for an estimate:\n%s", u.text())
	}

	clock.Set(clock.Now().Add(25 * time.Second)) // 30 s after the first byte, 30% done
	u.Sent(30 * mb)
	mustContain(t, u.text(), "30s elapsed", "about 2m left") // 70 s still to go, shown rounded up
}

func TestUploadProgressGrowsWhenThePlanWasTooSmall(t *testing.T) {
	u := NewUploadProgress(ModeALAC, nil)
	u.Plan(1, 10*mb)
	u.Begin("A.m4a", 10*mb)
	u.Sent(12 * mb) // larger than planned
	mustContain(t, u.text(), "] 100%")

	v := NewUploadProgress(ModeALAC, nil)
	v.Plan(1, 10*mb)
	v.AddUnplanned(5 * mb) // a file whose cached copy was unusable
	v.Begin("B.m4a", 5*mb)
	mustContain(t, v.text(), "File 1 of 2: B.m4a")
}

func TestUploadProgressFinalMessages(t *testing.T) {
	t.Run("after an upload", func(t *testing.T) {
		u, clock, edits := newClockedUpload()
		u.Plan(2, 2*mb)
		u.Begin("A.m4a", mb)
		u.Finish()
		u.Begin("B.m4a", mb)
		u.Finish()
		u.SetFinal("SUMMARY", 150*time.Second)
		clock.Set(clock.Now().Add(69 * time.Second))

		u.Complete()

		texts := edits.all()
		want := "Download complete in 2m 30s, upload in 1m 09s:\n\nSUMMARY"
		if got := texts[len(texts)-1]; got != want {
			t.Fatalf("final text = %q, want %q", got, want)
		}
		if !edits.anyContains("Uploading to Telegram [alac]") {
			t.Fatal("the upload should have been shown before it finished")
		}
	})

	t.Run("nothing needed uploading", func(t *testing.T) {
		u, _, edits := newClockedUpload()
		u.Plan(0, 0)
		u.SetFinal("SUMMARY", 150*time.Second)
		u.Complete()

		got := edits.all()
		if len(got) != 1 || got[0] != "Download complete in 2m 30s:\n\nSUMMARY" {
			t.Fatalf("edits = %q", got)
		}
	})

	t.Run("cancelled part way", func(t *testing.T) {
		u, _, edits := newClockedUpload()
		u.Plan(3, 3*mb)
		u.Begin("A.m4a", mb)
		u.Finish()
		u.Cancel()
		u.Complete()

		texts := edits.all()
		if got := texts[len(texts)-1]; got != "Cancelled while uploading: 1 of 3 file(s) were sent." {
			t.Fatalf("final text = %q", got)
		}
	})

	t.Run("cancelled with nothing to upload", func(t *testing.T) {
		u, _, edits := newClockedUpload()
		u.Plan(0, 0)
		u.Cancel()
		u.Complete()

		if got := edits.all(); len(got) != 1 || got[0] != "Cancelled." {
			t.Fatalf("edits = %q", got)
		}
	})

	t.Run("completing twice writes once", func(t *testing.T) {
		u, _, edits := newClockedUpload()
		u.Plan(0, 0)
		u.SetFinal("SUMMARY", time.Second)
		u.Complete()
		n := len(edits.all())
		u.Complete()
		if len(edits.all()) != n {
			t.Fatal("a second Complete must not edit the message again")
		}
	})
}

func TestUploadProgressNilIsSafe(t *testing.T) {
	var u *UploadProgress
	u.SetFinal("x", time.Second)
	u.Plan(2, mb)
	u.AddUnplanned(mb)
	u.Begin("A.m4a", mb)
	u.Sent(mb)
	u.Finish()
	u.Cancel()
	u.Complete()
}

func TestUploadProgressRedrawsWhileUploading(t *testing.T) {
	withProgressInterval(t, 20*time.Millisecond)
	edits := &editLog{}
	u := NewUploadProgress(ModeALAC, edits.add)

	u.Plan(1, 10*mb)
	u.Begin("A.m4a", 10*mb)
	u.Sent(5 * mb)

	waitFor(t, "a redraw showing the bytes sent", func() bool { return edits.anyContains("5.0 MB of 10.0 MB") })
	u.Finish()
	u.Complete()
}
