# darkbot

**A small, open-source Twitch channel points sound bot for Windows, macOS and Linux.**

Viewers redeem a channel point reward, darkbot plays the matching sound. That's the whole idea.
darkbot does the sound part of tools like Streamer.bot, and nothing else: one window, no scripting, no account, no server.

## What it does

- **Turns audio files into channel point rewards.** Pick a file, give it a name, a price and a color, and darkbot creates the reward on your Twitch channel. Renaming, repricing, recoloring, switching it off and deleting are all done from darkbot.
- **Works with rewards you already have.** You can also attach a sound to an existing reward. darkbot then only listens and doesn't change that reward.
- **Cooldowns.** Set a cooldown for everyone and a separate one per viewer. For rewards darkbot created, Twitch shows the cooldown to viewers too (from 60 seconds up). Rewards darkbot created can also be limited per stream, in total and per viewer (e.g. a loud sound 3 times per stream, or once per viewer); Twitch counts and enforces that itself.
- **Fair to viewers.** If a sound can't play (per-viewer cooldown, switched off, too many sounds waiting, file error), darkbot rejects the redemption instead of silently eating it, and Twitch returns the points. This only works for rewards darkbot created, because Twitch only lets the creating app change redemptions.
- **One sound at a time.** Redemptions, events, chat commands and tests share one queue and play in order (max 20 waiting), so sounds never mix.
- **Choose where sounds play:**
  - *Audio device:* darkbot plays the sound itself on the output you pick, like Streamer.bot or StreamElements. Capture that output, or darkbot itself, in OBS.
  - *Inside OBS:* darkbot plays the sound in an OBS media source and routes it to the audio tracks 1–6 you choose, for example stream only, or stream and recording but not your VOD track.
