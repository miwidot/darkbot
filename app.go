package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/autostart"
	"github.com/miwidot/darkbot/internal/bot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/obs"
	"github.com/miwidot/darkbot/internal/selfupdate"
	"github.com/miwidot/darkbot/internal/songs"
	"github.com/miwidot/darkbot/internal/spotify"
	"github.com/miwidot/darkbot/internal/stats"
	"github.com/miwidot/darkbot/internal/tray"
	"github.com/miwidot/darkbot/internal/twitch"
	"github.com/miwidot/darkbot/internal/version"
)

// defaultClientID is darkbot's own public Twitch app (client type Public,
// device code flow, no secret). Client IDs are public by design
// (https://dev.twitch.tv/docs/authentication/register-app/), so it lives in
// the source. Forks can replace it at build time:
//
//	make windows CLIENT_ID=<your twitch client id>
//
// Users can still enter their own Client ID in the app.
var defaultClientID = "9dtliag7dlvijuuzim3gsttux64pqv"

// Errors from other packages that the UI shows translated.
func init() {
	i18n.RegisterSentinel(twitch.ErrNotLoggedIn, "err.notLoggedIn")
	i18n.RegisterSentinel(twitch.ErrTokenRejected, "err.tokenRejected")
	i18n.RegisterSentinel(twitch.ErrNotAffiliate, "err.notAffiliate")
	i18n.RegisterSentinel(twitch.ErrNotFound, "err.notFound")
	i18n.RegisterSentinel(twitch.ErrNotOwner, "err.notOwner")
}

const (
	// playbackGrace is added to a sound's length before we give up waiting
	// for the webview to report the end of playback.
	playbackGrace  = 5 * time.Second
	rewardsTimeout = 15 * time.Second
	soundURLPrefix = "/sound/"
	// firstUpdateCheck waits a moment after start so the window is up first.
	firstUpdateCheck = 5 * time.Second
	// updateCheckEvery: while running, new releases are only announced.
	// GitHub allows 60 unauthenticated API requests per hour and IP; two
	// per hour leave plenty for other programs on the same network.
	updateCheckEvery = 30 * time.Minute
	// spotifyLoginTimeout bounds the code exchange + profile request.
	spotifyLoginTimeout = 30 * time.Second
	// chatSendTimeout bounds one chat reply.
	chatSendTimeout = 10 * time.Second
)

// App is bound to the frontend: every exported method is callable from JS.
type App struct {
	tray trayState
	ch   channelState
	// OBS keep-alive, guarded by mu (see obs_app.go).
	obsWant, obsAuthFailed, obsWaitLogged bool
	obsScene                              *obs.SceneStatus
	ctx                                   context.Context
	log                                   *applog.Log
	cfg                                   *config.Store
	tok                                   *twitch.TokenManager
	hx                                    *twitch.Helix
	obs                                   *obs.Client
	bot                                   *bot.Bot
	web                                   *webOutput
	obsO                                  *obs.Output
	sp                                    *spotify.Client
	sr                                    *songs.Handler
	st                                    *stats.Stats

	mu          sync.Mutex
	esCancel    context.CancelFunc
	esConnected bool
	loginCancel context.CancelFunc
	loginCode   *twitch.DeviceCode
	update      *selfupdate.Info
	updating    bool
	spLogin     *spotify.PendingLogin   // Spotify login waiting for the pasted address
	fw          *twitch.FollowerWatcher // unfollow detection, nil when off
	fwCancel    context.CancelFunc
	// sysLangs are the system languages reported by the web view
	// (navigator.languages); used when the user didn't pick a language.
	sysLangs []string
}

