// Package backup saves darkbot's settings together with the files they
// name (sounds, alert pictures and videos, answer files) into one ZIP file,
// and restores them, also on another computer.
//
// Logins and passwords are never part of a backup: they live in the OS
// keyring, not in the settings. After restoring, the streamer connects
// Twitch, Spotify and OBS again where needed.
//
// Layout of a backup:
//
//	darkbot-backup.json   Manifest: format, darkbot version, time, the
//	                      settings and one entry per file
//	files/001-airhorn.mp3 the files, numbered so equal names can't clash
//
// When restoring, a file that is still at its old path with the same
// content stays there; otherwise it is unpacked into a darkbot folder and
// the settings are pointed at it. Names inside the ZIP are never used as
// paths on disk, so a crafted backup can't write outside that folder.
package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

const (
	// ManifestName is the settings file inside a backup.
	ManifestName = "darkbot-backup.json"
	// FormatVersion is raised when the layout changes in a way older
	// darkbot versions can't read.
	FormatVersion = 1
	filesPrefix   = "files/"

	// Limits when reading a backup, so a damaged or crafted ZIP can't fill
	// the disk or memory. Real settings are a few hundred KB.
	maxManifestBytes = 16 << 20
	// maxFileBytes: one file. Sounds are capped at 25 MB (audio.MaxFileMB),
	// answer files at 1 MB; an alert video has no cap of its own.
	maxFileBytes = 512 << 20
	// maxTotalBytes: all files together.
	maxTotalBytes = 4 << 30
	// maxFiles: more than every possible setting (200 commands with sound
	// and answer file, rewards, events) needs.
	maxFiles = 5000
	// maxNameRunes keeps entry and file names short.
	maxNameRunes = 80

	filePerm = 0o600
	dirPerm  = 0o700
)

