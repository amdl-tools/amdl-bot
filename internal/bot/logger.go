package bot

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Logger captures output from the Runner (which uses fmt.Printf/Println internally).
// We redirect os.Stdout by swapping the writer at the process level via a pipe,
// but a cleaner approach is to use a capturing buffer per job.
// This Logger is passed to jobs so they can accumulate messages.
type Logger struct {
	mu   sync.Mutex
	buf  strings.Builder
	sink func(line string) // optional live callback, e.g. to edit a Telegram message
}

func NewLogger(sink func(line string)) *Logger {
	return &Logger{sink: sink}
}

func (l *Logger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := string(p)
	l.buf.WriteString(s)
	if l.sink != nil {
		// Deliver each newline-terminated line
		for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				l.sink(line)
			}
		}
	}
	return len(p), nil
}

func (l *Logger) Printf(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(l, format, args...)
}

func (l *Logger) Println(args ...interface{}) {
	_, _ = fmt.Fprintln(l, args...)
}

// Lines returns a copy of all accumulated output.
func (l *Logger) Lines() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// WriterFor returns an io.Writer backed by this logger,
// usable as a drop-in for os.Stdout replacements.
func (l *Logger) Writer() io.Writer {
	return l
}