// NewApp wires all components. Errors here are fatal config/keyring problems.
func NewApp(log *applog.Log) (*App, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Open(dir)
	if err != nil {
		return nil, err
	}
	if cfg.Get().TwitchClientID == "" && defaultClientID != "" {
		if err := cfg.Update(func(c *config.Config) { c.TwitchClientID = defaultClientID }); err != nil {
			return nil, err
		}
	}
	store := &twitch.KeyringStore{FallbackPath: cfg.TokenPath(), Log: log}
	tok, err := twitch.NewTokenManager(cfg.Get().TwitchClientID, store, log)
	if err != nil {
		return nil, err
	}

	a := &App{log: log, cfg: cfg, tok: tok, hx: twitch.NewHelix(tok), st: stats.Open(cfg.StatsPath(), log)}
	a.ch.kick = make(chan struct{}, 1)
	a.st.OnChange(func() {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "stats")
		}
	})
	a.obs = obs.New(a.onOBSDrop)
	a.obs.OnScenesChanged(a.refreshOBSScene)
	a.web = &webOutput{app: a, pending: map[string]chan string{}}
	a.obsO = &obs.Output{OBS: a.obs, Cfg: cfg}
	a.bot = bot.New(cfg, log, a.currentOutput, func(act bot.Activity) {
		a.countActivity(act)
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "activity", act)
		}
	})
	a.bot.SetResolver(a.resolveRedemption)
	a.bot.SetOverlay(a.overlay)
	a.bot.SetLocalizer(func(err error) string { return i18n.Localize(err, a.lang()) })

	spStore := &spotify.KeyringStore{FallbackPath: cfg.SpotifyTokenPath(), Log: log}
	if a.sp, err = spotify.NewClient(cfg.Get().Songs.SpotifyClientID, spStore, log); err != nil {
		return nil, err
	}
	a.sp.OnChange(a.emitState)
	a.sr = songs.New(songs.Deps{
		Settings:  func() config.SongRequests { return cfg.Get().Songs },
		Player:    a.sp,
		Connected: func() bool { return a.sp.Current() != nil },
		Lang:      a.lang,
		Reply:     a.sendChat,
		OnCommand: a.st.Command,
		OnActivity: func(act songs.Activity) {
			switch act.Kind {
			case songs.ActAdded:
				a.st.SongAdded(act.User)
			case songs.ActRejected:
				a.st.SongRejected()
			case songs.ActSkipped:
				a.st.Skip()
			}
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "activity", act)
			}
		},
		Log: log,
	})
	return a, nil
}

func (a *App) currentOutput() bot.Output {
	if a.cfg.Get().OutputMode == config.OutputOBS {
		return a.obsO
	}
	return a.web
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	selfupdate.CleanupPrevious()
	a.log.Infof("darkbot %s", version.Version)
	a.log.OnChange(func() { runtime.EventsEmit(ctx, "log") })
	a.tok.OnChange(func(t *twitch.Token) {
		if t == nil {
			// Logged out (by the user or because Twitch rejected the refresh
			// token): stop listening so the status pills tell the truth.
			a.stopEventSub()
		}
		a.refreshChannelTotalsSoon() // logged in, out, or new permissions
		a.emitState()
	})

	go a.tok.Run(ctx)
	go a.bot.Run(ctx)

	if a.tok.Current() != nil {
		a.startEventSub()
	} else {
		a.log.Infof("Not logged in to Twitch yet")
	}
	a.startOBS(ctx)
	a.startChannelTotals(ctx)
	go a.updateLoop(ctx)
	a.startTray()
	// darkbot may have been moved since autostart was switched on.
	if err := autostart.Refresh(); err != nil {
		a.log.Warnf("Autostart: could not update the entry: %v", err)
	}
}

// ---------- updates ----------

// updateLoop checks GitHub releases. Only the check right after start may
// install automatically (the stream hasn't started yet); later checks just
// show the update in the UI so a running stream is never interrupted.
func (a *App) updateLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(firstUpdateCheck):
	}
	if info := a.checkUpdate(); info != nil && info.Available && a.cfg.Get().AutoUpdate {
		a.log.Infof("Installing update v%s automatically", info.Latest)
		if err := a.installUpdate(); err != nil {
			a.log.Errorf("Automatic update failed: %v", err)
		}
	}
	ticker := time.NewTicker(updateCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkUpdate()
		}
	}
}

func (a *App) checkUpdate() *selfupdate.Info {
	info, err := selfupdate.Check(version.RepoOwner, version.RepoName, version.Version)
	if err != nil {
		a.log.Warnf("Update check: %v", err)
		return nil
	}
	a.mu.Lock()
	a.update = info
	a.mu.Unlock()
	if info.Available {
		a.log.Infof("Update available: v%s (you have %s)", info.Latest, version.Version)
	}
	a.emitState()
	return info
}

// CheckUpdate runs an update check now (Settings → Updates).
func (a *App) CheckUpdate() (*selfupdate.Info, error) {
	info, err := selfupdate.Check(version.RepoOwner, version.RepoName, version.Version)
	if err != nil {
		return nil, a.uiErr(err)
	}
	a.mu.Lock()
	a.update = info
	a.mu.Unlock()
	a.emitState()
	return info, nil
}

// InstallUpdate downloads and verifies the latest release, replaces the
// program and restarts it.
func (a *App) InstallUpdate() error { return a.uiErr(a.installUpdate()) }

