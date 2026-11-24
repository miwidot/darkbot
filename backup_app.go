package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/autostart"
	"github.com/miwidot/darkbot/internal/backup"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/selfupdate"
	"github.com/miwidot/darkbot/internal/version"
)

// Backups (Settings, Backup): all settings plus the files they name in one
// ZIP, without logins or passwords (internal/backup).

// backupStamp is the date in a backup's suggested file name and the time
// in the name of the settings kept before an import.
const (
	backupDateStamp   = "2006-01-02"
	preImportStamp    = "20060102-150405"
	backupNamePattern = "darkbot-backup-%s.zip"
)

// BackupExport is what ExportBackup reports to the UI.
type BackupExport struct {
	// Path is where the backup was saved ("" = the dialog was closed).
	Path   string              `json:"path"`
	Report backup.ExportReport `json:"report"`
}

// ExportBackup asks where to save and writes the backup there.
func (a *App) ExportBackup() (BackupExport, error) {
	lang := a.lang()
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           i18n.T(lang, "backup.saveTitle"),
		DefaultFilename: fmt.Sprintf(backupNamePattern, time.Now().Format(backupDateStamp)),
		Filters:         []runtime.FileFilter{{DisplayName: i18n.T(lang, "backup.fileType"), Pattern: "*.zip"}},
	})
	if err != nil || path == "" {
		return BackupExport{}, a.uiErr(err)
	}
	if filepath.Ext(path) == "" {
		path += ".zip"
	}
	rep, err := writeBackup(path, a.cfg.Get())
	if err != nil {
		a.log.Errorf("Backup failed: %v", err)
		return BackupExport{}, a.uiErr(i18n.Wrap(err, "err.backupWrite"))
	}
	a.log.Infof("Backup saved to %s (%d files, %d MB)", path, rep.Files, rep.Bytes>>20)
	for _, s := range rep.Skipped {
		a.log.Warnf("Backup: left out %s (%s)", s.Path, s.Reason)
	}
	return BackupExport{Path: path, Report: rep}, nil
}

// writeBackup writes into a temporary file next to path and renames it,
// so a failed backup never leaves a half-written ZIP (or replaces a good
// one with it).
func writeBackup(path string, cfg config.Config) (backup.ExportReport, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".darkbot-backup-*")
	if err != nil {
		return backup.ExportReport{}, err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	rep, err := backup.Export(tmp, cfg, version.Version, time.Now())
	if err != nil {
		return backup.ExportReport{}, err
	}
	if err := tmp.Close(); err != nil {
		return backup.ExportReport{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return backup.ExportReport{}, err
	}
	done = true
	return rep, nil
}

// BackupImport is what ImportBackup reports to the UI.
type BackupImport struct {
	// Path of the backup ("" = the dialog was closed, nothing changed).
	Path   string               `json:"path"`
	Report backup.RestoreReport `json:"report"`
	// Previous is the copy of the settings from before the import.
	Previous string `json:"previous"`
}

// ImportBackup asks for a backup and replaces all settings with it. The
// current settings are kept as config.json.before-import-<time> first.
// Logins stay as they are. darkbot should restart afterwards
// (RestartApp), so everything runs with the restored settings.
func (a *App) ImportBackup() (BackupImport, error) {
	lang := a.lang()
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   i18n.T(lang, "backup.openTitle"),
		Filters: []runtime.FileFilter{{DisplayName: i18n.T(lang, "backup.fileType"), Pattern: "*.zip"}},
	})
	if err != nil || path == "" {
		return BackupImport{}, a.uiErr(err)
	}
	res, err := a.importBackup(path, time.Now())
	if err != nil {
		a.log.Errorf("Restoring backup %s failed: %v", path, i18n.Localize(err, "en"))
		return BackupImport{}, a.uiErr(err)
	}
	return res, nil
}

func (a *App) importBackup(path string, now time.Time) (BackupImport, error) {
	cfg, rep, err := backup.Restore(path, a.cfg.RestoredFilesDir())
	if err != nil {
		return BackupImport{}, err
	}
	prev := a.cfg.PreImportPath(now.Format(preImportStamp))
	if err := config.SaveJSON(prev, a.cfg.Get()); err != nil {
		return BackupImport{}, i18n.Wrap(err, "err.backupKeepOld")
	}
	if err := a.cfg.Update(func(c *config.Config) { *c = cfg }); err != nil {
		return BackupImport{}, err
	}
	a.log.Infof("Restored backup %s (darkbot %s, %s): %d files in place, %d unpacked to %s; previous settings kept as %s",
		path, rep.Version, rep.Created.Local().Format(time.DateTime), rep.Kept, rep.Unpacked, a.cfg.RestoredFilesDir(), prev)
	for _, p := range rep.Missing {
		a.log.Warnf("Backup: %s was not in the backup; its setting was emptied and turned off", p)
	}
	a.emitState()
	return BackupImport{Path: path, Report: rep, Previous: prev}, nil
}

// RestartApp restarts darkbot (after restoring a backup), like an update
// does.
func (a *App) RestartApp() error {
	a.log.Infof("Restarting")
	a.stopAll()
	var args []string
	if startedInTray {
		args = append(args, autostart.TrayArg)
	}
	return a.uiErr(selfupdate.Restart(args...))
}
