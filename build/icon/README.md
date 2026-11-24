# darkbot icons

`icon.py` draws every icon from one design: a graphite console tile with a
2x2 grid of sound pads, one lit by the amber LED (darkbot's accent color),
like a pad that is playing in the app.

Run `make icons` (needs `rsvg-convert`, Pillow and macOS `iconutil`). It writes:

| File | Used for |
|---|---|
| `build/icon/appicon.svg`, `appicon.png` | the full-bleed source (README, website) |
| `build/icon/appicon-macos.svg` | macOS variant on Apple's icon grid (inset, with shadow) |
| `build/darwin/icon.icns` | macOS app bundle (`make mac` copies it to `Contents/Resources`) |
| `build/icon/icon.ico` | Windows exe and window icon, via `rsrc_windows_amd64.syso` (`make winres`) |
| `internal/tray/icon.ico` | Windows tray icon (embedded by `internal/tray`) |
| `build/linux/icon.png` | Linux window icon (embedded in `main.go`) |

Sizes up to 32 px use a simplified drawing (lighter pads, wider gaps,
stronger rim) so the grid still reads in the taskbar and tray.