func (a *App) installUpdate() error {
	a.mu.Lock()
	if a.updating {
		a.mu.Unlock()
		return i18n.E("err.updateRunning")
	}
	a.updating = true
	a.mu.Unlock()
	a.emitState()
	defer func() {
		a.mu.Lock()
		a.updating = false
		a.mu.Unlock()
		a.emitState()
	}()

	tag, err := selfupdate.Update(version.RepoOwner, version.RepoName, version.Version)
	if err != nil {
		return err
	}
	a.log.Infof("Update %s installed, restarting", tag)
	// Restart exits without the normal shutdown: save what shutdown saves.
	a.st.Flush()
	a.obs.Disconnect()
	tray.Stop()
	var args []string
	if startedInTray {
		args = append(args, autostart.TrayArg)
	}
	return selfupdate.Restart(args...)
}

// SetAutoUpdate toggles automatic installation on start.
func (a *App) SetAutoUpdate(on bool) error {
	return a.uiErr(a.cfg.Update(func(c *config.Config) { c.AutoUpdate = on }))
}

func (a *App) shutdown(context.Context) {
	a.st.Flush()
	a.obs.Disconnect()
	tray.Stop()
}

// ---------- state ----------

// State is everything the UI renders.
type State struct {
	Config         config.Config `json:"config"`
	TwitchLogin    string        `json:"twitchLogin"`
	EventSubOnline bool          `json:"eventSubOnline"`
	OBSConnected   bool          `json:"obsConnected"`
	// OBSWaiting: darkbot should be connected and retries until OBS is
	// there. OBSAuthFailed: OBS rejected the password, no retries.
	OBSWaiting    bool `json:"obsWaiting"`
	OBSAuthFailed bool `json:"obsAuthFailed"`
	// OBSScene: where darkbot's scene is (nil = unknown, e.g. not connected).
	OBSScene         *obs.SceneStatus `json:"obsScene"`
	LoginUserCode    string           `json:"loginUserCode"`
	LoginURI         string           `json:"loginUri"`
	SupportedFormats []string         `json:"supportedFormats"`
	Version          string           `json:"version"`
	Update           *selfupdate.Info `json:"update"`
	Updating         bool             `json:"updating"`
	// NeedsReconnect: the stored token lacks a scope (e.g. from an older
	// darkbot), so creating rewards needs a fresh Twitch login.
	NeedsReconnect bool `json:"needsReconnect"`
	// Language is the UI language in effect (config choice or system).
	Language string `json:"language"`
	// ChatReady: the Twitch login may read and write chat (song requests).
	ChatReady bool `json:"chatReady"`
	// SpotifyUser is the connected Spotify account ("" = not connected).
	SpotifyUser         string `json:"spotifyUser"`
	SpotifyLoginPending bool   `json:"spotifyLoginPending"`
	SpotifyRedirectURI  string `json:"spotifyRedirectUri"`
	// Token health for the dashboard ("Logins" panel).
	TwitchHealth  twitch.Health  `json:"twitchHealth"`
	SpotifyHealth spotify.Health `json:"spotifyHealth"`
	// FollowerStatus: unfollow detection state (nil = off).
	FollowerStatus *twitch.FollowerStatus `json:"followerStatus"`
	// Autostart: darkbot starts when the user logs in to the computer.
	Autostart          bool `json:"autostart"`
	AutostartSupported bool `json:"autostartSupported"`
	// TraySupported: closing can hide darkbot to the tray (Windows).
	TraySupported bool `json:"traySupported"`
	// Channel: followers and subs as they are on Twitch now.
	Channel ChannelTotals `json:"channel"`
}

// GetState returns the current UI state.
func (a *App) GetState() State {
	s := State{Config: a.cfg.Get(), OBSConnected: a.obs.Connected(), SupportedFormats: audio.Extensions, Version: version.Version, Language: a.lang()}
	s.Config.OBS.Password = "" // never send secrets back to the UI
	if t := a.tok.Current(); t != nil {
		s.TwitchLogin = t.Login
		s.NeedsReconnect = !t.HasScopes()
		s.ChatReady = t.HasScope(twitch.ScopeReadChat) && t.HasScope(twitch.ScopeWriteChat)
	}
	if st := a.sp.Current(); st != nil {
		s.SpotifyUser = st.DisplayName
	}
	s.SpotifyRedirectURI = spotify.RedirectURI
	s.TwitchHealth, s.SpotifyHealth = a.tok.Health(), a.sp.Health()
	s.FollowerStatus = a.followerStatus()
	s.TraySupported = tray.Supported()
	s.Channel = a.channelTotals()
	s.AutostartSupported = autostart.Supported()
	if s.AutostartSupported {
		on, err := autostart.Enabled()
		if err != nil {
			a.log.Warnf("Autostart: could not read the setting: %v", err)
		}
		s.Autostart = on
	}
	a.mu.Lock()
	s.EventSubOnline = a.esConnected
	if a.loginCode != nil {
		s.LoginUserCode, s.LoginURI = a.loginCode.UserCode, a.loginCode.VerificationURI
	}
	s.Update, s.Updating = a.update, a.updating
	s.OBSWaiting, s.OBSAuthFailed = a.obsStatusLocked(s.OBSConnected)
	if a.obsScene != nil && s.OBSConnected {
		st := *a.obsScene
		s.OBSScene = &st
	}
	s.SpotifyLoginPending = a.spLogin != nil
	a.mu.Unlock()
	return s
}

