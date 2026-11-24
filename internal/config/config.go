// Package config holds darkbot's persistent settings and the Twitch token.
//
// Everything lives in the OS user config directory:
//   - Windows: %AppData%\darkbot
//   - macOS:   ~/Library/Application Support/darkbot
//   - Linux:   ~/.config/darkbot
//
// Files are written atomically (temp file + rename) with 0600 permissions,
// because they contain the OBS password and the Twitch refresh token.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/secret"
)

const (
	appDirName     = "darkbot"
	configFileName = "config.json"
	tokenFileName  = "token.json"
	// spotifyTokenFileName is only used when the OS keyring is unavailable.
	spotifyTokenFileName = "spotify-token.json"
	// ytmusicTokenFileName is only used when the OS keyring is unavailable.
	ytmusicTokenFileName = "ytmusic-token.json"
	statsFileName        = "stats.json"
	activityFileName     = "activity.json"
	// followersFileName: the follower list unfollow detection compares with.
	followersFileName = "followers.json"
	// obsPasswordFileName is only used when the OS keyring is unavailable.
	obsPasswordFileName = "obs-password.json"
	// restoredDirName holds the files a backup brought from another
	// computer (or that were gone from their old place).
	restoredDirName = "restored-files"
	dirPerm         = 0o700
)

// OutputMode decides where sounds are played.
type OutputMode string

const (
	// OutputDevice plays the sound directly on a selected audio output device
	// (like Streamer.bot / StreamElements). OBS captures that device.
	OutputDevice OutputMode = "device"
	// OutputOBS plays the sound inside OBS via a media source on chosen tracks.
	OutputOBS OutputMode = "obs"
)

// OBSTrackCount is the number of audio tracks OBS offers per source.
const OBSTrackCount = 6

// Default values for a fresh config.
const (
	DefaultOBSHost      = "localhost"
	DefaultOBSPort      = 4455 // obs-websocket v5 default port
	DefaultOBSInputName = "[darkbot] sound"
	// LegacyOBSInputName: the default up to v0.2.6; such configs move to
	// DefaultOBSInputName and OBS's source is renamed (internal/obs).
	LegacyOBSInputName    = "darkbot-sound"
	DefaultSoundVolume    = 1.0
	DefaultMasterVolume   = 1.0
	MaxVolume             = 1.0 // 100 %: HTML audio elements cannot amplify
	DefaultCooldownSecond = 0
	DefaultRewardCost     = 100
	// DefaultSongCooldownSeconds: one song request per viewer per minute.
	DefaultSongCooldownSeconds = 60
	// DefaultSongMaxMinutes keeps 20-minute mixes out of the queue.
	DefaultSongMaxMinutes = 10
)

// Sound maps one channel point reward to one audio file.
type Sound struct {
	ID string `json:"id"`
	// RewardID is the Twitch custom reward ID (stable even if the title changes).
	RewardID string `json:"reward_id"`
	// RewardTitle is used for display and as fallback match if RewardID is empty.
	RewardTitle string `json:"reward_title"`
	// Managed means darkbot created this reward on Twitch: darkbot keeps its
	// title, cost, cooldown and on/off state in sync and fulfills or rejects
	// its redemptions. Unmanaged sounds use an existing reward read-only.
	Managed bool `json:"managed"`
	// RewardCost is the channel point price of a managed reward.
	RewardCost int `json:"reward_cost"`
	// RewardColor is the managed reward's color on Twitch, "#RRGGBB".
	// Empty: not chosen yet (Twitch picks a random one on create).
	RewardColor string `json:"reward_color,omitempty"`
	File        string `json:"file"`
	// Volume is a linear factor, 1.0 = 100 %.
	Volume float64 `json:"volume"`
	// CooldownSeconds blocks the sound for everyone after it was triggered.
	CooldownSeconds int `json:"cooldown_seconds"`
	// UserCooldownSeconds blocks the sound for the same viewer.
	UserCooldownSeconds int `json:"user_cooldown_seconds"`
	// MaxPerStream and MaxPerUserPerStream limit a managed reward's
	// redemptions per live stream on Twitch; 0 = no limit.
	MaxPerStream        int  `json:"max_per_stream,omitempty"`
	MaxPerUserPerStream int  `json:"max_per_user_per_stream,omitempty"`
	Enabled             bool `json:"enabled"`
}

