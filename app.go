package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwi/darkbot/internal/applog"
	"github.com/miwi/darkbot/internal/audio"
	"github.com/miwi/darkbot/internal/bot"
	"github.com/miwi/darkbot/internal/config"
	"github.com/miwi/darkbot/internal/obs"
	"github.com/miwi/darkbot/internal/twitch"
)

// defaultClientID can be baked into a release build:
//
//	wails build -ldflags "-X main.defaultClientID=<your twitch client id>"
//
// Otherwise the user enters their own Client ID in the app.
var defaultClientID = ""

const (
	// playbackGrace is added to a sound's length before we give up waiting
	// for the webview to report the end of playback.
	playbackGrace  = 5 * time.Second
	rewardsTimeout = 15 * time.Second
	soundURLPrefix = "/sound/"
)

// App is bound to the frontend: every exported method is callable from JS.
type App struct {
	ctx  context.Context
	log  *applog.Log
	cfg  *config.Store
	tok  *twitch.TokenManager
	hx   *twitch.Helix
	obs  *obs.Client
	bot  *bot.Bot
	web  *webOutput
	obsO *obs.Output

	mu          sync.Mutex
	esCancel    context.CancelFunc
	esConnected bool
	loginCancel context.CancelFunc
	loginCode   *twitch.DeviceCode
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

	a := &App{log: log, cfg: cfg, tok: tok, hx: twitch.NewHelix(tok)}
	a.obs = obs.New(func(err error) {
		log.Errorf("OBS connection lost: %v", err)
		a.emitState()
	})
	a.web = &webOutput{app: a, pending: map[string]chan string{}}
	a.obsO = &obs.Output{OBS: a.obs, Cfg: cfg}
	a.bot = bot.New(cfg, log, a.currentOutput)
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
	a.log.OnChange(func() { runtime.EventsEmit(ctx, "log") })
	a.tok.OnChange(func(t *twitch.Token) {
		if t == nil {
			// Logged out (by the user or because Twitch rejected the refresh
			// token): stop listening so the status pills tell the truth.
			a.stopEventSub()
		}
		a.emitState()
	})

	go a.tok.Run(ctx)
	go a.bot.Run(ctx)

	if a.tok.Current() != nil {
		a.startEventSub()
	} else {
		a.log.Infof("Not logged in to Twitch yet")
	}
	if c := a.cfg.Get().OBS; c.AutoConnect {
		go func() { _ = a.ConnectOBS() }()
	}
}

func (a *App) shutdown(context.Context) {
	a.obs.Disconnect()
}

// ---------- state ----------

// State is everything the UI renders.
type State struct {
	Config           config.Config `json:"config"`
	TwitchLogin      string        `json:"twitchLogin"`
	EventSubOnline   bool          `json:"eventSubOnline"`
	OBSConnected     bool          `json:"obsConnected"`
	LoginUserCode    string        `json:"loginUserCode"`
	LoginURI         string        `json:"loginUri"`
	SupportedFormats []string      `json:"supportedFormats"`
}

// GetState returns the current UI state.
func (a *App) GetState() State {
	s := State{Config: a.cfg.Get(), OBSConnected: a.obs.Connected(), SupportedFormats: audio.Extensions}
	s.Config.OBS.Password = "" // never send secrets back to the UI
	if t := a.tok.Current(); t != nil {
		s.TwitchLogin = t.Login
	}
	a.mu.Lock()
	s.EventSubOnline = a.esConnected
	if a.loginCode != nil {
		s.LoginUserCode, s.LoginURI = a.loginCode.UserCode, a.loginCode.VerificationURI
	}
	a.mu.Unlock()
	return s
}

func (a *App) emitState() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "state")
	}
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

// ---------- Twitch ----------

// SetClientID saves the Twitch Client ID.
func (a *App) SetClientID(id string) error {
	id = strings.TrimSpace(id)
	if err := a.cfg.Update(func(c *config.Config) { c.TwitchClientID = id }); err != nil {
		return err
	}
	a.tok.SetClientID(id)
	return nil
}

// StartLogin begins the device code flow and opens the activation page.
func (a *App) StartLogin() error {
	clientID := a.cfg.Get().TwitchClientID
	if clientID == "" {
		return errors.New("enter your Twitch Client ID first")
	}
	a.tok.SetClientID(clientID)
	a.CancelLogin()

	dc, err := twitch.StartDeviceFlow(a.ctx, clientID)
	if err != nil {
		return err
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
	return err
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
	go es.Run(ctx)
}

func (a *App) stopEventSub() {
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
		return nil, twitch.ErrNotLoggedIn
	}
	ctx, cancel := context.WithTimeout(a.ctx, rewardsTimeout)
	defer cancel()
	return a.hx.GetCustomRewards(ctx, t.UserID)
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
	return a.cfg.Update(func(c *config.Config) {
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
	})
}

// ConnectOBS connects with the stored settings.
func (a *App) ConnectOBS() error {
	c := a.cfg.Get().OBS
	v, err := a.obs.Connect(c.Host, c.Port, c.Password)
	a.emitState()
	if err != nil {
		a.log.Errorf("%v", err)
		return err
	}
	a.log.Infof("Connected to %s", v)
	return nil
}

// DisconnectOBS closes the OBS connection.
func (a *App) DisconnectOBS() {
	a.obs.Disconnect()
	a.log.Infof("Disconnected from OBS")
	a.emitState()
}

// ---------- output & sounds ----------

// SaveOutput stores the global output settings.
func (a *App) SaveOutput(mode string, deviceID, deviceName string, master float64) error {
	m := config.OutputMode(mode)
	if m != config.OutputDevice && m != config.OutputOBS {
		return fmt.Errorf("unknown output mode %q", mode)
	}
	return a.cfg.Update(func(c *config.Config) {
		c.OutputMode, c.AudioDeviceID, c.AudioDeviceName, c.MasterVolume = m, deviceID, deviceName, master
	})
}

// PickSoundFile opens a native file dialog.
func (a *App) PickSoundFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a sound",
		Filters: []runtime.FileFilter{{
			DisplayName: "Audio (" + strings.Join(audio.Extensions, ", ") + ")",
			Pattern:     "*" + strings.Join(audio.Extensions, ";*"),
		}},
	})
}

// SaveSound adds or updates a sound (matched by ID).
func (a *App) SaveSound(s config.Sound) (config.Sound, error) {
	s.File = strings.TrimSpace(s.File)
	if s.File == "" {
		return s, errors.New("choose a sound file")
	}
	if !filepath.IsAbs(s.File) {
		return s, errors.New("the sound file path must be absolute")
	}
	if _, err := audio.Duration(s.File); err != nil {
		return s, fmt.Errorf("cannot read sound file: %w", err)
	}
	if s.RewardID == "" && strings.TrimSpace(s.RewardTitle) == "" {
		return s, errors.New("choose a channel point reward")
	}
	if s.ID == "" {
		s.ID = config.NewID()
	}
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
	return s, err
}

// DeleteSound removes a sound.
func (a *App) DeleteSound(id string) error {
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

// TestSound plays a sound now (ignores cooldowns).
func (a *App) TestSound(id string) {
	go a.bot.Test(a.ctx, id)
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
		return fmt.Errorf("no playback confirmation from the app window after %s", (length + playbackGrace).Round(time.Second))
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