// File is one file in a backup.
type File struct {
	// Path is where the file was on the computer the backup was made on.
	Path string `json:"path"`
	// Entry is its name inside the ZIP.
	Entry  string `json:"entry"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is darkbot-backup.json.
type Manifest struct {
	Format  int           `json:"format"`
	Version string        `json:"darkbot_version"`
	Created time.Time     `json:"created"`
	Config  config.Config `json:"config"`
	Files   []File        `json:"files"`
}

// Skipped is a file the settings name that could not be put into a backup.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ExportReport says what a backup contains.
type ExportReport struct {
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Skipped []Skipped `json:"skipped,omitempty"`
}

// Export writes cfg and the files it names as a backup to w.
func Export(w io.Writer, cfg config.Config, version string, now time.Time) (ExportReport, error) {
	cfg = cfg.Clone()
	cfg.OBS.Password = "" // only in very old settings; never in a backup
	var rep ExportReport
	m := Manifest{Format: FormatVersion, Version: version, Created: now.UTC(), Config: cfg}
	zw := zip.NewWriter(w)
	for _, p := range cfg.Files() {
		f, err := addFile(zw, p, len(m.Files)+1, now)
		if err != nil {
			var skip *skipError
			if errors.As(err, &skip) {
				rep.Skipped = append(rep.Skipped, Skipped{Path: p, Reason: skip.reason})
				continue
			}
			return ExportReport{}, err
		}
		m.Files = append(m.Files, f)
		rep.Files++
		rep.Bytes += f.Size
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return ExportReport{}, fmt.Errorf("encode backup: %w", err)
	}
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: ManifestName, Method: zip.Deflate, Modified: now})
	if err != nil {
		return ExportReport{}, err
	}
	if _, err := mw.Write(data); err != nil {
		return ExportReport{}, err
	}
	if err := zw.Close(); err != nil {
		return ExportReport{}, fmt.Errorf("finish backup: %w", err)
	}
	return rep, nil
}

// skipError: the file is left out of the backup, the export goes on.
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

// addFile copies one file into the ZIP. A file that is gone or unreadable
// is skipped (the settings keep its path), so one missing sound doesn't
// stop the whole backup.
func addFile(zw *zip.Writer, path string, n int, now time.Time) (File, error) {
	src, err := os.Open(path)
	if err != nil {
		return File{}, &skipError{reason: err.Error()}
	}
	defer src.Close()
	fi, err := src.Stat()
	switch {
	case err != nil:
		return File{}, &skipError{reason: err.Error()}
	case !fi.Mode().IsRegular():
		return File{}, &skipError{reason: "not a regular file"}
	case fi.Size() > maxFileBytes:
		return File{}, &skipError{reason: fmt.Sprintf("larger than %d MB", maxFileBytes>>20)}
	}
	entry := fmt.Sprintf("%s%03d-%s", filesPrefix, n, safeName(path))
	// Sounds, pictures and videos are compressed already; only text
	// (answer files) gets smaller with Deflate.
	method := zip.Store
	if strings.EqualFold(filepath.Ext(path), ".txt") {
		method = zip.Deflate
	}
	dst, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: method, Modified: now})
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, maxFileBytes+1))
	if err != nil {
		// The ZIP may already hold part of it: no way back, fail the export.
		return File{}, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if size > maxFileBytes {
		return File{}, fmt.Errorf("%s grew past %d MB while it was saved", filepath.Base(path), maxFileBytes>>20)
	}
	return File{Path: path, Entry: entry, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// safeName is the base name of a path (Windows or Unix) reduced to
// letters, digits and . _ - so it is a valid file name everywhere.
func safeName(path string) string {
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	for _, r := range base {
		if b.Len() >= maxNameRunes {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := strings.TrimLeft(b.String(), ".")
	if name == "" {
		name = "file"
	}
	return name
}

// RestoreReport says what restoring did with the files.
type RestoreReport struct {
	// Kept: files still at their old path with the same content.
	Kept int `json:"kept"`
	// Unpacked: files written into the restore folder.
	Unpacked int `json:"unpacked"`
	// Missing: paths the settings name that the backup has no file for
	// (they were missing when it was made). Those settings are emptied
	// and their reward, event or command turned off: a backup may only
	// point darkbot at files it brings along. Otherwise a shared backup
	// could make an answer file of a command any file on this computer,
	// and darkbot would post its lines in chat.
	Missing []string `json:"missing,omitempty"`
	// Version and Created describe the backup.
	Version string    `json:"version"`
	Created time.Time `json:"created"`
}

// Restore reads the backup at zipPath, puts its files in place (unpacking
// into dir what isn't on this computer) and returns its settings with the
// file paths pointing at them (see RestoreReport.Missing for settings
// without a file). Nothing is changed in the settings; the
// caller saves the returned config. On an error, files unpacked so far are
// removed again.
func Restore(zipPath, dir string) (config.Config, RestoreReport, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return config.Config{}, RestoreReport{}, i18n.Wrap(err, "err.backupNotZip")
	}
	defer zr.Close()
	m, entries, err := readManifest(&zr.Reader)
	if err != nil {
		return config.Config{}, RestoreReport{}, err
	}
	rep := RestoreReport{Version: m.Version, Created: m.Created}
	byPath := make(map[string]File, len(m.Files))
	for _, f := range m.Files {
		byPath[f.Path] = f
	}

	var created []string
	fail := func(err error) (config.Config, RestoreReport, error) {
		for _, p := range created {
			_ = os.Remove(p)
		}
		return config.Config{}, RestoreReport{}, err
	}
	moved := map[string]string{} // old path -> path on this computer
	for _, p := range m.Config.Files() {
		f, ok := byPath[p]
		if !ok {
			rep.Missing = append(rep.Missing, p)
			continue
		}
		if sameFile(p, f) {
			rep.Kept++
			moved[p] = p
			continue
		}
		dst, err := unpack(entries[f.Entry], f, dir)
		if err != nil {
			return fail(err)
		}
		created = append(created, dst)
		rep.Unpacked++
		moved[p] = dst
	}
	cfg := m.Config
	cfg.OBS.Password = ""
	cfg.RewriteFiles(func(p string) (string, bool) {
		np, ok := moved[p]
		return np, ok
	})
	return cfg, rep, nil
}

// readManifest reads and checks darkbot-backup.json and returns the ZIP
// entries its files refer to.
func readManifest(zr *zip.Reader) (Manifest, map[string]*zip.File, error) {
	byName := map[string]*zip.File{}
	var mf *zip.File
	for _, f := range zr.File {
		if f.Name == ManifestName {
			mf = f
		}
		byName[f.Name] = f
	}
	if mf == nil {
		return Manifest{}, nil, i18n.E("err.backupNoManifest")
	}
	if mf.UncompressedSize64 > maxManifestBytes {
		return Manifest{}, nil, i18n.E("err.backupDamaged")
	}
	rc, err := mf.Open()
	if err != nil {
		return Manifest{}, nil, i18n.Wrap(err, "err.backupDamaged")
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	rc.Close()
	if err != nil || len(data) > maxManifestBytes {
		return Manifest{}, nil, i18n.Wrap(err, "err.backupDamaged")
	}
	// Start from the defaults like config.Open, so settings an older
	// darkbot didn't have get their default values.
	m := Manifest{Config: config.Default()}
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, nil, i18n.Wrap(err, "err.backupDamaged")
	}
	if m.Format < 1 {
		return Manifest{}, nil, i18n.E("err.backupDamaged")
	}
	if m.Format > FormatVersion {
		return Manifest{}, nil, i18n.E("err.backupNewer", "version", m.Version)
	}
	if len(m.Files) > maxFiles {
		return Manifest{}, nil, i18n.E("err.backupDamaged")
	}
	var total int64
	entries := map[string]*zip.File{}
	paths := map[string]bool{}
	for _, f := range m.Files {
		zf := byName[f.Entry]
		switch {
		case zf == nil, !strings.HasPrefix(f.Entry, filesPrefix), entries[f.Entry] != nil,
			f.Path == "", paths[f.Path], f.Size < 0, f.Size > maxFileBytes,
			zf.UncompressedSize64 != uint64(f.Size), len(f.SHA256) != sha256.Size*2:
			return Manifest{}, nil, i18n.E("err.backupDamaged")
		}
		total += f.Size
		if total > maxTotalBytes {
			return Manifest{}, nil, i18n.E("err.backupDamaged")
		}
		entries[f.Entry], paths[f.Path] = zf, true
	}
	return m, entries, nil
}

// sameFile reports whether path holds exactly the backed-up file.
func sameFile(path string, f File) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != f.Size {
		return false
	}
	src, err := os.Open(path)
	if err != nil {
		return false
	}
	defer src.Close()
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == f.SHA256
}

// unpack writes one file into dir under a free name and checks its size
// and checksum. It returns the new path.
func unpack(zf *zip.File, f File, dir string) (string, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// The name comes from the entry but is cleaned again: it is only a
	// label, never a path.
	name := safeName(strings.TrimPrefix(f.Entry, filesPrefix))
	out, err := createFree(dir, name)
	if err != nil {
		return "", err
	}
	dst := out.Name()
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(dst)
		}
	}()
	rc, err := zf.Open()
	if err != nil {
		return "", i18n.Wrap(err, "err.backupDamaged")
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, f.Size+1))
	if err != nil {
		return "", i18n.Wrap(err, "err.backupDamaged")
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return "", i18n.E("err.backupDamaged")
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", dst, err)
	}
	ok = true
	return dst, nil
}

// createFree creates name in dir, or name-2, name-3, ... if it is taken
// (an earlier restore). O_EXCL never overwrites an existing file.
func createFree(dir, name string) (*os.File, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	const maxTries = 1000
	for i := 1; i <= maxTries; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", n, err)
		}
	}
	return nil, fmt.Errorf("no free name for %s in %s", name, dir)
}