// OBS holds the obs-websocket connection and the media-source output settings.
type OBS struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Password is only read from configs of v0.2.x and older: darkbot
	// moves it to the OS keyring on start (see obs_app.go) and never
	// writes it here again.
	Password string `json:"password,omitempty"`
	// AutoConnect connects to OBS on app start.
	AutoConnect bool `json:"auto_connect"`
	// InputName is the media source darkbot creates/uses in OBS-output mode.
	InputName string `json:"input_name"`
	// Tracks are OBS audio tracks 1..6 the media source is routed to.
	Tracks [OBSTrackCount]bool `json:"tracks"`
}

// SongRequests are the chat song request settings (Spotify).
type SongRequests struct {
	// Enabled turns the chat commands on.
	Enabled bool `json:"enabled"`
	// SpotifyClientID is the streamer's own Spotify app (see README).
	SpotifyClientID string `json:"spotify_client_id"`
	// UserCooldownSeconds: how long a viewer waits between two requests
	// (broadcaster and moderators are exempt).
	UserCooldownSeconds int `json:"user_cooldown_seconds"`
	// MaxMinutes rejects longer songs; 0 = no limit.
	MaxMinutes int `json:"max_minutes"`
	// BlockExplicit rejects songs Spotify marks as explicit.
	BlockExplicit bool `json:"block_explicit"`
	// ChatReplies answers commands in chat.
	ChatReplies bool `json:"chat_replies"`
	// HideTitles leaves song titles out of chat answers: a title on
	// Spotify can be any text, and darkbot answers as the streamer's own
	// account, which AutoMod doesn't filter. Off = titles are named (the
	// default, like common chat bots).
	HideTitles bool `json:"hide_titles"`
	// RequesterCanSkip lets the viewer who requested the current song skip
	// it (moderators and the broadcaster can always skip).
	RequesterCanSkip bool `json:"requester_can_skip"`
	// YTMusicEnabled: requests may also go to YouTube Music through Pear
	// Desktop's API Server.
	YTMusicEnabled bool `json:"ytmusic_enabled"`
	// YTMusicPort is the port of Pear Desktop's API Server.
	YTMusicPort int `json:"ytmusic_port"`
	// SearchService answers "!sr <name>" (links go to the service they
	// belong to): ServiceSpotify or ServiceYTMusic.
	SearchService string `json:"search_service"`
}

// Song request services.
const (
	ServiceSpotify = "spotify"
	ServiceYTMusic = "ytmusic"
	// DefaultYTMusicPort is Pear Desktop's default API Server port.
	DefaultYTMusicPort = 26538
	maxPort            = 65535
)

// EventAction is what darkbot does on a channel event (follow, sub, ...).
// The event always shows in the activity feed; the action is optional.
type EventAction struct {
	Enabled bool `json:"enabled"`
	// Sound file to play ("" = none) at Volume.
	Sound  string  `json:"sound"`
	Volume float64 `json:"volume"`
	// Chat message template ("" = none); placeholders {user}, {amount},
	// {tier}, {months}. Viewer text is never inserted.
	Chat string `json:"chat"`
	// MinAmount: only react from this amount on (bits, raid viewers, gifted
	// subs); 0 = always.
	MinAmount int `json:"min_amount"`
	// Visual alert in OBS: a picture/GIF or video file ("" = none) and an
	// on-screen text template (same placeholders as Chat), shown for
	// Seconds (0 = video length, or the default for pictures).
	Visual      string `json:"visual,omitempty"`
	OverlayText string `json:"overlay_text,omitempty"`
	Seconds     int    `json:"seconds,omitempty"`
	// Shoutout (raids only): give the raider Twitch's shoutout.
	Shoutout bool `json:"shoutout,omitempty"`
	// LeadSeconds (ad warning only): warn this long before the ad;
	// 0 = DefaultAdLead.
	LeadSeconds int `json:"lead_seconds,omitempty"`
}

