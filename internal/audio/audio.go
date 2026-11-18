// Package audio has small helpers around sound files. Decoding is pure Go
// (no cgo); playback itself happens in the app's webview or in OBS.
package audio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/flac"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/vorbis"
	"github.com/gopxl/beep/v2/wav"

	"github.com/miwidot/darkbot/internal/i18n"
)

// Extensions lists the supported file types (lowercase, with dot).
var Extensions = []string{".mp3", ".wav", ".ogg", ".flac"}

// MIMEType maps a supported extension to the type the webview expects.
func MIMEType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	}
	return ""
}

// Supported reports whether the file extension is supported.
func Supported(path string) bool { return MIMEType(path) != "" }

// MaxFileMB caps sound files chosen for redemptions, events and commands.
// Alert sounds are seconds long; a long WAV would be read into memory on
// every play (the webview's asset server buffers whole responses on
// Windows). 25 MB is still a few minutes of uncompressed WAV.
const MaxFileMB = 25

// Check validates a sound file the streamer picks: supported, readable,
// at most MaxFileMB. It returns its play length.
func Check(path string) (time.Duration, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if fi.Size() > MaxFileMB<<20 {
		return 0, i18n.E("err.soundTooBig", "mb", MaxFileMB)
	}
	return Duration(path)
}

// Duration decodes the file header/stream and returns its play length.
func Duration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	var (
		s      beep.StreamSeekCloser
		format beep.Format
	)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		s, format, err = mp3.Decode(f)
	case ".wav":
		s, format, err = wav.Decode(f)
	case ".ogg":
		s, format, err = vorbis.Decode(f)
	case ".flac":
		s, format, err = flac.Decode(f)
	default:
		f.Close()
		return 0, fmt.Errorf("unsupported file type %q (supported: %s)", filepath.Ext(path), strings.Join(Extensions, ", "))
	}
	if err != nil {
		f.Close()
		return 0, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	defer s.Close() // closes f too
	n := s.Len()
	if n <= 0 || format.SampleRate <= 0 {
		return 0, errors.New("could not determine length")
	}
	return format.SampleRate.D(n), nil
}
