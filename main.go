// darkbot: a tiny cross-platform Twitch channel-points sound bot.
package main

import (
	"embed"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/autostart"
	"github.com/miwidot/darkbot/internal/selfupdate"
	"github.com/miwidot/darkbot/internal/tray"
)

//go:embed all:frontend/dist
var assets embed.FS

const (
	windowWidth     = 980
	windowHeight    = 720
	windowMinWidth  = 760
	windowMinHeight = 560
	// singleInstanceID names the lock that keeps darkbot from running twice
	// (two instances would play every sound and answer every command twice).
	// Windows: a named mutex, macOS/Linux: a lock file and D-Bus; no ports.
	singleInstanceID = "com.miwidot.darkbot"
)

// startedInTray: started by autostart with the tray argument, so the window
// starts hidden. Kept across a self-update restart.
var startedInTray bool

func main() {
	for _, arg := range os.Args[1:] {
		switch {
		case arg == autostart.TrayArg:
			startedInTray = tray.Supported()
		case strings.HasPrefix(arg, selfupdate.WaitArg):
			// Restarted after an update: the old process must be gone before
			// the single-instance lock is taken.
			if pid, err := strconv.Atoi(strings.TrimPrefix(arg, selfupdate.WaitArg)); err == nil && pid > 0 {
				if !selfupdate.WaitForExit(pid, selfupdate.RestartWait) {
					log.Printf("darkbot: previous process %d still running after %s", pid, selfupdate.RestartWait)
				}
			}
		}
	}
	logs := applog.New()
	app, err := NewApp(logs)
	if err != nil {
		log.Fatalf("darkbot: %v", err)
	}

	err = wails.Run(&options.App{
		Title:     "darkbot",
		Width:     windowWidth,
		Height:    windowHeight,
		MinWidth:  windowMinWidth,
		MinHeight: windowMinHeight,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: http.HandlerFunc(app.serveSound),
		},
		BackgroundColour: &options.RGBA{R: 14, G: 14, B: 18, A: 255},
		StartHidden:      startedInTray,
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		OnBeforeClose:    app.beforeClose,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.onSecondInstance,
		},
		Bind: []any{app},
	})
	if err != nil {
		log.Fatalf("darkbot: %v", err)
	}
}
