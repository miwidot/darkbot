# darkbot build targets.
#
# We call `go build` directly instead of `wails build`: the Wails v2.11 CLI's
# binding generator crashes with Go 1.27 ("package fmt without types"), and
# darkbot doesn't need generated bindings (the frontend uses window.go.*).
#
#   make mac      -> build/bin/darkbot.app      (on macOS, needs Xcode CLT)
#   make windows  -> build/bin/darkbot.exe      (from ANY OS, no C compiler)
#   make linux    -> build/bin/darkbot-linux    (on Linux, needs GTK3 + WebKit2GTK 4.1 dev packages)
#   make test
#
# Icons: build/icon/icon.py draws them all (see build/icon/README.md).
# Windows takes its exe and window icon from rsrc_windows_amd64.syso, which
# `go build` links in by itself; regenerate it with `make winres`.

TAGS      := desktop,production
# VERSION is baked in for the self-updater; local builds stay "dev" and never
# update themselves. Releases: make windows VERSION=0.2.0
VERSION   ?= dev
LDFLAGS   := -w -s -X github.com/miwidot/darkbot/internal/version.Version=$(VERSION)
# Optional: replace darkbot's built-in Twitch Client ID (app.go) with your own:
#   make windows CLIENT_ID=abc123
ifdef CLIENT_ID
LDFLAGS += -X main.defaultClientID=$(CLIENT_ID)
endif
BIN       := build/bin
APP       := $(BIN)/darkbot.app

.PHONY: all mac windows linux test clean icons winres

all: test

mac:
	mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources
	cp build/darwin/Info.plist $(APP)/Contents/Info.plist
	cp build/darwin/icon.icns $(APP)/Contents/Resources/icon.icns
	CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
		go build -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(APP)/Contents/MacOS/darkbot .

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -tags $(TAGS) -ldflags "$(LDFLAGS) -H windowsgui" -o $(BIN)/darkbot.exe .

linux:
	go build -tags $(TAGS),webkit2_41 -ldflags "$(LDFLAGS)" -o $(BIN)/darkbot-linux .

test:
	go vet ./internal/...
	go test -race ./internal/...
	CGO_LDFLAGS="$(if $(filter Darwin,$(shell uname -s)),-framework UniformTypeIdentifiers,)" go test -tags $(TAGS) .

# Redraws every icon (needs rsvg-convert, Pillow and macOS iconutil), then
# rebuilds the Windows resource object.
icons:
	python3 build/icon/icon.py
	$(MAKE) winres

# Windows icon resource. ID 3 is the one Wails loads as the window icon
# (winc.AppIconID); Explorer shows the same group for the exe.
winres:
	go run github.com/tc-hib/go-winres@v0.3.3 make --in build/windows/winres.json --arch amd64 --out rsrc

clean:
	rm -rf $(BIN)