// Ad warning lead time: Get Ad Schedule is read every few seconds while
// live (internal app), so the warning is at most that late; a minute ahead
// is the usual "ad in 60 seconds" announcement.
const (
	DefaultAdLead = 60
	MinAdLead     = 15
	MaxAdLead     = 600
)

// Permission says who may use a chat command. Each level includes the
// ones above it in this list (a moderator may use a VIP command).
type Permission string

const (
	PermEveryone    Permission = "everyone"
	PermSubscriber  Permission = "subscriber"
	PermVIP         Permission = "vip"
	PermModerator   Permission = "moderator"
	PermBroadcaster Permission = "broadcaster"
)

// Permissions lists every level, lowest first.
var Permissions = []Permission{PermEveryone, PermSubscriber, PermVIP, PermModerator, PermBroadcaster}

// NowPlaying: the "Now Playing" overlay, a browser source in the darkbot
// scene showing the song Spotify or YouTube Music plays.
type NowPlaying struct {
	// Enabled shows the overlay in OBS.
	Enabled bool `json:"enabled"`
	// Theme is one of NowPlayingThemes.
	Theme          string `json:"theme"`
	ShowCover      bool   `json:"show_cover"`
	ShowProgress   bool   `json:"show_progress"`
	ShowRequester  bool   `json:"show_requester"`
	HideWhenPaused bool   `json:"hide_when_paused"`
}

// NowPlayingThemes are the overlay designs (frontend/dist/overlay).
var NowPlayingThemes = []string{"console", "glass", "band", "vinyl", "minimal", "pill"}

// DefaultNowPlaying: off, the darkbot look, everything shown.
func DefaultNowPlaying() NowPlaying {
	return NowPlaying{Theme: NowPlayingThemes[0], ShowCover: true, ShowProgress: true, ShowRequester: true, HideWhenPaused: true}
}

// Command triggers.
const (
	// TriggerCommand: the message starts with "!name" (or an alias).
	TriggerCommand = "command"
	// TriggerWord: the message's first word is the name (or an alias),
	// without "!", e.g. "hi" in "Hi everyone!".
	TriggerWord = "word"
	// TriggerFirst: a viewer's first chat message of the live stream,
	// whatever it says (a greeting); it has no name.
	TriggerFirst = "first"
)

// ChatCommand answers "!name" (or an alias) in chat, or a message that
// starts with a word (Trigger).
type ChatCommand struct {
	ID string `json:"id"`
	// Name without "!", lowercase.
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	// Trigger is TriggerCommand, TriggerWord or TriggerFirst.
	Trigger string `json:"trigger"`
	// Response template; placeholders {user} and {target}.
	Response string `json:"response"`
	// ResponseFile is a text file; when set, each answer is a random line
	// of it (same placeholders) instead of Response. darkbot reads it again
	// whenever it changes.
	ResponseFile string `json:"response_file,omitempty"`
	// OncePerStream answers each viewer at most once per stream (e.g. a
	// greeting); the broadcaster is exempt.
	OncePerStream bool       `json:"once_per_stream,omitempty"`
	Permission    Permission `json:"permission"`
	// CooldownSeconds blocks the command for everyone after a use;
	// UserCooldownSeconds for the same viewer. The broadcaster is exempt.
	CooldownSeconds     int `json:"cooldown_seconds"`
	UserCooldownSeconds int `json:"user_cooldown_seconds"`
	// Sound file played in the sound queue ("" = none) at Volume.
	Sound   string  `json:"sound,omitempty"`
	Volume  float64 `json:"volume,omitempty"`
	Enabled bool    `json:"enabled"`
}

// ChatTimer posts one of its Messages every IntervalMinutes while the
// stream is live.
type ChatTimer struct {
	ID string `json:"id"`
	// Messages: one is posted each time, picked by Order.
	Messages []string `json:"messages"`
	// Message is the single message of settings from v0.7.3 and older;
	// normalize moves it into Messages.
	Message         string `json:"message,omitempty"`
	Order           string `json:"order"`
	IntervalMinutes int    `json:"interval_minutes"`
	// MinLines: chat messages needed since its last post, so it never
	// talks into an empty chat; 0 = no minimum.
	MinLines int  `json:"min_lines"`
	Enabled  bool `json:"enabled"`
}

