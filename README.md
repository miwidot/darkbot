# darkbot

A deliberately small Twitch **channel points → sound** bot for Windows, macOS and Linux.
Think "Streamer.bot, but only the sound part".

- Native desktop window (Wails, same stack as `streamerchat`), **no open network ports**
- Twitch login via device code (no client secret, no callback server)
- Channel point redemptions via **EventSub WebSocket** (outgoing connection only)
- Per-sound volume, **global cooldown** and **per-viewer cooldown**
- Sounds play one after another (queue, max 20 waiting)
- Two output modes, chosen globally:
  - **Audio device** – plays in the app on the selected output device (like Streamer.bot / StreamElements). Capture it in OBS.
  - **OBS media source** – plays inside OBS via obs-websocket on the selected **audio tracks 1–6**

## Setup

1. **Twitch app** – [dev.twitch.tv/console](https://dev.twitch.tv/console) → *Register Your Application*
   - OAuth Redirect URL: `http://localhost` (required by the form, not used)
   - Category: *Application Integration*, **Client Type: Public**
   - Copy the **Client ID** into darkbot → *Connections* → *Log in with Twitch*, confirm the code on twitch.tv/activate.
   - Channel points require an **Affiliate or Partner** channel.
2. **Sounds** – *Sounds* → *Add sound*: pick a reward, a file (mp3, wav, ogg, flac), volume and cooldowns.
3. **Output** – *Audio output*:
   - *Audio device*: choose the output. Get it into OBS with *Audio Output Capture* (for a virtual cable / dedicated device) or capture darkbot itself with *Application Audio Capture* (Windows) / *macOS Audio Capture → Application* (macOS).
   - *OBS media source*: enable OBS → *Tools → WebSocket Server Settings*, enter host/port/password under *Connections*, tick the tracks. darkbot creates the media source `darkbot-sound` in the current scene. OBS only plays sources of the active scene, so add it to your other scenes too (*Add → Existing source*).

### Output device selection per OS

| OS | Device picker | Notes |
|---|---|---|
| Windows (WebView2) | yes | Device names appear after allowing microphone access once (*Refresh* button). Nothing is recorded; the stream is closed immediately. |
| macOS (WKWebView) | no – system default | WebKit hides the output list. Use *macOS Audio Capture → Application: darkbot* in OBS, or OBS mode. |
| Linux (WebKitGTK) | usually no – system default | Use PipeWire/Pulse routing or OBS mode. |

## Twitch token: how auto refresh works

Docs: [refresh tokens](https://dev.twitch.tv/docs/authentication/refresh-tokens/), [validate tokens](https://dev.twitch.tv/docs/authentication/validate-tokens/), [device code flow](https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow).

- Access tokens from the device flow live ~4 hours. darkbot refreshes **15 minutes before expiry** (checked every minute).
- The token is **validated on start and every hour** (mandatory per Twitch). A 401 there triggers a refresh.
- Any Helix call that gets a 401 refreshes once and retries.
- Device-flow refresh tokens are **single-use**: every refresh returns a new one, which is saved immediately. All refresh paths share one lock, and concurrent 401s cause exactly one refresh (tested).
- Network errors never log you out, they are retried. Only an explicit 400/401 from Twitch ("Invalid refresh token") logs you out.
- Public-client refresh tokens expire **30 days after they were issued**. Because each refresh issues a new one, you only need to log in again if darkbot didn't run for 30 days.
- Tokens are stored in the OS keyring (Windows Credential Manager / macOS Keychain / Secret Service); fallback: `token.json` (0600) in the config dir.

## Cooldowns

darkbot checks cooldowns itself, so it works with **any existing reward**. A redemption during cooldown is logged and ignored (no sound).
Twitch only lets the app that *created* a reward refund points or set Twitch-side cooldowns, so darkbot doesn't refund. For a visible cooldown on Twitch, set the reward's cooldown in the Twitch dashboard as well.

## Build

```bash
make test      # vet + tests (race detector)
make mac       # build/bin/darkbot.app   (macOS, Xcode CLT)
make windows   # build/bin/darkbot.exe   (any OS, no C compiler needed)
make linux     # build/bin/darkbot-linux (on Linux: libgtk-3-dev libwebkit2gtk-4.1-dev)
make windows CLIENT_ID=xxxx   # bake a Twitch Client ID into the build
```

`wails build` isn't used: the Wails v2.11 CLI binding generator crashes on Go 1.27, and darkbot calls bindings via `window.go.main.App` directly. CI (`.github/workflows/build.yml`) builds all three platforms on native runners.

## Files

Config: `%AppData%\darkbot\config.json` / `~/Library/Application Support/darkbot/config.json` / `~/.config/darkbot/config.json` (0600, contains the OBS password).

## Layout

```
main.go, app.go          Wails window, bindings, sound file handler (in-process, no port)
frontend/dist/           vanilla HTML/CSS/JS UI, audio playback (setSinkId)
internal/twitch/         device login, token manager (auto refresh), Helix, EventSub WebSocket
internal/bot/            reward → sound matching, cooldowns, play queue
internal/obs/            obs-websocket v5 client + media-source output
internal/audio/          pure-Go decoding for file validation and length
internal/config/         JSON config (atomic writes)
```
