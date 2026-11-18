package audio

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// writeWAV writes a silent 16-bit mono PCM WAV of the given length.
func writeWAV(t *testing.T, path string, sampleRate, samples int) {
	t.Helper()
	const bitsPerSample, channels = 16, 1
	dataLen := samples * channels * bitsPerSample / 8
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := func(v any) {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	f.WriteString("RIFF")
	w(uint32(36 + dataLen))
	f.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(sampleRate * channels * bitsPerSample / 8))
	w(uint16(channels * bitsPerSample / 8))
	w(uint16(bitsPerSample))
	f.WriteString("data")
	w(uint32(dataLen))
	f.Write(make([]byte, dataLen))
}

func TestDurationWAV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "beep.wav")
	writeWAV(t, p, 44100, 66150) // 1.5 s
	d, err := Duration(p)
	if err != nil {
		t.Fatal(err)
	}
	if d < 1490*time.Millisecond || d > 1510*time.Millisecond {
		t.Fatalf("duration = %s, want 1.5s", d)
	}
}

func TestDurationRejectsUnsupported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(p, []byte("nope"), 0o600)
	if _, err := Duration(p); err == nil {
		t.Fatal("expected error for unsupported file")
	}
	if Supported(p) || !Supported("A.MP3") {
		t.Fatal("Supported() wrong")
	}
}

func TestCheckRefusesHugeFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "long.wav")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileMB<<20 + 1); err != nil { // sparse, no real disk use
		t.Fatal(err)
	}
	f.Close()
	_, err = Check(p)
	var e *i18n.Error
	if !errors.As(err, &e) || e.Key != "err.soundTooBig" {
		t.Fatalf("want err.soundTooBig, got %v", err)
	}
}