// Lines returns the timer's messages, including the single message of
// settings from v0.7.3 and older.
func (t ChatTimer) Lines() []string {
	if len(t.Messages) == 0 && t.Message != "" {
		return []string{t.Message}
	}
	return t.Messages
}

// Timer orders: TimerRandom shuffles the messages like a deck of cards
// (each once per round, never the same twice in a row); TimerInOrder posts
// them top to bottom.
const (
	TimerRandom  = "random"
	TimerInOrder = "in_order"
)

// Chat holds commands and timers. Both are off until the streamer turns
// them on.
type Chat struct {
	CommandsEnabled bool          `json:"commands_enabled"`
	TimersEnabled   bool          `json:"timers_enabled"`
	Commands        []ChatCommand `json:"commands"`
	Timers          []ChatTimer   `json:"timers"`
}

// Limits for commands and timers.
const (
	MaxCommands        = 200
	MaxTimers          = 50
	MaxAliases         = 10
	MaxCommandCooldown = 3600 // one hour, in seconds
	// MinTimerMinutes keeps timers from flooding chat; MaxTimerMinutes is
	// half a day.
	MinTimerMinutes = 5
	MaxTimerMinutes = 720
	MaxTimerLines   = 1000
	// MaxTimerMessages: messages one timer takes turns with.
	MaxTimerMessages = 20
)

// MaxChatTemplate matches Twitch's chat message limit (500 characters).
const MaxChatTemplate = 500

// MaxOverlayText: an on-screen alert line, kept short enough to read.
const MaxOverlayText = 200

// MaxAlertSeconds: longest time an alert may stay on screen.
const MaxAlertSeconds = 60

// Config is the whole persisted app configuration.
type Config struct {
	TwitchClientID string `json:"twitch_client_id"`
	// Language is the UI language chosen by the user ("en", "de"); empty
	// follows the system language.
	Language   string     `json:"language"`
	OutputMode OutputMode `json:"output_mode"`
	// AudioDeviceID is the web audio output "sinkId" chosen in the UI;
	// empty = system default output.
	AudioDeviceID   string  `json:"audio_device_id"`
	AudioDeviceName string  `json:"audio_device_name"`
	MasterVolume    float64 `json:"master_volume"`
	// AutoUpdate installs a newer release automatically when darkbot starts
	// (never mid-stream: while running, updates are only announced).
	AutoUpdate bool         `json:"auto_update"`
	OBS        OBS          `json:"obs"`
	Sounds     []Sound      `json:"sounds"`
	Songs      SongRequests `json:"songs"`
	// Events maps an event kind (twitch.Ev*) to its action.
	Events map[string]EventAction `json:"events"`
	// UnfollowDetection compares the follower list every few minutes,
	// because Twitch has no unfollow event (opt-in).
	UnfollowDetection bool `json:"unfollow_detection"`
	// QuitOnClose: closing the window quits darkbot. Default false: where
	// darkbot has a tray icon (Windows), it keeps running there.
	QuitOnClose bool `json:"quit_on_close"`
	// Chat: commands and timers.
	Chat Chat `json:"chat"`
	// NowPlaying is the song overlay for OBS.
	NowPlaying NowPlaying `json:"now_playing"`
	// Clips is the built-in clip command.
	Clips Clips `json:"clips"`
	// Marker (!marker: a stream marker for editing the VOD) and Snooze
	// (!snooze: push the next ad back) are built-in commands.
	Marker BuiltinCommand `json:"marker"`
	Snooze BuiltinCommand `json:"snooze"`
	// Uptime (!uptime: how long the stream runs) and Followage
	// (!followage [@name]: how long someone follows) answer questions.
	Uptime    BuiltinCommand `json:"uptime"`
	Followage BuiltinCommand `json:"followage"`
	// Overlays: the goal and poll/prediction overlays in OBS.
	Overlays Overlays `json:"overlays"`
	// IgnoredChatters: Twitch logins the statistics never count (the
	// streamer's own bots), on top of stats.KnownBots.
	IgnoredChatters []string `json:"ignored_chatters"`
}

