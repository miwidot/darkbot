// darkbot: a tiny cross-platform Twitch channel-points sound bot.
package main

import (
	"embed"
	"log"
	"net/http"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/miwi/darkbot/internal/applog"
)

//go:embed all:frontend/dist
var assets embed.FS

const (
	windowWidth     = 980
	windowHeight    = 720
	windowMinWidth  = 760
	windowMinHeight = 560
)

func main() {
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
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []any{app},
	})
	if err != nil {
		log.Fatalf("darkbot: %v", err)
	}
}
