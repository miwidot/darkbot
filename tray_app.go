package main

import (
	"context"
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/tray"
)

// Tray icon (Windows): closing the window hides darkbot to the tray so
// sounds, chat and events keep running; "Quit" in the tray menu ends it.

// trayState is kept outside App's mutex: OnBeforeClose and the tray menu
// run on their own threads.
type trayState struct {
	quitting  atomic.Bool  // the user chose Quit: let the window close
	hintShown atomic.Bool  // "still running in the tray" was logged
	last      atomic.Value // tray.Texts last sent, to skip identical updates
}

func (a *App) startTray() {
	if !tray.Supported() {
		return
	}
	tray.Start(a.trayTexts(), tray.Handlers{
		Open: a.showWindow,
		Update: func() {
			a.showWindow()
			go func() {
				if err := a.installUpdate(); err != nil {
					a.log.Errorf("Update from the tray failed: %v", err)
				}
			}()
		},
		Quit: a.quit,
	})
}

func (a *App) trayTexts() tray.Texts {
	lang := a.lang()
	t := tray.Texts{Tooltip: "darkbot", Open: i18n.T(lang, "tray.open"), Quit: i18n.T(lang, "tray.quit")}
	a.mu.Lock()
	up := a.update
	a.mu.Unlock()
	if up != nil && up.Available {
		t.Update = i18n.T(lang, "tray.update", "version", up.Latest)
		t.Tooltip = i18n.T(lang, "tray.tooltipUpdate", "version", up.Latest)
	}
	return t
}

// refreshTray updates the tray after state changes (language, update found).
func (a *App) refreshTray() {
	if !tray.Supported() {
		return
	}
	t := a.trayTexts()
	if prev, ok := a.tray.last.Load().(tray.Texts); ok && prev == t {
		return
	}
	a.tray.last.Store(t)
	tray.SetTexts(t)
}

// showWindow brings the window back (tray click, second start of darkbot).
func (a *App) showWindow() {
	if a.ctx == nil {
		return
	}
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
}

func (a *App) quit() {
	a.tray.quitting.Store(true)
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// beforeClose hides the window instead of quitting while the tray icon is
// there and the user wants that (default).
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if !a.hideOnClose(tray.Supported()) {
		return false
	}
	runtime.WindowHide(ctx)
	if !a.tray.hintShown.Swap(true) {
		a.log.Infof("darkbot keeps running in the tray (notification area). Quit it from the tray menu")
	}
	return true
}

// hideOnClose decides what the window's X does.
func (a *App) hideOnClose(haveTray bool) bool {
	return haveTray && !a.tray.quitting.Load() && !a.cfg.Get().QuitOnClose
}

// onSecondInstance: darkbot was started again while running (possibly
// hidden in the tray). Show the running one instead of a second bot that
// would play every sound twice.
func (a *App) onSecondInstance(options.SecondInstanceData) {
	a.log.Infof("darkbot was started again: showing the running window")
	a.showWindow()
}

// SetQuitOnClose: true = closing the window quits darkbot, false = it keeps
// running in the tray.
func (a *App) SetQuitOnClose(quit bool) error {
	err := a.cfg.Update(func(c *config.Config) { c.QuitOnClose = quit })
	a.emitState()
	return a.uiErr(err)
}