// Overlays switches darkbot's OBS overlays besides Now Playing.
type Overlays struct {
	// Goals shows the channel's creator goals as progress bars.
	Goals bool `json:"goals"`
	// Votes shows a running poll or prediction.
	Votes bool `json:"votes"`
}

// BuiltinCommand is a built-in chat command's settings.
type BuiltinCommand struct {
	Enabled bool `json:"enabled"`
	// Name without "!", lowercase.
	Name       string     `json:"name"`
	Permission Permission `json:"permission"`
	// CooldownSeconds for everyone; the broadcaster and moderators skip it.
	CooldownSeconds int `json:"cooldown_seconds"`
}

// Built-in command defaults: both act on the stream itself, so only
// moderators and the broadcaster may use them unless the streamer opens
// them up.
const (
	DefaultMarkerName = "marker"
	DefaultSnoozeName = "snooze"
	// DefaultMarkerCooldown keeps a chat from setting a marker per message.
	DefaultMarkerCooldown = 10
	DefaultUptimeName     = "uptime"
	DefaultFollowageName  = "followage"
	// DefaultInfoCooldown: !uptime and !followage are for everyone, so a
	// chat asking together gets one answer per this many seconds.
	DefaultInfoCooldown = 30
)

// DefaultMarker and DefaultSnooze are the settings of a new config.
func DefaultMarker() BuiltinCommand {
	return BuiltinCommand{Name: DefaultMarkerName, Permission: PermModerator, CooldownSeconds: DefaultMarkerCooldown}
}

func DefaultSnooze() BuiltinCommand {
	return BuiltinCommand{Name: DefaultSnoozeName, Permission: PermModerator}
}

// DefaultUptime and DefaultFollowage only read, so everyone may use them.
func DefaultUptime() BuiltinCommand {
	return BuiltinCommand{Name: DefaultUptimeName, Permission: PermEveryone, CooldownSeconds: DefaultInfoCooldown}
}

func DefaultFollowage() BuiltinCommand {
	return BuiltinCommand{Name: DefaultFollowageName, Permission: PermEveryone, CooldownSeconds: DefaultInfoCooldown}
}

// normalizeBuiltin gives settings from before the command its defaults and
// keeps the rest in range.
func normalizeBuiltin(c *BuiltinCommand, def BuiltinCommand) {
	if c.Name == "" {
		*c = def
	}
	if !slices.Contains(Permissions, c.Permission) {
		c.Permission = PermBroadcaster // unknown level: fail closed
	}
	c.CooldownSeconds = min(max(c.CooldownSeconds, 0), MaxCommandCooldown)
}

// Clips is the built-in "!clips" command: a viewer makes a clip of the
// last ClipSeconds of the stream and darkbot posts its link.
type Clips struct {
	Enabled bool `json:"enabled"`
	// Name without "!", lowercase (DefaultClipName).
	Name       string     `json:"name"`
	Permission Permission `json:"permission"`
	// CooldownSeconds: after a clip, nobody (except the broadcaster and
	// moderators) can make the next one this long.
	CooldownSeconds int `json:"cooldown_seconds"`
	// Title template, placeholder {user}; "" keeps Twitch's default title
	// (the stream title).
	Title string `json:"title"`
}

// Clip command defaults and limits.
const (
	DefaultClipName = "clips"
	// DefaultClipCooldown: one clip a minute, so a chat can't flood the
	// channel with clips of the same moment.
	DefaultClipCooldown = 60
	// MaxClipTitle keeps a title as short as Twitch's clip editor does.
	MaxClipTitle = 100
)

// DefaultClips are the clip command's settings for a new config.
func DefaultClips() Clips {
	return Clips{Name: DefaultClipName, Permission: PermEveryone, CooldownSeconds: DefaultClipCooldown}
}

