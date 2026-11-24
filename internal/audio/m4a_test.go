package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// Real files: testdata/aac_ffmpeg.m4a and aac_afconvert.m4a are 1.5 s of
// AAC made with ffmpeg and macOS afconvert, alac.m4a Apple Lossless.
func TestM4AFiles(t *testing.T) {
	for _, name := range []string{"aac_ffmpeg.m4a", "aac_afconvert.m4a"} {
		d, err := Check(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// afconvert counts the encoder's priming samples too (~1.57 s).
		if d < 1500*time.Millisecond || d > 1600*time.Millisecond {
			t.Errorf("%s: length %v, want about 1.5 s", name, d)
		}
	}
	_, err := Check(filepath.Join("testdata", "alac.m4a"))
	if e, ok := errors.AsType[*i18n.Error](err); !ok || e.Key != "err.m4aLossless" {
		t.Errorf("alac: err = %v, want err.m4aLossless", err)
	}
	if MIMEType("x.M4A") != "audio/mp4" || !Supported("song.m4a") {
		t.Error("m4a must be a supported type")
	}
}

// atom builds an atom with a 32-bit size.
func atom(typ string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	return append(binary.BigEndian.AppendUint32(nil, uint32(atomHeader+len(b))), append([]byte(typ), b...)...)
}

func mdhd0(scale, dur uint32) []byte {
	b := make([]byte, mdhdV0Len)
	binary.BigEndian.PutUint32(b[12:], scale)
	binary.BigEndian.PutUint32(b[16:], dur)
	return atom("mdhd", b)
}

func mdhd1(scale uint32, dur uint64) []byte {
	b := make([]byte, mdhdV1Len)
	b[0] = 1
	binary.BigEndian.PutUint32(b[20:], scale)
	binary.BigEndian.PutUint64(b[24:], dur)
	return atom("mdhd", b)
}

func hdlr(sub string) []byte {
	return atom("hdlr", make([]byte, fullAtomHead), []byte("mhlr"), []byte(sub), make([]byte, 12), []byte{0})
}

func stsd(format string) []byte {
	entry := atom(format, make([]byte, 28))
	return atom("stsd", make([]byte, fullAtomHead), binary.BigEndian.AppendUint32(nil, 1), entry)
}

func track(header []byte, handler, format string) []byte {
	return atom("trak", atom("tkhd", make([]byte, 84)),
		atom("mdia", header, hdlr(handler), atom("minf", atom("smhd", make([]byte, 8)), atom("stbl", stsd(format)))))
}

func ftyp() []byte { return atom("ftyp", []byte("M4A "), make([]byte, 4)) }

func TestM4ADuration(t *testing.T) {
	aac := track(mdhd0(44100, 66150), "soun", "mp4a") // 1.5 s
	tests := []struct {
		name string
		file []byte
		want time.Duration
		key  string // expected i18n error, "" = success or plain error
		fail bool
	}{
		{"version 0", bytes.Join([][]byte{ftyp(), atom("moov", aac), atom("mdat", make([]byte, 64))}, nil), 1500 * time.Millisecond, "", false},
		{"version 1, 10 hours", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd1(48000, 48000*36000), "soun", "mp4a"))}, nil), 10 * time.Hour, "", false},
		{"video track first", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(600, 6000), "vide", "avc1"), aac)}, nil), 1500 * time.Millisecond, "", false},
		{"mdat at end with size 0", append(bytes.Join([][]byte{ftyp(), atom("moov", aac)}, nil), 0, 0, 0, 0, 'm', 'd', 'a', 't', 1, 2, 3), 1500 * time.Millisecond, "", false},
		{"no sound", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(600, 6000), "vide", "avc1"))}, nil), 0, "err.m4aNoSound", true},
		{"no moov", ftyp(), 0, "err.m4aNoSound", true},
		{"protected", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(44100, 1), "soun", "enca"))}, nil), 0, "err.m4aProtected", true},
		{"other codec", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(44100, 1), "soun", "ac-3"))}, nil), 0, "err.m4aCodec", true},
		{"unknown length", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(44100, unknownLength), "soun", "mp4a"))}, nil), 0, "", true},
		{"zero time scale", bytes.Join([][]byte{ftyp(), atom("moov", track(mdhd0(0, 5), "soun", "mp4a"))}, nil), 0, "", true},
		{"cut off", bytes.Join([][]byte{ftyp(), atom("moov", aac)}, nil)[:60], 0, "", true},
		{"atom bigger than parent", bytes.Join([][]byte{ftyp(), atom("moov", append([]byte{0, 0, 1, 0}, []byte("trak")...))}, nil), 0, "", true},
		{"atom smaller than header", bytes.Join([][]byte{ftyp(), {0, 0, 0, 4, 'm', 'o', 'o', 'v'}}, nil), 0, "", true},
		{"size 0 inside moov", bytes.Join([][]byte{ftyp(), atom("moov", []byte{0, 0, 0, 0, 't', 'r', 'a', 'k'})}, nil), 0, "", true},
		{"nested too deep", bytes.Join([][]byte{ftyp(), atom("moov", atom("moov", atom("moov", atom("moov", atom("moov", atom("moov", aac))))))}, nil), 0, "", true},
	}
	for _, tt := range tests {
		d, err := m4aDuration(bytes.NewReader(tt.file), int64(len(tt.file)))
		if tt.fail {
			if err == nil {
				t.Errorf("%s: no error, length %v", tt.name, d)
				continue
			}
			if tt.key != "" {
				if e, ok := errors.AsType[*i18n.Error](err); !ok || e.Key != tt.key {
					t.Errorf("%s: err = %v, want %s", tt.name, err, tt.key)
				}
			}
			continue
		}
		if err != nil || d != tt.want {
			t.Errorf("%s: %v, %v; want %v", tt.name, d, err, tt.want)
		}
	}
}

// A 64-bit extended size (size field 1) is read.
func TestM4AExtendedSize(t *testing.T) {
	moov := atom("moov", track(mdhd0(1000, 2000), "soun", "mp4a"))
	ext := binary.BigEndian.AppendUint32(nil, 1)
	ext = append(ext, "moov"...)
	ext = binary.BigEndian.AppendUint64(ext, uint64(len(moov)+atomExtended))
	ext = append(ext, moov[atomHeader:]...)
	file := append(ftyp(), ext...)
	path := filepath.Join(t.TempDir(), "ext.m4a")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := Duration(path); err != nil || d != 2*time.Second {
		t.Fatalf("got %v, %v; want 2s", d, err)
	}
}
