// Package applog is a tiny in-memory log that the GUI can display.
// It also mirrors every line to stderr so problems are visible when the app is
// started from a terminal.
package applog

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// maxLines caps memory use; older lines are dropped.
const maxLines = 1000

// Log is a thread-safe ring of log lines.
type Log struct {
	mu       sync.Mutex
	lines    []string
	onChange func()
}

// New creates an empty log.
func New() *Log { return &Log{} }

// OnChange sets a callback fired (from any goroutine) after each new line.
func (l *Log) OnChange(fn func()) {
	l.mu.Lock()
	l.onChange = fn
	l.mu.Unlock()
}

// Infof logs an informational message.
func (l *Log) Infof(format string, args ...any) { l.add("INFO", format, args...) }

// Warnf logs a warning.
func (l *Log) Warnf(format string, args ...any) { l.add("WARN", format, args...) }

// Errorf logs an error.
func (l *Log) Errorf(format string, args ...any) { l.add("ERROR", format, args...) }

func (l *Log) add(level, format string, args ...any) {
	line := fmt.Sprintf("%s %-5s %s", time.Now().Format("15:04:05"), level, fmt.Sprintf(format, args...))
	log.Println(line)

	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLines {
		l.lines = append([]string(nil), l.lines[len(l.lines)-maxLines:]...)
	}
	cb := l.onChange
	l.mu.Unlock()

	if cb != nil {
		cb()
	}
}

// Lines returns a copy of all current lines.
func (l *Log) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}