- **Song requests from chat (Spotify and YouTube Music).** Viewers type `!sr <song link or name>` and the song goes into your queue. A Spotify link goes to Spotify, a YouTube or YouTube Music link to YouTube Music, a name is searched on the service you pick. `!song`, `!next` and `!queue` show what plays on the service that is playing; `!skip` is for you, your moderators and (optional) whoever requested the song. Per-viewer cooldown, maximum length and an explicit filter are adjustable. See [Song requests](#song-requests).
- **Alerts for channel events.** The Alerts tab groups them into Support, Hype & goals, Polls & predictions and Channel; the number on each tab counts the alerts that are on. Follows, subs, resubs, gifted subs, Bits (cheers and Bits spent on Power-ups, Twitch's and your own; `{powerup}` names it), raids, watch streaks shared in chat, hype trains (start, level ups, end), shoutouts, ad breaks and stream start/end appear live in the activity feed. For each you can play a sound and/or post a chat message (`{user}`, `{amount}`, `{tier}`, `{months}`, `{top}` for the biggest hype train contributor, `{points}` for the channel points of a watch streak), optionally only from a minimum amount or level. A sub bomb runs one action for the gifter, not one per recipient. Twitch has no unfollow event; an opt-in check compares your follower list every 10 minutes (up to 5,000 followers). darkbot keeps that list, so after a start it also lists follows and unfollows from while it was off (marked, without sound or chat message).
- **More alerts:** Twitch's own channel point rewards (highlighted message, emote unlocks; `{reward}`), title or category changes (`{title}`, `{category}`), goals started and reached, polls and predictions (started, locked, resolved; `{title}`, `{winner}`), and an **ad warning** a set time before the next automatic ad (Twitch has no event for it; darkbot reads the ad schedule every 15 s while you're live). A **raid** can give the raider Twitch's real shoutout automatically.
- **Goal and poll overlays (OBS).** Switch them on in their Alerts tab: your Twitch goals as progress bars (top left) and the running poll or prediction with a countdown and its result (top right). Like Now Playing they are browser sources in darkbot's scene, fed over the OBS connection: no port.
- **Shared Chat aware.** During a Shared Chat session Twitch would send everything darkbot writes to every channel of the session, and with darkbot's kind of login that can't be changed. So darkbot writes nothing in chat until the session ends (sounds, alerts and clips keep working; the log lists what wasn't sent).
- **Chat commands and timers (off until you turn them on).** Your own commands like `!discord` or `!hug @name` with a chat answer (`{user}`, `{target}`), an optional sound, other names, who may use them (everyone, subscribers, VIPs, moderators, only you) and cooldowns. A command can also react to a word at the start of a message instead of `!name` (e.g. "hi", which also catches "Hi!" and "hallo," as another name), and answer with a random line from a text file you pick (one answer per line, re-read when you edit it, never the same line twice in a row). "Once per stream" answers each viewer only once until your next stream starts, which suits greetings. A command can also be the **greeting**: it answers every viewer's first chat message of the stream, whatever it says (only while you are live; never you or known bots; with text, a random line from a file and/or a sound, and who gets greeted, e.g. only subscribers). Your own messages never trigger a word. Timers post a message every few minutes while you are live, optionally only after some chat messages, never two at once. A timer can take turns with up to 20 messages (one per line, e.g. your socials): shuffled like a deck of cards, so each comes once per round and none twice in a row, or top to bottom. darkbot writes with your account and ignores its own messages, so a command can't trigger itself.
- **Import from Streamer.bot.** Paste a Streamer.bot import code (export your actions with their commands and timers) and darkbot shows what it can take over and why the rest not, then imports it. It takes commands and timers whose action only sends a chat message (plus, for commands, one sound): names and other names, cooldowns, who may use them (empty, Moderators, VIPs and Subscribers groups), `%user%`/`%userName%` as `{user}` and `%targetUser%` as `{target}`, and random timer messages. Code, other variables, chat commands like `/announce` (`/me` works), several actions or your own user groups are left out with the reason. Everything imported starts turned off, so nothing answers twice while Streamer.bot still runs.
- **Built-in commands `!marker` and `!snooze` (off until you turn them on, moderators only by default).** `!marker` sets a stream marker so you find the moment in the VOD (text after it becomes the description); `!snooze` pushes the next automatic ad back 5 minutes and says how many snoozes are left. `!uptime` says how long the stream has been live (read from Twitch, so it is right even if darkbot started later) and `!followage` how long someone has been following (`!followage @name` for someone else); both are for everyone by default, with a 30-second cooldown. A built-in command that is off leaves its name to your own commands.
- **Clips from chat (`!clips`, off until you turn it on).** A viewer types `!clips` and darkbot makes a clip of the last 60 seconds of your stream, waits until Twitch has it and posts the link in chat; the clip also shows in the activity feed. You choose the command's name, who may use it and a cooldown for everyone (you and your mods skip it), and optionally a title (`{user}`; empty keeps your stream title). Only one clip is made at a time, so a whole chat typing `!clips` gets one clip. Clips only work while you're live; darkbot says so in chat otherwise, and also when the category can't be clipped or clips are off in your channel. Needs the Twitch permission `clips:edit` (darkbot asks you to reconnect Twitch once).
- **Alerts in OBS, without a port.** Each event can show a picture, animated GIF or video (WebM with transparency works) and a text like "{user} is following!" in OBS, together with its sound. One click in Settings, OBS creates a scene called `[darkbot]` that holds everything darkbot shows or plays, and adds it on top of each of your scenes; nothing else in your scenes changes. New scenes are noticed and offered. *Remove from OBS* takes it all out again.
- **Now Playing overlay (OBS).** Shows the song Spotify or YouTube Music is playing in your stream, in one of six designs (Console, Glass, Lower third, Vinyl, Minimal, Compact), with cover, progress bar and, for song requests, who asked for it. The Now Playing tab previews all six live with your current song; one click picks the design. darkbot adds it as a browser source to its OBS scene and feeds it over the OBS connection: no port, no web server. Song titles are shown as text only, never run as HTML.
- **Dashboard.** The start page counts redemptions, chat commands and song requests per stream (kept across restarts and updates; a new stream starts a new count) and in total, ranks the most redeemed sounds and most active viewers, and shows the state of your Twitch and Spotify logins (valid until, next automatic renewal, how long the login lasts) with a renew button, and whether Pear Desktop answers when you use YouTube Music.
- **Statistics.** A tab of its own with top chatters, top subs (gifted subs count for the gifter), top Bits and top raiders, for this stream or the last 7, 30, 60 or 90 days, and a chart of chat messages: per hour for a week (with messages and chatters per day), per day for longer ranges. Bots never count: a built-in list (Nightbot, StreamElements, Streamlabs, Moobot, Fossabot and others) plus your own list; you don't count either, because darkbot writes as you. Twitch marks bots in chat only when they use an app login, so it goes by name. It stays small: finished days go to their own file that is written once a day, a day keeps at most 1,000 people, and days older than a week only their 200 most active (the top lists stay right; "chatters" over a long range becomes "at least"). Measured with 1,000 different chatters every day for 90 days: about 5 MB of memory and a 1.1 MB history file.
- **Starts with your computer** if you want (Windows, macOS, Linux; per user, no admin rights). On Windows it starts quietly in the tray.
- **Lives in the Windows tray.** Closing the window keeps darkbot running next to the clock (can be switched off); click the icon to open it, the menu installs updates and quits. Starting darkbot a second time just shows the running one, so no sound plays twice.
- **Live view.** Every sound is a pad. It lights up while playing and shows its cooldown draining. An activity feed lists who redeemed what, why something didn't play, and every channel event. It keeps the last 200 lines across restarts and updates and can be cleared.
- **Explains itself.** Every setting has a ? tooltip that says what it does and when you need it.
- **English and German.** darkbot follows your system language, or you pick one under Settings, General. All texts live in one file per language (`internal/i18n/locales/`), so adding a language means translating one JSON file.
- **Updates itself.** On start, darkbot installs a newer release from GitHub automatically (can be switched off). While it's running, it checks every 30 minutes and only announces updates (in the window and the tray menu), so a stream is never interrupted. Every download is verified against GitHub's SHA-256 digest.

## What it deliberately doesn't do

- No open network ports, no local web server, no browser source. It's a desktop app; the window *is* the app. Alerts reach OBS through OBS's own websocket, which darkbot connects to.
- No moderation bot: no timeouts, bans, filters or loyalty points. Chat commands answer with text you wrote, and viewers can never make your account post their own words (only their name, or a valid Twitch login for `{target}`).
- No chat spam: everything darkbot posts counts against one budget (60 messages per 30 s, Twitch mutes accounts at 100), thank-you messages for events against their own (15 per 30 s, so a follow-bot wave can't flood your chat), and song requests are capped at 10 per minute for the whole channel.
- No cloud, no telemetry, no account. Your settings stay on your computer; the Twitch, Spotify and Pear Desktop logins and the OBS password live in the OS keychain (a 0600 file only where no keychain is available). Logging out also invalidates the Twitch login at Twitch.

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
   - *Inside OBS:* in OBS open Tools, then WebSocket Server Settings, and enable it. Enter host, port and password in Settings, then OBS. Tick the tracks. darkbot creates the media source `[darkbot] sound`. With the `[darkbot]` scene set up (Settings, OBS) it plays in all your scenes; otherwise it is created in the current scene, and OBS only plays sources of the active scene.

### Picking an output device per system

| System | Device list | Notes |
|---|---|---|
| Windows (WebView2) | yes | Device names show after allowing microphone access once (*Refresh*). Nothing is recorded; darkbot closes the microphone immediately. |
| macOS (WKWebView) | no, system default | WebKit doesn't expose the output list. Capture darkbot in OBS with *macOS Audio Capture*, or use *Inside OBS*. |
| Linux (WebKitGTK) | usually no, system default | Use PipeWire/PulseAudio routing or *Inside OBS*. |

## Song requests

Use Spotify, YouTube Music or both. Links always go to the service they belong to; `!sr <name>` searches on the service you choose in Settings, Song requests (or on the other one while that one isn't connected). `!song`, `!next`, `!queue` and `!skip` use the service that is playing right now.

Reconnect Twitch once if darkbot asks, so it may read and answer chat (`user:read:chat`, `user:write:chat`).

### Spotify

Spotify only lets an app in Development Mode serve 5 users, and the extended quota is reserved for companies with at least 250,000 monthly users ([Spotify quota modes](https://developer.spotify.com/documentation/web-api/concepts/quota-modes)). So darkbot can't ship one shared Spotify app: **every streamer creates their own, which takes two minutes.**

1. Open the [Spotify developer dashboard](https://developer.spotify.com/dashboard), log in with the account you stream with and create an app.
2. Add the redirect URI `http://127.0.0.1:43117/callback` exactly like this.
3. Tick **Web API**, accept the terms, save.
4. Copy the app's **Client ID** into darkbot (Settings, Song requests) and click **Connect Spotify**.
5. Allow access in the browser. The browser then shows "This site can't be reached". **That's expected**, because darkbot opens no port. Copy the whole address from the address bar into darkbot and click **Finish connecting**. The code in that address only works together with a secret that never leaves darkbot (PKCE).

Requirements and limits:

- Adding to the queue and skipping need **Spotify Premium** (Spotify Web API), and Spotify must be playing on one of your devices.
- The Spotify login lasts 6 months, then you connect once more.

### YouTube Music

YouTube Music has no public API for the queue. darkbot uses [Pear Desktop](https://github.com/pear-devs/pear-desktop) (formerly th-ch/youtube-music), the free open-source YouTube Music app for Windows, macOS and Linux. Its **API Server** plugin lets other programs search, queue and skip. darkbot connects to it on `127.0.0.1` as a client; darkbot itself still opens no port.

1. Install Pear Desktop and sign in to YouTube Music there.
2. In Pear Desktop open the Plugins menu and turn on **API Server [Beta]**.
3. In that plugin's menu set **Hostname** to `127.0.0.1` (the default `0.0.0.0` lets every device in your network control your player). Leave HTTPS off and the authorization strategy at **Authorize at first request**.
4. In darkbot (Settings, Song requests) tick **Take YouTube Music requests** and click **Connect YouTube Music**, then **Allow** in the window Pear Desktop shows. If you changed the port in Pear Desktop (default 26538), enter the same one in darkbot first.

Requirements and limits:

- Pear Desktop has to be running while you stream. If it isn't, viewers get a short "not available" answer and the dashboard says so.
- Links to single songs and videos work (`music.youtube.com/watch?v=…`, `youtube.com/watch?v=…`, `youtu.be/…`); playlist and channel links are refused.
- Pear Desktop answers "added" even for songs it can't add, so darkbot watches the queue until the song shows up (at most 5 seconds) and only then confirms in chat.
- The explicit filter catches what YouTube Music marks as explicit; music videos (as opposed to songs) carry no such mark there.
- Pear Desktop's API is part of the app, not a stable public interface; a future Pear Desktop version may change it. darkbot was tested with Pear Desktop 3.12.1.

### Both services

- **Rights:** music you hold no rights to can get your VODs muted or your channel a DMCA strike on Twitch, and neither Spotify's nor YouTube's terms allow broadcasting. darkbot only manages your queue; what you play is your decision.
- Song titles are named in chat answers by default. A title on Spotify or YouTube can be any text and darkbot posts from your own account, which AutoMod doesn't filter; switch off "Name song titles in chat" and darkbot answers without them.

## Now Playing overlay

1. Set up the darkbot scene in OBS once (Settings, OBS), if you haven't for alerts.
2. Open the **Now Playing** tab, pick a design, tick **Show in OBS**. darkbot adds the browser source `[darkbot] now playing` to its scene, bottom left. Move and resize it in OBS like any source; darkbot never moves it back.
3. Connect Spotify and/or YouTube Music (Song requests). The overlay shows whichever is playing; paused songs are hidden unless you switch that off.

How it works: darkbot writes the overlay page into its settings folder and OBS shows it as a local-file browser source. Every few seconds darkbot asks Spotify (Get Currently Playing Track, a permission darkbot already has) or Pear Desktop for the song and sends it to the page with obs-browser's `emit_event` request over the existing OBS connection. The page counts the progress on by itself in between. Cover images load from Spotify's or YouTube's image servers inside OBS; darkbot's own previews show a copy darkbot downloaded, because darkbot's window loads nothing from the internet.

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
`wails build` isn't used because the Wails v2 binding generator crashed on Go 1.27; the frontend calls `window.go.main.App` directly. CI also runs `govulncheck`, so a known vulnerability in Go or a dependency that darkbot's code reaches fails the build.

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
internal/ytmusic/      YouTube Music through Pear Desktop's API Server (client only)
frontend/dist/overlay/ the Now Playing page (six designs), used by OBS and the previews
internal/songs/        chat commands !sr !song !next !queue !skip
internal/chatbot/      your own chat commands and timers
internal/sbimport/     reads Streamer.bot import codes (commands and timers)
internal/stats/        dashboard counts and the statistics view (session, 7 days, total; kept on disk)
internal/secret/       OS keyring storage with 0600 file fallback
internal/i18n/         UI languages: locales/en.json, de.json (shared by Go and the UI)
```

Settings live in `%AppData%\darkbot`, `~/Library/Application Support/darkbot` or `~/.config/darkbot`.

## License

MIT, see [LICENSE](LICENSE). The bundled Archivo font is licensed under the SIL Open Font License 1.1 (`frontend/dist/fonts/OFL.txt`).

---

powered by [Tarkov Stammtisch](https://tarkov-stammtisch.de)