func (a *App) emitState() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "state")
	}
	a.refreshTray()
}

// ReportAudioSupport is called once by the frontend at start so the log shows
// what the system's web view can do (helps when "my device is missing").
func (a *App) ReportAudioSupport(sinkSupported bool, outputs, named int) {
	if sinkSupported {
		a.log.Infof("Audio: output device selection supported, %d output device(s) visible (%d with names)", outputs, named)
	} else {
		a.log.Warnf("Audio: this web view cannot select an output device, sounds use the system default (use OBS mode for track routing)")
	}
}

// GetLogs returns all log lines.
func (a *App) GetLogs() []string { return a.log.Lines() }

// ---------- language ----------

// lang is the UI language in effect.
func (a *App) lang() string {
	a.mu.Lock()
	sys := a.sysLangs
	a.mu.Unlock()
	return i18n.Resolve(a.cfg.Get().Language, sys)
}

// uiErr translates an error for the UI (Wails passes err.Error() to JS).
func (a *App) uiErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(i18n.Localize(err, a.lang()))
}

// ReportSystemLanguage is called by the frontend at start with
// navigator.languages, the only portable way to learn the system language.
func (a *App) ReportSystemLanguage(langs []string) {
	a.mu.Lock()
	a.sysLangs = append([]string(nil), langs...)
	a.mu.Unlock()
	a.log.Infof("System languages %v, UI language %s", langs, a.lang())
}

// SetLanguage stores the UI language; "" follows the system.
func (a *App) SetLanguage(code string) error {
	if code != "" && !i18n.IsSupported(code) {
		return a.uiErr(i18n.E("err.language", "lang", code))
	}
	if err := a.cfg.Update(func(c *config.Config) { c.Language = code }); err != nil {
		return a.uiErr(err)
	}
	a.emitState()
	return nil
}

// GetTranslations returns all UI strings for lang ("" = the language in
// effect), English filling any gaps.
func (a *App) GetTranslations(lang string) map[string]string {
	if !i18n.IsSupported(lang) {
		lang = a.lang()
	}
	return i18n.Dict(lang)
}

// GetLanguages lists the shipped UI languages.
func (a *App) GetLanguages() []string { return i18n.Supported }

// ---------- Twitch ----------

// StartLogin begins the device code flow and opens the activation page.
// The Client ID is built in (defaultClientID); it is missing only in a fork
// built without one.
func (a *App) StartLogin() error {
	clientID := a.cfg.Get().TwitchClientID
	if clientID == "" {
		return a.uiErr(i18n.E("err.noClientID"))
	}
	a.tok.SetClientID(clientID)
	a.CancelLogin()

	dc, err := twitch.StartDeviceFlow(a.ctx, clientID)
	if err != nil {
		return a.uiErr(err)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.loginCancel, a.loginCode = cancel, dc
	a.mu.Unlock()
	a.emitState()
	runtime.BrowserOpenURL(a.ctx, dc.VerificationURI)
	a.log.Infof("Twitch login started, confirm code %s in your browser", dc.UserCode)

	go func() {
		defer a.clearLogin()
		tok, err := twitch.PollDeviceFlow(ctx, clientID, dc)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				a.log.Errorf("Twitch login failed: %v", err)
			}
			return
		}
		if err := a.tok.Login(ctx, tok); err != nil {
			a.log.Errorf("Twitch login failed: %v", err)
			return
		}
		a.log.Infof("Logged in to Twitch as %s", tok.Login)
		a.startEventSub()
	}()
	return nil
}

func (a *App) clearLogin() {
	a.mu.Lock()
	a.loginCancel, a.loginCode = nil, nil
	a.mu.Unlock()
	a.emitState()
}

