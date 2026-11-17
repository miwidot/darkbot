//go:build windows

package tray

import (
	"runtime"
	"sync"

	"fyne.io/systray"
)

// Supported reports whether this system shows darkbot in the tray.
func Supported() bool { return true }

var (
	mu      sync.Mutex
	started bool
	ready   bool
	texts   Texts
	items   struct{ open, update, quit *systray.MenuItem }
)

// Start shows the tray icon. Windows delivers a window's messages only to
// the thread that created it, so the tray gets its own locked OS thread
// with its own message loop, apart from the webview's.
func Start(t Texts, h Handlers) {
	mu.Lock()
	if started {
		mu.Unlock()
		return
	}
	started, texts = true, t
	mu.Unlock()
	go func() {
		runtime.LockOSThread()
		systray.Run(func() { onReady(h) }, nil)
	}()
}

func onReady(h Handlers) {
	systray.SetIcon(icon)
	systray.SetOnTapped(h.Open) // left click: open; right click: menu
	mu.Lock()
	items.open = systray.AddMenuItem("", "")
	items.update = systray.AddMenuItem("", "")
	systray.AddSeparator()
	items.quit = systray.AddMenuItem("", "")
	ready = true
	apply(texts)
	mu.Unlock()
	for {
		select {
		case <-items.open.ClickedCh:
			h.Open()
		case <-items.update.ClickedCh:
			h.Update()
		case <-items.quit.ClickedCh:
			h.Quit()
		}
	}
}

// SetTexts updates labels, tooltip and the update entry.
func SetTexts(t Texts) {
	mu.Lock()
	defer mu.Unlock()
	texts = t
	if ready {
		apply(t)
	}
}

// apply needs mu held and the menu built.
func apply(t Texts) {
	systray.SetTooltip(t.Tooltip)
	items.open.SetTitle(t.Open)
	items.quit.SetTitle(t.Quit)
	if t.Update == "" {
		items.update.Hide()
		return
	}
	items.update.SetTitle(t.Update)
	items.update.Show()
}

// Stop removes the icon (otherwise it lingers until the mouse passes over).
func Stop() {
	mu.Lock()
	on := started
	mu.Unlock()
	if on {
		systray.Quit()
	}
}
