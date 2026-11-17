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

TAGS      := desktop,production
# VERSION is baked in for the self-updater; local builds stay "dev" and never
# update themselves. Releases: make windows VERSION=0.2.0
VERSION   ?= dev
LDFLAGS   := -w -s -X github.com/miwidot/darkbot/internal/version.Version=$(VERSION)
# Optional: bake a Twitch Client ID into release builds:
#   make windows CLIENT_ID=abc123
ifdef CLIENT_ID
LDFLAGS += -X main.defaultClientID=$(CLIENT_ID)
endif
BIN       := build/bin
APP       := $(BIN)/darkbot.app

.PHONY: all mac windows linux test clean

all: test

mac:
	mkdir -p $(APP)/Contents/MacOS
	cp build/darwin/Info.plist $(APP)/Contents/Info.plist
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

clean:
	rm -rf $(BIN)