// CancelLogin aborts a running device code login.
func (a *App) CancelLogin() {
	a.mu.Lock()
	cancel := a.loginCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Logout removes the Twitch token and stops listening.
func (a *App) Logout() error {
	a.stopEventSub()
	err := a.tok.Logout()
	a.log.Infof("Logged out of Twitch")
	return a.uiErr(err)
}

func (a *App) startEventSub() {
	a.stopEventSub()
	ctx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.esCancel = cancel
	a.mu.Unlock()

	es := twitch.NewEventSub(a.hx, a.tok, a.log, a.bot.HandleRedemption, func(up bool) {
		a.mu.Lock()
		a.esConnected = up
		a.mu.Unlock()
		a.emitState()
	})
	es.OnChannelEvent(a.onChannelEvent)
	es.OnChat(func(m twitch.ChatMessage) {
		// EventSub delivers on its read loop; Spotify calls must not block it.
		if songs.IsCommand(m.Text) {
			go a.sr.Handle(ctx, m)
		}
	})
	go es.Run(ctx)
	a.startFollowerWatcher()
}

func (a *App) stopEventSub() {
	a.stopFollowerWatcher()
	a.mu.Lock()
	cancel := a.esCancel
	a.esCancel, a.esConnected = nil, false
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.emitState()
}

// GetRewards lists the channel's custom rewards for the reward picker.
func (a *App) GetRewards() ([]twitch.Reward, error) {
	t := a.tok.Current()
	if t == nil {
		return nil, a.uiErr(twitch.ErrNotLoggedIn)
	}
	ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
	defer cancel()
	rewards, err := a.hx.GetCustomRewards(ctx, t.UserID)
	return rewards, a.uiErr(err)
}

// ---------- OBS ----------

// OBSSettings is what the UI sends for OBS. Password is only changed when
// PasswordChanged is true, because the UI never receives the stored one.
type OBSSettings struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Password        string `json:"password"`
	PasswordChanged bool   `json:"passwordChanged"`
	AutoConnect     bool   `json:"autoConnect"`
	InputName       string `json:"inputName"`
	Tracks          []bool `json:"tracks"`
}

// SaveOBS stores OBS settings.
func (a *App) SaveOBS(s OBSSettings) error {
	return a.uiErr(a.cfg.Update(func(c *config.Config) {
		c.OBS.Host = strings.TrimSpace(s.Host)
		c.OBS.Port = s.Port
		if s.PasswordChanged {
			c.OBS.Password = s.Password
		}
		c.OBS.AutoConnect = s.AutoConnect
		c.OBS.InputName = strings.TrimSpace(s.InputName)
		for i := range c.OBS.Tracks {
			c.OBS.Tracks[i] = i < len(s.Tracks) && s.Tracks[i]
		}
	}))
}

// ---------- output & sounds ----------

// SaveOutput stores the global output settings.
func (a *App) SaveOutput(mode string, deviceID, deviceName string, master float64) error {
	m := config.OutputMode(mode)
	if m != config.OutputDevice && m != config.OutputOBS {
		return a.uiErr(i18n.E("err.unknownMode", "mode", mode))
	}
	return a.uiErr(a.cfg.Update(func(c *config.Config) {
		c.OutputMode, c.AudioDeviceID, c.AudioDeviceName, c.MasterVolume = m, deviceID, deviceName, master
	}))
}

// SetMasterVolume changes only the master volume (status bar slider).
func (a *App) SetMasterVolume(v float64) error {
	return a.uiErr(a.cfg.Update(func(c *config.Config) { c.MasterVolume = v }))
}

// PickSoundFile opens a native file dialog.
func (a *App) PickSoundFile() (string, error) {
	lang := a.lang()
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: i18n.T(lang, "dialog.chooseSound"),
		Filters: []runtime.FileFilter{{
			DisplayName: i18n.T(lang, "dialog.audioFiles", "ext", strings.Join(audio.Extensions, ", ")),
			Pattern:     "*" + strings.Join(audio.Extensions, ";*"),
		}},
	})
	return path, a.uiErr(err)
}

// SaveSound adds or updates a sound (matched by ID).
func (a *App) SaveSound(s config.Sound) (config.Sound, error) {
	s, err := a.saveSound(s)
	return s, a.uiErr(err)
}

