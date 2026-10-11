package bot

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// MonitorState tracks whether wrapper-lite is currently considered up or down.
type MonitorState int

const (
	monitorUnknown MonitorState = iota
	monitorUp
	monitorDown
)

// Monitor periodically pings wrapper-lite and notifies admins on state changes.
type Monitor struct {
	url           string
	interval      time.Duration
	timeout       time.Duration
	adminIDs      []int64
	notifyFn      func(adminID int64, text string)

	mu            sync.Mutex
	state         MonitorState
	downSince     time.Time
	lastCheck     time.Time
	consecutiveFail int
}

// NewMonitor creates a Monitor for the given lite-server URL.
// notifyFn is called with each admin's ID and the alert message.
func NewMonitor(liteServerURL string, interval, timeout time.Duration, adminIDs []int64, notifyFn func(int64, string)) *Monitor {
	return &Monitor{
		url:      liteServerURL + "/health",
		interval: interval,
		timeout:  timeout,
		adminIDs: adminIDs,
		notifyFn: notifyFn,
		state:    monitorUnknown,
	}
}

// Start launches the background health-check loop.
func (m *Monitor) Start() {
	if m.url == "" || len(m.adminIDs) == 0 {
		log.Println("monitor: no lite-server URL or no admin IDs configured, health check disabled")
		return
	}
	log.Printf("monitor: watching %s every %s", m.url, m.interval)
	go m.loop()
}

// StatusText returns a human-readable status for the /stats command.
func (m *Monitor) StatusText() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch m.state {
	case monitorUp:
		ago := time.Since(m.lastCheck).Round(time.Second)
		return fmt.Sprintf("wrapper-lite: OK (checked %s ago)", ago)
	case monitorDown:
		duration := time.Since(m.downSince).Round(time.Second)
		return fmt.Sprintf("wrapper-lite: DOWN for %s", duration)
	default:
		return "wrapper-lite: not yet checked"
	}
}

func (m *Monitor) loop() {
	// Run an immediate check on startup, then tick.
	m.check()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.check()
	}
}

func (m *Monitor) check() {
	client := &http.Client{Timeout: m.timeout}
	resp, err := client.Get(m.url)
	if err == nil {
		resp.Body.Close()
	}

	ok := err == nil && resp.StatusCode < 500

	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastCheck = time.Now()

	if ok {
		m.consecutiveFail = 0
		if m.state == monitorDown {
			// Recovered
			downFor := time.Since(m.downSince).Round(time.Second)
			m.state = monitorUp
			m.notify(fmt.Sprintf(
				"wrapper-lite is back online (was down for %s)", downFor))
		} else {
			m.state = monitorUp
		}
		return
	}

	// Failed ping
	m.consecutiveFail++

	// Only alert after 2 consecutive failures to avoid flapping on brief hiccups.
	if m.consecutiveFail == 2 {
		m.state = monitorDown
		m.downSince = time.Now()
		reason := "unreachable"
		if err != nil {
			reason = err.Error()
		} else {
			reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		m.notify(fmt.Sprintf("wrapper-lite is DOWN\nURL: %s\nReason: %s", m.url, reason))
	}
}

// notify sends a message to all configured admins.
// Caller must hold m.mu.
func (m *Monitor) notify(text string) {
	log.Println("monitor: alert -", text)
	for _, id := range m.adminIDs {
		m.notifyFn(id, text)
	}
}