// Default returns a config with sensible defaults.
func Default() Config {
	return Config{
		OutputMode:   OutputDevice,
		MasterVolume: DefaultMasterVolume,
		NowPlaying:   DefaultNowPlaying(),
		Clips:        DefaultClips(),
		Marker:       DefaultMarker(),
		Snooze:       DefaultSnooze(),
		Uptime:       DefaultUptime(),
		Followage:    DefaultFollowage(),
		AutoUpdate:   true,
		OBS: OBS{
			Host:      DefaultOBSHost,
			Port:      DefaultOBSPort,
			InputName: DefaultOBSInputName,
			Tracks:    [OBSTrackCount]bool{true}, // track 1 only
		},
		Songs: SongRequests{
			UserCooldownSeconds: DefaultSongCooldownSeconds,
			MaxMinutes:          DefaultSongMaxMinutes,
			ChatReplies:         true,
			RequesterCanSkip:    true,
			YTMusicPort:         DefaultYTMusicPort,
			SearchService:       ServiceSpotify,
		},
	}
}

// normalize fixes zero/invalid values (e.g. from older or hand-edited files).
func (c *Config) normalize() {
	if c.OutputMode != OutputDevice && c.OutputMode != OutputOBS {
		c.OutputMode = OutputDevice
	}
	for k, a := range c.Events {
		a.Sound = strings.TrimSpace(a.Sound)
		a.Chat = strings.TrimSpace(a.Chat)
		if a.Volume <= 0 || a.Volume > MaxVolume {
			a.Volume = DefaultSoundVolume
		}
		a.MinAmount = max(a.MinAmount, 0)
		c.Events[k] = a
	}
	for i := range c.Chat.Commands {
		cmd := &c.Chat.Commands[i]
		if cmd.ID == "" {
			cmd.ID = NewID()
		}
		if !slices.Contains(Permissions, cmd.Permission) {
			cmd.Permission = PermBroadcaster // unknown level: fail closed
		}
		if cmd.Trigger != TriggerWord && cmd.Trigger != TriggerFirst {
			cmd.Trigger = TriggerCommand
		}
		cmd.CooldownSeconds = min(max(cmd.CooldownSeconds, 0), MaxCommandCooldown)
		cmd.UserCooldownSeconds = min(max(cmd.UserCooldownSeconds, 0), MaxCommandCooldown)
		if cmd.Volume <= 0 || cmd.Volume > MaxVolume {
			cmd.Volume = DefaultSoundVolume
		}
	}
	for i := range c.Chat.Timers {
		tm := &c.Chat.Timers[i]
		if tm.ID == "" {
			tm.ID = NewID()
		}
		tm.IntervalMinutes = min(max(tm.IntervalMinutes, MinTimerMinutes), MaxTimerMinutes)
		tm.MinLines = min(max(tm.MinLines, 0), MaxTimerLines)
		tm.Messages, tm.Message = tm.Lines(), ""
		if tm.Order != TimerInOrder {
			tm.Order = TimerRandom
		}
	}
	c.Songs.SpotifyClientID = strings.TrimSpace(c.Songs.SpotifyClientID)
	c.Songs.UserCooldownSeconds = max(c.Songs.UserCooldownSeconds, 0)
	c.Songs.MaxMinutes = max(c.Songs.MaxMinutes, 0)
	if c.Songs.YTMusicPort <= 0 || c.Songs.YTMusicPort > maxPort {
		c.Songs.YTMusicPort = DefaultYTMusicPort
	}
	// No theme: settings from before the overlay existed get its defaults
	// (its switches are false there only because they were missing).
	if c.NowPlaying.Theme == "" {
		c.NowPlaying = DefaultNowPlaying()
	} else if !slices.Contains(NowPlayingThemes, c.NowPlaying.Theme) {
		c.NowPlaying.Theme = NowPlayingThemes[0]
	}
	// No name: settings from before the clip command get its defaults.
	if c.Clips.Name == "" {
		c.Clips = DefaultClips()
	}
	if !slices.Contains(Permissions, c.Clips.Permission) {
		c.Clips.Permission = PermBroadcaster // unknown level: fail closed
	}
	c.Clips.CooldownSeconds = min(max(c.Clips.CooldownSeconds, 0), MaxCommandCooldown)
	normalizeBuiltin(&c.Marker, DefaultMarker())
	normalizeBuiltin(&c.Snooze, DefaultSnooze())
	normalizeBuiltin(&c.Uptime, DefaultUptime())
	normalizeBuiltin(&c.Followage, DefaultFollowage())
	if c.Songs.SearchService != ServiceSpotify && c.Songs.SearchService != ServiceYTMusic {
		c.Songs.SearchService = ServiceSpotify
	}
	if c.Language != "" && !i18n.IsSupported(c.Language) {
		c.Language = ""
	}
	if c.MasterVolume <= 0 || c.MasterVolume > MaxVolume {
		c.MasterVolume = DefaultMasterVolume
	}
	if c.OBS.Host == "" {
		c.OBS.Host = DefaultOBSHost
	}
	if c.OBS.Port <= 0 || c.OBS.Port > 65535 {
		c.OBS.Port = DefaultOBSPort
	}
	if c.OBS.InputName == "" || c.OBS.InputName == LegacyOBSInputName {
		c.OBS.InputName = DefaultOBSInputName
	}
	for i := range c.Sounds {
		s := &c.Sounds[i]
		if s.ID == "" {
			s.ID = NewID()
		}
		if s.Volume <= 0 || s.Volume > MaxVolume {
			s.Volume = DefaultSoundVolume
		}
		if s.CooldownSeconds < 0 {
			s.CooldownSeconds = 0
		}
		if s.UserCooldownSeconds < 0 {
			s.UserCooldownSeconds = 0
		}
		// Limits exist only on rewards darkbot manages: Twitch lets only
		// the creating app change a reward.
		if !s.Managed {
			s.MaxPerStream, s.MaxPerUserPerStream = 0, 0
		}
		s.MaxPerStream = max(s.MaxPerStream, 0)
		s.MaxPerUserPerStream = max(s.MaxPerUserPerStream, 0)
		if s.Managed && s.RewardCost <= 0 {
			s.RewardCost = DefaultRewardCost
		}
	}
}