func (a *App) saveSound(s config.Sound) (config.Sound, error) {
	s.File = strings.TrimSpace(s.File)
	s.RewardTitle = strings.TrimSpace(s.RewardTitle)
	if s.File == "" {
		return s, i18n.E("err.chooseFile")
	}
	if !filepath.IsAbs(s.File) {
		return s, i18n.E("err.pathAbsolute")
	}
	if _, err := audio.Duration(s.File); err != nil {
		return s, i18n.Wrap(err, "err.readFile")
	}
	if s.ID == "" {
		s.ID = config.NewID()
	}

	if s.Managed {
		created, err := a.syncReward(&s)
		if err != nil {
			return s, err
		}
		if err := a.storeSound(s); err != nil {
			if created {
				// Don't leave an orphan reward on Twitch.
				a.deleteReward(s.RewardID)
			}
			return s, err
		}
		return s, nil
	}

	if s.RewardID == "" && s.RewardTitle == "" {
		return s, i18n.E("err.chooseReward")
	}
	return s, a.storeSound(s)
}

func (a *App) storeSound(s config.Sound) error {
	err := a.cfg.Update(func(c *config.Config) {
		for i := range c.Sounds {
			if c.Sounds[i].ID == s.ID {
				c.Sounds[i] = s
				return
			}
		}
		c.Sounds = append(c.Sounds, s)
	})
	a.emitState()
	return err
}

// syncReward creates or updates the Twitch reward of a managed sound.
// It reports whether a new reward was created.
func (a *App) syncReward(s *config.Sound) (bool, error) {
	t, err := a.requireManageScope()
	if err != nil {
		return false, err
	}
	if n := utf8.RuneCountInString(s.RewardTitle); n < 1 || n > twitch.MaxRewardTitle {
		return false, i18n.E("err.rewardName", "max", twitch.MaxRewardTitle)
	}
	if s.RewardCost < twitch.MinRewardCost || s.RewardCost > twitch.MaxRewardCost {
		return false, i18n.E("err.price", "min", twitch.MinRewardCost, "max", twitch.MaxRewardCost)
	}
	color, ok := twitch.NormalizeRewardColor(s.RewardColor)
	if !ok {
		return false, i18n.E("err.rewardColor")
	}
	s.RewardColor = color
	settings := twitch.RewardSettings{Title: s.RewardTitle, Cost: s.RewardCost, Enabled: s.Enabled, CooldownSeconds: s.CooldownSeconds, BackgroundColor: color}
	ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
	defer cancel()

	if s.RewardID != "" {
		err := a.hx.UpdateReward(ctx, t.UserID, s.RewardID, settings)
		if err == nil {
			a.log.Infof("Updated reward %q on Twitch", s.RewardTitle)
			return false, nil
		}
		if !errors.Is(err, twitch.ErrNotFound) {
			return false, i18n.Wrap(err, "err.updateReward")
		}
		a.log.Warnf("Reward %q was deleted on Twitch, creating it again", s.RewardTitle)
	}
	r, err := a.hx.CreateReward(ctx, t.UserID, settings)
	if err != nil {
		return false, i18n.Wrap(err, "err.createReward")
	}
	s.RewardID = r.ID
	if s.RewardColor == "" {
		// Twitch picked a random color: remember it so darkbot shows it.
		s.RewardColor, _ = twitch.NormalizeRewardColor(r.BackgroundColor)
	}
	a.log.Infof("Created reward %q (%d points) on Twitch", s.RewardTitle, s.RewardCost)
	return true, nil
}

func (a *App) requireManageScope() (*twitch.Token, error) {
	t := a.tok.Current()
	if t == nil {
		return nil, i18n.E("err.connectFirst")
	}
	if !t.HasScope(twitch.ScopeManageRedemptions) {
		return nil, i18n.E("err.reconnect")
	}
	return t, nil
}

func (a *App) deleteReward(rewardID string) {
	t := a.tok.Current()
	if t == nil || rewardID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
	defer cancel()
	if err := a.hx.DeleteReward(ctx, t.UserID, rewardID); err != nil {
		a.log.Errorf("Delete reward on Twitch: %v", err)
	}
}

// DeleteSound removes a sound. For a reward darkbot created, the reward is
// deleted on Twitch first, so no orphan reward stays on the channel.
func (a *App) DeleteSound(id string) error { return a.uiErr(a.deleteSound(id)) }

func (a *App) deleteSound(id string) error {
	var target *config.Sound
	for _, s := range a.cfg.Get().Sounds {
		if s.ID == id {
			target = &s
			break
		}
	}
	if target == nil {
		return nil
	}
	if target.Managed && target.RewardID != "" {
		t, err := a.requireManageScope()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
		defer cancel()
		if err := a.hx.DeleteReward(ctx, t.UserID, target.RewardID); err != nil {
			return i18n.Wrap(err, "err.deleteReward")
		}
		a.log.Infof("Deleted reward %q on Twitch", target.RewardTitle)
	}
	err := a.cfg.Update(func(c *config.Config) {
		out := c.Sounds[:0]
		for _, s := range c.Sounds {
			if s.ID != id {
				out = append(out, s)
			}
		}
		c.Sounds = out
	})
	a.emitState()
	return err
}

