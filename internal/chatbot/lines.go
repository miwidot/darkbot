package chatbot

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

const (
	// MaxLineFileBytes caps an answer file: a few thousand sentences fit
	// in far less, and a wrongly chosen big file must not be read.
	MaxLineFileBytes = 1 << 20
	// MaxLineFileLines caps how many answers one file may hold.
	MaxLineFileLines = 10000
)

// utf8BOM: Windows editors may start a UTF-8 file with it.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ReadLines reads an answer file: one answer per line, empty lines
// skipped, each line joined into one chat line (no tabs etc.).
func ReadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, i18n.Wrap(err, "err.readFile")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxLineFileBytes+1))
	if err != nil {
		return nil, i18n.Wrap(err, "err.readFile")
	}
	if len(data) > MaxLineFileBytes {
		return nil, i18n.E("err.lineFileTooBig", "max", MaxLineFileBytes>>20)
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	text := strings.ToValidUTF8(string(data), "")
	var lines []string
	for l := range strings.Lines(text) {
		if l = oneLine(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch {
	case len(lines) == 0:
		return nil, i18n.E("err.lineFileEmpty")
	case len(lines) > MaxLineFileLines:
		return nil, i18n.E("err.lineFileTooMany", "max", MaxLineFileLines)
	}
	return lines, nil
}

// lineFile is one answer file as last read.
type lineFile struct {
	mod   time.Time
	size  int64
	lines []string
	last  int // index of the last answer, -1 = none yet
}

// Lines hands out random lines of answer files. It reads a file again only
// when it changed (time or size), so the streamer can edit it while
// darkbot runs. Safe for concurrent use.
type Lines struct {
	mu    sync.Mutex
	files map[string]*lineFile
	// intN picks a random index; tests replace it.
	intN func(n int) int
}

// NewLines creates an empty cache.
func NewLines() *Lines {
	return &Lines{files: map[string]*lineFile{}, intN: rand.IntN}
}

// Pick returns a random line of the file, never the same one twice in a
// row (when the file has more than one).
func (l *Lines) Pick(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", i18n.Wrap(err, "err.readFile")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.files[path]
	if f == nil || !f.mod.Equal(st.ModTime()) || f.size != st.Size() {
		lines, err := ReadLines(path)
		if err != nil {
			delete(l.files, path)
			return "", err
		}
		f = &lineFile{mod: st.ModTime(), size: st.Size(), lines: lines, last: -1}
		l.files[path] = f
	}
	i := l.intN(len(f.lines))
	if len(f.lines) > 1 && i == f.last {
		i = (i + 1 + l.intN(len(f.lines)-1)) % len(f.lines)
	}
	f.last = i
	return f.lines[i], nil
}