// Clone returns a deep copy so callers never share a slice.
func (c Config) Clone() Config {
	out := c
	out.Sounds = append([]Sound(nil), c.Sounds...)
	out.Chat.Commands = make([]ChatCommand, len(c.Chat.Commands))
	for i, cmd := range c.Chat.Commands {
		cmd.Aliases = append([]string(nil), cmd.Aliases...)
		out.Chat.Commands[i] = cmd
	}
	out.Chat.Timers = make([]ChatTimer, len(c.Chat.Timers))
	for i, tm := range c.Chat.Timers {
		tm.Messages = append([]string(nil), tm.Messages...)
		out.Chat.Timers[i] = tm
	}
	if c.Events != nil {
		out.Events = make(map[string]EventAction, len(c.Events))
		maps.Copy(out.Events, c.Events)
	}
	return out
}

// RewriteFiles calls fn for every setting that names a file on this
// computer (reward and event sounds, alert pictures and videos, command
// sounds and answer files) and stores the path it returns. When fn says
// not to keep a file, the setting is emptied and the reward, event or
// command it belongs to is turned off. Empty settings are skipped.
// Backups use it to collect and to move those files; a new file setting
// must be added here.
func (c *Config) RewriteFiles(fn func(path string) (newPath string, keep bool)) {
	rw := func(p *string, enabled *bool) {
		if *p == "" {
			return
		}
		np, keep := fn(*p)
		if !keep {
			np, *enabled = "", false
		}
		*p = np
	}
	for i := range c.Sounds {
		rw(&c.Sounds[i].File, &c.Sounds[i].Enabled)
	}
	for k, a := range c.Events {
		rw(&a.Sound, &a.Enabled)
		rw(&a.Visual, &a.Enabled)
		c.Events[k] = a
	}
	for i := range c.Chat.Commands {
		cmd := &c.Chat.Commands[i]
		rw(&cmd.Sound, &cmd.Enabled)
		rw(&cmd.ResponseFile, &cmd.Enabled)
	}
}