// resolveRedemption fulfills a played redemption or rejects one that
// couldn't play (cooldown, off, queue full, error).
func (a *App) resolveRedemption(r twitch.Redemption, fulfill bool) {
	t := a.tok.Current()
	if t == nil {
		return
	}
	status := twitch.StatusCanceled
	if fulfill {
		status = twitch.StatusFulfilled
	}
	ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
	defer cancel()
	if err := a.hx.UpdateRedemptionStatus(ctx, t.UserID, r.RewardID, r.ID, status); err != nil {
		a.log.Warnf("Could not mark redemption of %q by %s as %s: %v", r.RewardTitle, r.UserName, strings.ToLower(status), err)
		return
	}
	if !fulfill {
		a.log.Infof("Rejected %s's redemption of %q", r.UserName, r.RewardTitle)
	}
}

// TestSound plays a sound ignoring cooldowns, in turn with the queue.
func (a *App) TestSound(id string) {
	a.bot.Test(id)
}

// SoundDone is called by the frontend when playback finished. errMsg is
// empty on success.
func (a *App) SoundDone(token, errMsg string) { a.web.done(token, errMsg) }

// serveSound serves configured sound files to the webview. Only files that
// are configured as a sound (looked up by ID) are served — never arbitrary
// paths. This runs inside Wails' in-process asset server: no network port.
func (a *App) serveSound(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, soundURLPrefix) || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, soundURLPrefix)
	if file, ok := a.eventSoundFile(id); ok {
		w.Header().Set("Content-Type", audio.MIMEType(file))
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, file)
		return
	}
	for _, s := range a.cfg.Get().Sounds {
		if s.ID == id {
			w.Header().Set("Content-Type", audio.MIMEType(s.File))
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, s.File)
			return
		}
	}
	http.NotFound(w, r)
}

// webOutput plays sounds in the webview on the selected audio device.
type webOutput struct {
	app     *App
	mu      sync.Mutex
	pending map[string]chan string
}

// playRequest is sent to the frontend.
type playRequest struct {
	Token    string  `json:"token"`
	URL      string  `json:"url"`
	Volume   float64 `json:"volume"`
	DeviceID string  `json:"deviceId"`
}

func (o *webOutput) Play(ctx context.Context, s config.Sound, master float64) error {
	length, err := audio.Duration(s.File)
	if err != nil {
		return err
	}
	token := config.NewID()
	ch := make(chan string, 1)
	o.mu.Lock()
	o.pending[token] = ch
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.pending, token)
		o.mu.Unlock()
	}()

	cfg := o.app.cfg.Get()
	runtime.EventsEmit(o.app.ctx, "play", playRequest{
		Token:    token,
		URL:      soundURLPrefix + s.ID + "?t=" + token,
		Volume:   min(s.Volume*master, config.MaxVolume),
		DeviceID: cfg.AudioDeviceID,
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case msg := <-ch:
		if msg != "" {
			return errors.New(msg)
		}
		return nil
	case <-time.After(length + playbackGrace):
		return i18n.E("err.noPlaybackConfirm", "seconds", int((length + playbackGrace).Round(time.Second).Seconds()))
	}
}

func (o *webOutput) done(token, errMsg string) {
	o.mu.Lock()
	ch := o.pending[token]
	o.mu.Unlock()
	if ch != nil {
		select {
		case ch <- errMsg:
		default:
		}
	}
}

// ---------- song requests (Spotify) ----------

// sendChat answers a chat command as the broadcaster.
func (a *App) sendChat(ctx context.Context, text, replyTo string) {
	t := a.tok.Current()
	if t == nil || !t.HasScope(twitch.ScopeWriteChat) {
		a.log.Warnf("Can't answer in chat: reconnect Twitch once to allow sending chat messages")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, chatSendTimeout)
	defer cancel()
	if err := a.hx.SendChatMessage(ctx, t.UserID, text, replyTo); err != nil {
		a.log.Warnf("Chat reply failed: %v", err)
	}
}

// SaveSongSettings stores the song request settings.
func (a *App) SaveSongSettings(s config.SongRequests) error {
	err := a.cfg.Update(func(c *config.Config) { c.Songs = s })
	if err == nil {
		a.sp.SetClientID(a.cfg.Get().Songs.SpotifyClientID)
		a.emitState()
	}
	return a.uiErr(err)
}

