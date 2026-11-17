# darkbot

**A small, open-source Twitch channel points sound bot for Windows, macOS and Linux.**

Viewers redeem a channel point reward, darkbot plays the matching sound. That's the whole idea.
darkbot does the sound part of tools like Streamer.bot, and nothing else: one window, no scripting, no account, no server.

## What it does

- **Turns audio files into channel point rewards.** Pick a file, give it a name, a price and a color, and darkbot creates the reward on your Twitch channel. Renaming, repricing, recoloring, switching it off and deleting are all done from darkbot.
- **Works with rewards you already have.** You can also attach a sound to an existing reward. darkbot then only listens and doesn't change that reward.
- **Cooldowns.** Set a cooldown for everyone and a separate one per viewer. For rewards darkbot created, Twitch shows the cooldown to viewers too (from 60 seconds up).
- **Fair to viewers.** If a sound can't play (per-viewer cooldown, switched off, too many sounds waiting, file error), darkbot rejects the redemption instead of silently eating it, and Twitch returns the points. This only works for rewards darkbot created, because Twitch only lets the creating app change redemptions.
- **One sound at a time.** Redemptions queue up and play in order (max 20 waiting).
- **Choose where sounds play:**
  - *Audio device:* darkbot plays the sound itself on the output you pick, like Streamer.bot or StreamElements. Capture that output, or darkbot itself, in OBS.
  - *Inside OBS:* darkbot plays the sound in an OBS media source and routes it to the audio tracks 1–6 you choose, for example stream only, or stream and recording but not your VOD track.