// Files lists the files the settings name, each once, in a stable order.
func (c Config) Files() []string {
	seen := map[string]bool{}
	var out []string
	c = c.Clone() // RewriteFiles writes back into the Events map
	c.RewriteFiles(func(p string) (string, bool) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		return p, true
	})
	return out
}

// NewID returns a random 16-hex-char identifier.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; panic is the documented
		// behaviour of Go 1.24+ anyway.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// Store is a thread-safe, file-backed config holder.
type Store struct {
	mu   sync.RWMutex
	dir  string
	cfg  Config
	subs []func(Config)
	// Recovered is set when Open had to set an unreadable config aside
	// (for the log); "" normally.
	Recovered string
}

// corruptStamp names a set-aside unreadable config file.
const corruptStamp = "20060102-150405"

// Dir returns the darkbot config directory, creating it if needed.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	dir := filepath.Join(base, appDirName)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("create config dir %s: %w", dir, err)
	}
	return dir, nil
}

// Open loads the config from dir (missing file = defaults). An unreadable
// file (e.g. cut off by a crash) is set aside as config.json.corrupt-<time>
// and darkbot starts with defaults instead of not starting at all; Recovered
// then says what happened.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, cfg: Default()}
	path := filepath.Join(dir, configFileName)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// first start: keep defaults
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	default:
		cfg := Default()
		if perr := json.Unmarshal(data, &cfg); perr != nil {
			backup := path + ".corrupt-" + time.Now().Format(corruptStamp)
			if err := os.Rename(path, backup); err != nil {
				return nil, fmt.Errorf("parse config %s: %w (and could not set it aside: %v)", path, perr, err)
			}
			s.Recovered = fmt.Sprintf("the settings file could not be read (%v); it was kept as %s and darkbot started with default settings", perr, backup)
			break
		}
		s.cfg = cfg
	}
	s.cfg.normalize()
	return s, nil
}

// Get returns a copy of the current config.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Update applies fn to the config, normalizes, saves to disk and notifies subscribers.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	next := s.cfg.Clone()
	fn(&next)
	next.normalize()
	if err := writeJSON(filepath.Join(s.dir, configFileName), next); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cfg = next
	subs := append(([]func(Config))(nil), s.subs...)
	s.mu.Unlock()

	for _, fn := range subs {
		fn(next.Clone())
	}
	return nil
}

// Subscribe registers a callback that runs after each successful Update.
func (s *Store) Subscribe(fn func(Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// SpotifyTokenPath is the Spotify token file (used only without a keyring).
func (s *Store) SpotifyTokenPath() string { return filepath.Join(s.dir, spotifyTokenFileName) }

// YTMusicTokenPath is the Pear Desktop token file (used only without a keyring).
func (s *Store) YTMusicTokenPath() string { return filepath.Join(s.dir, ytmusicTokenFileName) }

// StatsPath is the dashboard totals file.
func (s *Store) StatsPath() string { return filepath.Join(s.dir, statsFileName) }

// ActivityPath is the kept activity feed.
func (s *Store) ActivityPath() string { return filepath.Join(s.dir, activityFileName) }

// OBSPasswordPath holds the OBS password only when the OS keyring is
// unavailable.
func (s *Store) OBSPasswordPath() string { return filepath.Join(s.dir, obsPasswordFileName) }

// FollowersPath is the follower list of the unfollow detection.
func (s *Store) FollowersPath() string { return filepath.Join(s.dir, followersFileName) }

// RestoredFilesDir is where restored backup files go.
func (s *Store) RestoredFilesDir() string { return filepath.Join(s.dir, restoredDirName) }

// PreImportPath names the copy of the settings kept before a backup
// replaces them.
func (s *Store) PreImportPath(stamp string) string {
	return filepath.Join(s.dir, configFileName+".before-import-"+stamp)
}

// TokenPath returns where the Twitch token file lives.
func (s *Store) TokenPath() string { return filepath.Join(s.dir, tokenFileName) }

// LoadJSON reads a JSON file into v. Returns os.ErrNotExist if missing.
func LoadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// SaveJSON writes v atomically with 0600 permissions.
func SaveJSON(path string, v any) error { return writeJSON(path, v) }

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	if err := secret.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