// SpotifyStartLogin opens Spotify's consent page in the browser. The user
// then pastes the address they land on into SpotifyFinishLogin.
func (a *App) SpotifyStartLogin() error {
	p, err := spotify.NewLogin(a.cfg.Get().Songs.SpotifyClientID)
	if err != nil {
		return a.uiErr(err)
	}
	a.mu.Lock()
	a.spLogin = p
	a.mu.Unlock()
	a.emitState()
	runtime.BrowserOpenURL(a.ctx, p.URL)
	a.log.Infof("Spotify login started in the browser")
	return nil
}

// SpotifyOpenLogin opens the pending login page again (closed tab etc.).
func (a *App) SpotifyOpenLogin() {
	a.mu.Lock()
	p := a.spLogin
	a.mu.Unlock()
	if p != nil {
		runtime.BrowserOpenURL(a.ctx, p.URL)
	}
}

// SpotifyFinishLogin takes the pasted redirect address (or the bare code).
func (a *App) SpotifyFinishLogin(pasted string) error {
	a.mu.Lock()
	p := a.spLogin
	a.mu.Unlock()
	if p == nil {
		return a.uiErr(i18n.E("err.spotifyNoLogin"))
	}
	code, err := p.CodeFromPaste(pasted)
	if err != nil {
		return a.uiErr(err)
	}
	ctx, cancel := context.WithTimeout(a.ctx, spotifyLoginTimeout)
	defer cancel()
	tok, err := a.sp.Login(ctx, p, code)
	if err != nil {
		a.log.Errorf("Spotify login failed: %v", err)
		return a.uiErr(err)
	}
	a.mu.Lock()
	a.spLogin = nil
	a.mu.Unlock()
	a.log.Infof("Connected to Spotify as %s", tok.DisplayName)
	a.emitState()
	return nil
}

// SpotifyCancelLogin drops a pending login.
func (a *App) SpotifyCancelLogin() {
	a.mu.Lock()
	a.spLogin = nil
	a.mu.Unlock()
	a.emitState()
}

// SpotifyLogout forgets the Spotify login.
func (a *App) SpotifyLogout() error {
	err := a.sp.Logout()
	a.log.Infof("Disconnected from Spotify")
	return a.uiErr(err)
}

// ---------- dashboard ----------

// countActivity feeds sound events into the dashboard statistics. Every
// redemption produces exactly one of queued/cooldown/disabled/queue_full;
// previews (Test) are never counted.
func (a *App) countActivity(act bot.Activity) {
	if act.Test || act.Event {
		return
	}
	switch act.Kind {
	case bot.ActQueued:
		a.st.Redemption(act.User, act.RewardTitle, true)
	case bot.ActCooldown, bot.ActDisabled, bot.ActQueueFull:
		a.st.Redemption(act.User, act.RewardTitle, false)
	case bot.ActDone:
		a.st.Played()
	case bot.ActError:
		a.st.Failed()
	}
}

// GetStats returns the dashboard numbers.
func (a *App) GetStats() stats.Snapshot { return a.st.Snapshot() }

// ResetSessionStats starts counting a new session (e.g. a new stream).
func (a *App) ResetSessionStats() {
	a.st.ResetSession()
	a.log.Infof("Dashboard session counters reset")
}

// ---------- logins & autostart ----------

// tokenRefreshTimeout bounds the "renew now" buttons.
const tokenRefreshTimeout = 30 * time.Second

// RefreshTwitchToken renews the Twitch access token now (dashboard).
func (a *App) RefreshTwitchToken() error {
	ctx, cancel := context.WithTimeout(a.ctx, tokenRefreshTimeout)
	defer cancel()
	err := a.tok.RefreshNow(ctx)
	a.emitState()
	return a.uiErr(err)
}

// RefreshSpotifyToken renews the Spotify access token now (dashboard).
func (a *App) RefreshSpotifyToken() error {
	ctx, cancel := context.WithTimeout(a.ctx, tokenRefreshTimeout)
	defer cancel()
	err := a.sp.RefreshNow(ctx)
	if err == nil {
		a.log.Infof("Spotify token refreshed")
	}
	a.emitState()
	return a.uiErr(err)
}

// SetAutostart switches starting darkbot at login on or off.
func (a *App) SetAutostart(on bool) error {
	if err := autostart.Set(on); err != nil {
		a.log.Errorf("Autostart: %v", err)
		return a.uiErr(i18n.Wrap(err, "err.autostart"))
	}
	if on {
		a.log.Infof("Autostart on: darkbot starts when you log in")
	} else {
		a.log.Infof("Autostart off")
	}
	a.emitState()
	return nil
}