- **Song requests from chat (Spotify).** Viewers type `!sr <Spotify link or song name>` and the song goes into your Spotify queue. `!song`, `!next` and `!queue` show what plays; `!skip` is for you, your moderators and (optional) whoever requested the song. Per-viewer cooldown, maximum length and an explicit filter are adjustable. Uses your own Spotify app, see [Song requests](#song-requests-spotify).
- **Channel events with actions.** Follows, subs, resubs, gifted subs, Bits, raids, hype trains (start, level ups, end), shoutouts, ad breaks and stream start/end appear live in the activity feed. For each you can play a sound and/or post a chat message (`{user}`, `{amount}`, `{tier}`, `{months}`, `{top}` for the biggest hype train contributor), optionally only from a minimum amount or level. A sub bomb runs one action for the gifter, not one per recipient. Twitch has no unfollow event; an opt-in check compares your follower list every 10 minutes (up to 5,000 followers).
- **Dashboard.** The start page counts redemptions, chat commands and song requests per session and in total, ranks the most redeemed sounds and most active viewers, and shows the state of your Twitch and Spotify logins (valid until, next automatic renewal, how long the login lasts) with a renew button.
- **Starts with your computer** if you want (Windows, macOS, Linux; per user, no admin rights). On Windows it starts quietly in the tray.
- **Lives in the Windows tray.** Closing the window keeps darkbot running next to the clock (can be switched off); click the icon to open it, the menu installs updates and quits. Starting darkbot a second time just shows the running one, so no sound plays twice.
- **Live view.** Every sound is a pad. It lights up while playing and shows its cooldown draining. An activity feed lists who redeemed what and why something didn't play.
- **Explains itself.** Every setting has a ? tooltip that says what it does and when you need it.
- **English and German.** darkbot follows your system language, or you pick one under Settings, General. All texts live in one file per language (`internal/i18n/locales/`), so adding a language means translating one JSON file.
- **Updates itself.** On start, darkbot installs a newer release from GitHub automatically (can be switched off). While it's running, it checks every 30 minutes and only announces updates (in the window and the tray menu), so a stream is never interrupted. Every download is verified against GitHub's SHA-256 digest.

## What it deliberately doesn't do

- No open network ports, no local web server, no browser source. It's a desktop app; the window *is* the app.
- No general chat bot: the only chat commands are the song requests, and event messages are short thank-yous you write yourself. No overlays.
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
   Nothing to register: darkbot comes with its own Twitch Client ID. You only need your own if you build a fork (see [Build from source](#build-from-source)).
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

## Song requests (Spotify)

Spotify only lets an app in Development Mode serve 5 users, and the extended quota is reserved for companies with at least 250,000 monthly users ([Spotify quota modes](https://developer.spotify.com/documentation/web-api/concepts/quota-modes)). So darkbot can't ship one shared Spotify app: **every streamer creates their own, which takes two minutes.**

1. Open the [Spotify developer dashboard](https://developer.spotify.com/dashboard), log in with the account you stream with and create an app.
2. Add the redirect URI `http://127.0.0.1:43117/callback` exactly like this.
3. Tick **Web API**, accept the terms, save.
4. Copy the app's **Client ID** into darkbot (Settings, Song requests) and click **Connect Spotify**.
5. Allow access in the browser. The browser then shows "This site can't be reached". **That's expected**, because darkbot opens no port. Copy the whole address from the address bar into darkbot and click **Finish connecting**. The code in that address only works together with a secret that never leaves darkbot (PKCE).
6. Reconnect Twitch once if darkbot asks, so it may read and answer chat (`user:read:chat`, `user:write:chat`).

Requirements and limits:

- Adding to the queue and skipping need **Spotify Premium** (Spotify Web API), and Spotify must be playing on one of your devices.
- The Spotify login lasts 6 months, then you connect once more.
- **Rights:** music you hold no rights to can get your VODs muted or your channel a DMCA strike on Twitch, and Spotify's developer terms don't allow broadcasting. darkbot only manages your queue; what you play is your decision.
- YouTube links are refused with a hint; only Spotify links and song names work.

## How the Twitch login stays alive

Docs: [device code flow](https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow), [refresh tokens](https://dev.twitch.tv/docs/authentication/refresh-tokens/), [validate tokens](https://dev.twitch.tv/docs/authentication/validate-tokens/).

- Access tokens last about 4 hours. darkbot refreshes them 15 minutes before they expire.
- The token is validated on start and every hour, as Twitch requires. A failed validation triggers a refresh.
- A Twitch API call that answers 401 refreshes once and retries.
- Device-flow refresh tokens are single-use. All refreshes share one lock, so parallel 401s cause exactly one refresh, and the new token is saved immediately.
- Network problems never log you out; darkbot retries. Only an explicit "invalid refresh token" from Twitch does.
- Refresh tokens of public apps expire 30 days after they were issued. Every refresh issues a new one, so you only log in again if darkbot didn't run for 30 days.

Permissions darkbot asks Twitch for, all read-only except where noted:

| Scope | What for |
|---|---|
| `channel:read:redemptions`, `channel:manage:redemptions` | receive redemptions; create and manage darkbot's own rewards, refund or complete their redemptions |
| `user:read:chat`, `user:write:chat` | read chat commands (`!sr`, `!skip`, ...), post replies and event messages |
| `moderator:read:followers` | follow events and the opt-in unfollow check |
| `channel:read:subscriptions` | subs, resubs, gifted subs, ended subs |
| `bits:read` | Bits |
| `channel:read:hype_train` | hype train start and end |
| `channel:read:ads` | ad breaks |
| `moderator:read:shoutouts` | shoutouts your channel receives |

Raids and stream start/end need no permission. After an update that adds permissions, darkbot asks you to reconnect Twitch once.

## Build from source

Requires Go 1.27+. darkbot uses [Wails v2](https://wails.io) with a plain HTML/CSS/JS frontend; no Node.js needed.

```bash
make test      # vet + tests with the race detector
make mac       # build/bin/darkbot.app   (macOS, Xcode command line tools)
make windows   # build/bin/darkbot.exe   (from any system, no C compiler needed)
make linux     # build/bin/darkbot-linux (on Linux: libgtk-3-dev libwebkit2gtk-4.1-dev)

make windows CLIENT_ID=xxxx VERSION=0.3.0   # your own Twitch Client ID (optional) and version
```

Using your own Twitch app (forks): create one at [dev.twitch.tv/console](https://dev.twitch.tv/console) (needs 2FA on your Twitch account): *Register your application*, category *Application Integration*, client type **Public**, OAuth redirect URL `http://localhost` (required by the form, never used). Never generate a secret, darkbot doesn't use one. Build with `CLIENT_ID=<id>`. darkbot keeps the ID in `twitch_client_id` in its `config.json`; the settings don't show it, since users never need to change it.


Local builds report version `dev` and never update themselves.
`wails build` isn't used because the Wails v2.11 binding generator crashes on Go 1.27; the frontend calls `window.go.main.App` directly.

### Releases

Push a tag like `v0.3.0`. The `release` workflow builds all platforms on GitHub's free runners for public repositories and publishes the files the self-updater looks for. The repository variable `TWITCH_CLIENT_ID` overrides the built-in Client ID (optional).

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
internal/spotify/      Spotify PKCE login (paste flow, no port) and Web API client
internal/songs/        chat commands !sr !song !next !queue !skip
internal/secret/       OS keyring storage with 0600 file fallback
internal/i18n/         UI languages: locales/en.json, de.json (shared by Go and the UI)
```

Settings live in `%AppData%\darkbot`, `~/Library/Application Support/darkbot` or `~/.config/darkbot`.

## License

MIT, see [LICENSE](LICENSE). The bundled Archivo font is licensed under the SIL Open Font License 1.1 (`frontend/dist/fonts/OFL.txt`).

---

powered by [Tarkov Stammtisch](https://tarkov-stammtisch.de)
