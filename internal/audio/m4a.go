package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// m4a support. darkbot never decodes sound itself for playback: the
// webview (WebView2, WKWebView, WebKitGTK) or OBS plays the file. It only
// needs the play length, to know when the next queued sound may start.
// An .m4a file is an MPEG-4 container, so the length is read from the
// container's headers; no AAC decoder is needed (beep has none).
//
// Layouts follow Apple's QuickTime File Format reference, whose atoms are
// "functionally identical" to the boxes of ISO/IEC 14496-12 (MPEG-4):
//   - Atoms: size (32 bit, includes the 8 header bytes; 1 = a 64-bit
//     extended size follows the type; 0 = extends to the end of the file,
//     top level only), type (fourcc), all integers big-endian.
//     https://developer.apple.com/documentation/quicktime-file-format/atoms
//   - Media header 'mdhd': version (1), flags (3), creation time,
//     modification time, time scale (4), duration, ... Version 0 has
//     32-bit times and duration; version 1 (ISO 14496-12) 64-bit ones.
//     https://developer.apple.com/documentation/quicktime-file-format/media_header_atom
//   - Handler reference 'hdlr': version (1), flags (3), component type (4),
//     component subtype (4) = 'soun' for a sound track.
//     https://developer.apple.com/documentation/quicktime-file-format/handler_reference_atom
//   - Sample description 'stsd': version (1), flags (3), number of
//     entries (4), then entries starting with size (4) and data format (4).
//     https://developer.apple.com/documentation/quicktime-file-format/sample_description_atom
//   - Data formats: 'mp4a' MPEG-4 Audio (AAC), 'alac' Apple Lossless,
//     'enca' encrypted audio (https://mp4ra.org/registered-types/codecs).
//
// Path to a track's media: moov > trak > mdia > {mdhd, hdlr,
// minf > stbl > stsd}.

const (
	atomHeader    = 8                            // size + type
	atomExtended  = 8                            // 64-bit size after the type when size == 1
	fullAtomHead  = 4                            // version + flags
	mdhdV0Len     = fullAtomHead + 4 + 4 + 4 + 4 // creation, modification, time scale, duration
	mdhdV1Len     = fullAtomHead + 8 + 8 + 4 + 8 // the same with 64-bit times and duration
	hdlrLen       = fullAtomHead + 4 + 4         // component type, component subtype
	stsdLen       = fullAtomHead + 4 + 4 + 4     // entries, first entry's size, its format
	maxAtomDepth  = 5                            // containers moov > trak > mdia > minf > stbl
	unknownLength = 0xFFFFFFFF                   // version 0 duration "not known"
)

// Data formats of an audio track.
const (
	formatAAC       = "mp4a"
	formatALAC      = "alac"
	formatEncrypted = "enca"
)

// atomsToOpen are the containers on the way to a track's media headers.
var atomsToOpen = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true}

// m4aTrack is what the media atom of one track says.
type m4aTrack struct {
	sound     bool
	format    string
	timeScale uint32
	duration  uint64
}

