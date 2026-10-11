package bot

import (
	"os"
	"testing"
	"time"
)

// State files are normally written a couple of seconds after a change, and the
// progress message is redrawn every few seconds. Tests write immediately and
// redraw quickly so they do not have to wait; the timing behaviour has tests of
// its own that set the delay explicitly.
func TestMain(m *testing.M) {
	persistDelay = 0
	progressInterval = 50 * time.Millisecond
	os.Exit(m.Run())
}
