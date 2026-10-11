package bot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorAlertsAfterTwoFailuresAndOnRecovery(t *testing.T) {
	var code int32 = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(int(atomic.LoadInt32(&code)))
	}))
	defer srv.Close()

	var msgs []string
	m := NewMonitor(srv.URL, time.Minute, time.Second, []int64{7}, func(id int64, text string) {
		msgs = append(msgs, text)
	})

	m.check()
	if len(msgs) != 0 {
		t.Fatalf("a healthy server must not alert: %q", msgs)
	}
	if !strings.Contains(m.StatusText(), "OK") {
		t.Fatalf("status should read OK, got %q", m.StatusText())
	}

	atomic.StoreInt32(&code, http.StatusInternalServerError)
	m.check()
	if len(msgs) != 0 {
		t.Fatalf("one failed check must not alert (avoids flapping): %q", msgs)
	}
	m.check()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "DOWN") {
		t.Fatalf("expected a single DOWN alert after two failures, got %q", msgs)
	}
	if !strings.Contains(m.StatusText(), "DOWN") {
		t.Fatalf("status should read DOWN, got %q", m.StatusText())
	}

	m.check()
	if len(msgs) != 1 {
		t.Fatalf("a server that stays down must not repeat the alert: %q", msgs)
	}

	atomic.StoreInt32(&code, http.StatusOK)
	m.check()
	if len(msgs) != 2 || !strings.Contains(msgs[1], "back online") {
		t.Fatalf("expected a recovery message, got %q", msgs)
	}
}

func TestMonitorNotifiesEveryAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := map[int64]int{}
	m := NewMonitor(srv.URL, time.Minute, time.Second, []int64{1, 2, 3}, func(id int64, text string) {
		got[id]++
	})
	m.check()
	m.check()

	for _, id := range []int64{1, 2, 3} {
		if got[id] != 1 {
			t.Errorf("admin %d received %d alerts, want 1", id, got[id])
		}
	}
}

func TestMonitorTreatsUnreachableServerAsDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens on this address any more

	var msgs []string
	m := NewMonitor(url, time.Minute, 500*time.Millisecond, []int64{1}, func(id int64, text string) {
		msgs = append(msgs, text)
	})
	m.check()
	m.check()

	if len(msgs) != 1 || !strings.Contains(msgs[0], "DOWN") {
		t.Fatalf("an unreachable server should raise a DOWN alert, got %q", msgs)
	}
}