// m4aDuration returns the play length of the first sound track of an
// MPEG-4 audio file of the given size. Only AAC ('mp4a') is accepted: it
// is the m4a format all of darkbot's players handle (Chromium-based
// WebView2 has no Apple Lossless decoder), and protected files can't be
// played at all.
func m4aDuration(r io.ReaderAt, size int64) (time.Duration, error) {
	var tracks []*m4aTrack
	var cur *m4aTrack // the track being read
	var walk func(start, end int64, depth int) error
	walk = func(start, end int64, depth int) error {
		for off := start; off < end; {
			typ, body, next, err := readAtom(r, off, end, depth == 0)
			if err != nil {
				return err
			}
			switch {
			case atomsToOpen[typ]:
				if depth >= maxAtomDepth {
					return fmt.Errorf("atoms nested deeper than %d", maxAtomDepth)
				}
				if typ == "trak" {
					cur = &m4aTrack{}
					tracks = append(tracks, cur)
				}
				if err := walk(body, next, depth+1); err != nil {
					return err
				}
			case cur != nil && typ == "mdhd":
				if cur.timeScale, cur.duration, err = readMdhd(r, body, next); err != nil {
					return err
				}
			case cur != nil && typ == "hdlr":
				b, err := readAt(r, body, next, hdlrLen)
				if err != nil {
					return err
				}
				cur.sound = string(b[8:12]) == "soun"
			case cur != nil && typ == "stsd":
				b, err := readAt(r, body, next, stsdLen)
				if err != nil {
					return err
				}
				if binary.BigEndian.Uint32(b[4:8]) > 0 {
					cur.format = string(b[12:16])
				}
			}
			off = next
		}
		return nil
	}
	if err := walk(0, size, 0); err != nil {
		return 0, err
	}
	for _, t := range tracks {
		if !t.sound {
			continue
		}
		switch t.format {
		case formatAAC:
		case formatALAC:
			return 0, i18n.E("err.m4aLossless")
		case formatEncrypted:
			return 0, i18n.E("err.m4aProtected")
		default:
			return 0, i18n.E("err.m4aCodec", "codec", t.format)
		}
		if t.timeScale == 0 || t.duration == 0 || t.duration == unknownLength {
			return 0, errors.New("could not determine length")
		}
		// Seconds and remainder separately, so a long file can't overflow.
		secs, rest := t.duration/uint64(t.timeScale), t.duration%uint64(t.timeScale)
		return time.Duration(secs)*time.Second + time.Duration(rest)*time.Second/time.Duration(t.timeScale), nil
	}
	return 0, i18n.E("err.m4aNoSound")
}

// readAtom reads the atom header at off (which must end by end) and
// returns its type, where its data starts and where the next atom starts.
func readAtom(r io.ReaderAt, off, end int64, topLevel bool) (typ string, body, next int64, err error) {
	h, err := readAt(r, off, end, atomHeader)
	if err != nil {
		return "", 0, 0, err
	}
	size, body := int64(binary.BigEndian.Uint32(h[:4])), off+atomHeader
	typ = string(h[4:8])
	switch size {
	case 0:
		if !topLevel {
			return "", 0, 0, fmt.Errorf("atom %q: size 0 is only allowed at the top level", typ)
		}
		size = end - off
	case 1:
		x, err := readAt(r, body, end, atomExtended)
		if err != nil {
			return "", 0, 0, err
		}
		u := binary.BigEndian.Uint64(x)
		if u > uint64(end-off) {
			return "", 0, 0, fmt.Errorf("atom %q is larger than the file", typ)
		}
		size, body = int64(u), body+atomExtended
	}
	if size < body-off || size > end-off {
		return "", 0, 0, fmt.Errorf("atom %q has an invalid size %d", typ, size)
	}
	return typ, body, off + size, nil
}

// readMdhd reads time scale and duration from a media header.
func readMdhd(r io.ReaderAt, body, end int64) (uint32, uint64, error) {
	b, err := readAt(r, body, end, mdhdV0Len)
	if err != nil {
		return 0, 0, err
	}
	switch b[0] {
	case 0:
		return binary.BigEndian.Uint32(b[12:16]), uint64(binary.BigEndian.Uint32(b[16:20])), nil
	case 1:
		if b, err = readAt(r, body, end, mdhdV1Len); err != nil {
			return 0, 0, err
		}
		return binary.BigEndian.Uint32(b[20:24]), binary.BigEndian.Uint64(b[24:32]), nil
	}
	return 0, 0, fmt.Errorf("unknown media header version %d", b[0])
}

// readAt reads n bytes at off, which must not run past end.
func readAt(r io.ReaderAt, off, end int64, n int) ([]byte, error) {
	if off < 0 || end-off < int64(n) {
		return nil, errors.New("file is cut off or damaged")
	}
	b := make([]byte, n)
	if _, err := r.ReadAt(b, off); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return b, nil
}
