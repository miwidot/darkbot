# darkbot

**A small, open-source Twitch channel points sound bot for Windows, macOS and Linux.**

Viewers redeem a channel point reward, darkbot plays the matching sound. That's the whole idea.
darkbot does the sound part of tools like Streamer.bot, and nothing else: one window, no scripting, no account, no server.

## What it does

- **Turns audio files into channel point rewards.** Pick a file, give it a name and a price, and darkbot creates the reward on your Twitch channel. Renaming, repricing, switching it off and deleting are all done from darkbot.
- **Works with rewards you already have.** You can also attach a sound to an existing reward. darkbot then only listens and doesn't change that reward.
- **Cooldowns.** Set a cooldown for everyone and a separate one per viewer. For rewards darkbot created, Twitch shows the cooldown to viewers too (from 60 seconds up).
- **Fair to viewers.** If a sound can't play (per-viewer cooldown, switched off, too many sounds waiting, file error), darkbot rejects the redemption instead of silently eating it, and Twitch returns the points. This only works for rewards darkbot created, because Twitch only lets the creating app change redemptions.
- **One sound at a time.** Redemptions queue up and play in order (max 20 waiting).
- **Choose where sounds play:**
  - *Audio device:* darkbot plays the sound itself on the output you pick, like Streamer.bot or StreamElements. Capture that output, or darkbot itself, in OBS.
  - *Inside OBS:* darkbot plays the sound in an OBS media source and routes it to the audio tracks 1–6 you choose, for example stream only, or stream and recording but not your VOD track.
- **Live view.** Every sound is a pad. It lights up while playing and shows its cooldown draining. An activity feed lists who redeemed what and why something didn't play.
- **Updates itself.** On start, darkbot installs a newer release from GitHub automatically (can be switched off). While it's running, it only announces updates so a stream is never interrupted. Every download is verified against GitHub's SHA-256 digest.

## What it deliberately doesn't do

- No open network ports, no local web server, no browser source. It's a desktop app; the window *is* the app.
- No chat bot, no commands, no alerts, no overlays. Other tools do that well.
- No cloud, no telemetry, no account. Your settings stay on your computer; the Twitch login lives in the OS keychain.

## Install

Download the file for your system from [Releases](https://github.com/miwidot/darkbot/releases):

| System | File |
|---|---|
| Windows 10/11 | `darkbot-windows-amd64.exe` (needs the WebView2 runtime, preinstalled on Windows 11) |
| macOS, Apple Silicon | `darkbot-macos-arm64.app.zip` |
| macOS, Intel | `darkbot-macos-amd64.app.zip` |
| Linux | `darkbot-linux-amd64` (needs GTK 3 and WebKitGTK 4.1) |

Builds aren't code-signed yet. On macOS right-click darkbot and choose *Open* the first time. On Windows choose *More info*, then *Run anyway*.

## First start

1. **Connect Twitch.** Open Settings (gear icon), then Twitch, then *Connect Twitch*, and confirm the code on twitch.tv/activate. Channel points need an Affiliate or Partner channel.
   If your build has no built-in Client ID, create one at [dev.twitch.tv/console](https://dev.twitch.tv/console): *Register your application*, client type **Public**, OAuth redirect URL `http://localhost` (required by the form, not used). Paste the Client ID under *Twitch application*.
2. **Add a sound.** *Add sound*, then choose a file (mp3, wav, ogg, flac), a reward name and a price. Click a pad to preview it.
3. **Choose the output.** Settings, then Audio output:
   - *Audio device:* to get the sound into OBS, pick a virtual cable or spare output and add it as *Audio Output Capture*. Or keep the default and add darkbot with *Application Audio Capture* (Windows) or *macOS Audio Capture* (macOS).
   - *Inside OBS:* in OBS open Tools, then WebSocket Server Settings, and enable it. Enter host, port and password in Settings, then OBS. Tick the tracks. darkbot creates the media source `darkbot-sound` in the current scene. OBS only plays sources of the active scene, so add it to your other scenes as an existing source.

### Picking an output device per system

| System | Device list | Notes |
|---|---|---|
| Windows (WebView2) | yes | Device names show after allowing microphone access once (*Refresh*). Nothing is recorded; darkbot closes the microphone immediately. |
| macOS (WKWebView) | no, system default | WebKit doesn't expose the output list. Capture darkbot in OBS with *macOS Audio Capture*, or use *Inside OBS*. |
| Linux (WebKitGTK) | usually no, system default | Use PipeWire/PulseAudio routing or *Inside OBS*. |

## How the Twitch login stays alive

Docs: [device code flow](https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow), [refresh tokens](https://dev.twitch.tv/docs/authentication/refresh-tokens/), [validate tokens](https://dev.twitch.tv/docs/authentication/validate-tokens/).

- Access tokens last about 4 hours. darkbot refreshes them 15 minutes before they expire.
- The token is validated on start and every hour, as Twitch requires. A failed validation triggers a refresh.
- A Twitch API call that answers 401 refreshes once and retries.
- Device-flow refresh tokens are single-use. All refreshes share one lock, so parallel 401s cause exactly one refresh, and the new token is saved immediately.
- Network problems never log you out; darkbot retries. Only an explicit "invalid refresh token" from Twitch does.
- Refresh tokens of public apps expire 30 days after they were issued. Every refresh issues a new one, so you only log in again if darkbot didn't run for 30 days.

Scopes: `channel:read:redemptions` (receive redemptions, list rewards) and `channel:manage:redemptions` (create and manage darkbot's own rewards, fulfill or reject their redemptions).

## Build from source

Requires Go 1.27+. darkbot uses [Wails v2](https://wails.io) with a plain HTML/CSS/JS frontend; no Node.js needed.

```bash
make test      # vet + tests with the race detector
make mac       # build/bin/darkbot.app   (macOS, Xcode command line tools)
make windows   # build/bin/darkbot.exe   (from any system, no C compiler needed)
make linux     # build/bin/darkbot-linux (on Linux: libgtk-3-dev libwebkit2gtk-4.1-dev)

make windows CLIENT_ID=xxxx VERSION=0.3.0   # bake in a Twitch Client ID and version
```

Local builds report version `dev` and never update themselves.
`wails build` isn't used because the Wails v2.11 binding generator crashes on Go 1.27; the frontend calls `window.go.main.App` directly.

### Releases

Push a tag like `v0.3.0`. The `release` workflow builds all platforms on GitHub's free runners for public repositories and publishes the files the self-updater looks for. Set the repository variable `TWITCH_CLIENT_ID` to bake a Client ID into releases.

## Project layout

```
main.go, app.go        window, bindings, sound file handler (in-process, no port)
frontend/dist/         UI (HTML/CSS/JS), audio playback, Archivo font (OFL)
internal/twitch/       device login, token manager, Helix, EventSub WebSocket
internal/bot/          reward to sound matching, cooldowns, play queue
internal/obs/          obs-websocket v5 client and media source output
internal/selfupdate/   verified GitHub release updates
internal/audio/        pure-Go decoding for validation and length
internal/config/       settings file (atomic writes)
```

Settings live in `%AppData%\darkbot`, `~/Library/Application Support/darkbot` or `~/.config/darkbot`.

## License

MIT, see [LICENSE](LICENSE). The bundled Archivo font is licensed under the SIL Open Font License 1.1 (`frontend/dist/fonts/OFL.txt`).
