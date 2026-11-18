// Package obs talks to OBS Studio via obs-websocket v5 (built into OBS 28+).
// Protocol: https://github.com/obsproject/obs-websocket/blob/master/docs/generated/protocol.md
//
// In OBS-output mode darkbot plays sounds through ONE media source
// (default name "[darkbot] sound"). For every sound it sets the file, volume
// and the audio tracks (1–6), then restarts the source.
//
// OBS only outputs audio of sources that are in the active scene. darkbot
// creates the source in the current program scene; to hear sounds in every
// scene, add it to your other scenes too ("Add existing source"), or put it in
// a scene you nest everywhere.
package obs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/closecodes"
	"github.com/andreykaipov/goobs/api/events"
	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/mediainputs"
	"github.com/andreykaipov/goobs/api/requests/scenes"
	"github.com/andreykaipov/goobs/api/typedefs"
	"github.com/gorilla/websocket"

	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

const (
	mediaSourceKind = "ffmpeg_source"
	restartAction   = "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART"
	connectTimeout  = 5 * time.Second
	// endPadding gives OBS a moment after the calculated length.
	endPadding = 300 * time.Millisecond
	// PingEvery: how often a live connection is checked. A closed OBS is
	// usually noticed at once (OBS says goodbye); the ping catches the rest
	// (crash, frozen OBS) within PingEvery + connectTimeout.
	PingEvery = 5 * time.Second
	// DropWithin: a dead connection is noticed at the latest after this
	// (next ping plus its response timeout).
	DropWithin = PingEvery + connectTimeout
)

// ErrNotConnected is returned when OBS is not connected.
var ErrNotConnected = i18n.E("err.obsOffline")

// ErrAuth means OBS rejected the password (close code 4009). Retrying
// with the same password is pointless.
var ErrAuth = i18n.E("err.obsAuth")

// Client is a thread-safe wrapper around a goobs connection.
type Client struct {
	dial     sync.Mutex // one Connect at a time (user click vs. reconnect loop)
	mu       sync.Mutex
	c        *goobs.Client
	onDrop   func(error)
	onScenes func() // a scene was added, removed or renamed in OBS
}

// OnScenesChanged sets what runs when the streamer adds, removes or
// renames a scene in OBS. Call before Connect.
func (o *Client) OnScenesChanged(fn func()) { o.onScenes = fn }

// New creates an unconnected client. onDrop is called when a live
// connection ends without Disconnect (OBS closed, crashed, stopped its
// websocket server).
func New(onDrop func(error)) *Client { return &Client{onDrop: onDrop} }

// Connect (re)connects to OBS.
func (o *Client) Connect(host string, port int, password string) (version string, err error) {
	o.dial.Lock()
	defer o.dial.Unlock()
	o.Disconnect()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	opts := []goobs.Option{goobs.WithResponseTimeoutDuration(connectTimeout)}
	if password != "" {
		opts = append(opts, goobs.WithPassword(password))
	}
	c, err := goobs.New(addr, opts...)
	if err != nil {
		var ce *websocket.CloseError
		if errors.As(err, &ce) && ce.Code == closecodes.AuthenticationFailed {
			return "", ErrAuth
		}
		return "", i18n.Wrap(err, "err.obsConnect", "addr", addr)
	}
	v, err := c.General.GetVersion()
	if err != nil {
		c.Disconnect()
		return "", fmt.Errorf("OBS version: %w", err)
	}
	o.mu.Lock()
	o.c = c
	o.mu.Unlock()
	go o.watch(c)
	return fmt.Sprintf("OBS %s, websocket %s", v.ObsVersion, v.ObsWebSocketVersion), nil
}

// watch notices when connection c ends: goobs closes IncomingEvents when
// OBS closes the websocket, and a failing ping covers silent deaths. It
// stops quietly when c was replaced or closed by Disconnect.
func (o *Client) watch(c *goobs.Client) {
	tick := time.NewTicker(PingEvery)
	defer tick.Stop()
	for {
		select {
		case ev, open := <-c.IncomingEvents:
			if open {
				switch ev.(type) {
				case *events.SceneCreated, *events.SceneRemoved, *events.SceneNameChanged:
					if o.onScenes != nil {
						go o.onScenes()
					}
				}
				continue
			}
			o.dropIf(c, errors.New("OBS closed the connection"))
			return
		case <-tick.C:
			if !o.current(c) {
				return
			}
			if err := ping(c); err != nil {
				o.dropIf(c, err)
				return
			}
		}
	}
}

// ping reports a hung OBS: no answer within the response timeout. An
// answer with an error status (e.g. NotReady 207 while OBS loads a scene
// collection or starts to quit) means OBS is alive, so it is no drop; a
// closing OBS is noticed through the closed connection instead. goobs
// returns both cases as plain errors, so the elapsed time tells them apart.
func ping(c *goobs.Client) error {
	start := time.Now()
	_, err := c.General.GetVersion()
	if err == nil || time.Since(start) < connectTimeout {
		return nil
	}
	return fmt.Errorf("OBS stopped answering: %w", err)
}

func (o *Client) current(c *goobs.Client) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.c == c
}

// dropIf ends connection c and reports it, unless it isn't the current one
// any more (Disconnect or a newer Connect).
func (o *Client) dropIf(c *goobs.Client, err error) {
	o.mu.Lock()
	if o.c != c {
		o.mu.Unlock()
		return
	}
	o.c = nil
	o.mu.Unlock()
	_ = c.Disconnect()
	if o.onDrop != nil {
		o.onDrop(err)
	}
}

// Disconnect closes the connection (safe to call when not connected).
func (o *Client) Disconnect() {
	o.mu.Lock()
	c := o.c
	o.c = nil
	o.mu.Unlock()
	if c != nil {
		_ = c.Disconnect()
	}
}

// Connected reports whether a connection exists.
func (o *Client) Connected() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.c != nil
}

func (o *Client) client() (*goobs.Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.c == nil {
		return nil, ErrNotConnected
	}
	return o.c, nil
}

// fail drops the connection after a request error so the UI shows the truth.
func (o *Client) fail(c *goobs.Client, err error) error {
	o.dropIf(c, err)
	return err
}

// Output plays sounds through the OBS media source.
type Output struct {
	OBS *Client
	Cfg *config.Store
}

// Play implements bot.Output.
func (p *Output) Play(ctx context.Context, s config.Sound, master float64) error {
	c, err := p.OBS.client()
	if err != nil {
		return err
	}
	length, err := audio.Duration(s.File)
	if err != nil {
		return err
	}
	cfg := p.Cfg.Get().OBS
	if err := ensureInput(c, cfg.InputName); err != nil {
		return p.OBS.fail(c, err)
	}

	tracks := typedefs.InputAudioTracks{}
	for i, on := range cfg.Tracks {
		tracks[strconv.Itoa(i+1)] = on
	}
	vol := s.Volume * master
	steps := []func() error{
		func() error {
			_, err := c.Inputs.SetInputSettings(inputs.NewSetInputSettingsParams().
				WithInputName(cfg.InputName).
				WithInputSettings(map[string]any{"is_local_file": true, "local_file": s.File}).
				WithOverlay(true))
			return err
		},
		func() error {
			_, err := c.Inputs.SetInputVolume(inputs.NewSetInputVolumeParams().
				WithInputName(cfg.InputName).WithInputVolumeMul(vol))
			return err
		},
		func() error {
			_, err := c.Inputs.SetInputAudioTracks(inputs.NewSetInputAudioTracksParams().
				WithInputName(cfg.InputName).WithInputAudioTracks(&tracks))
			return err
		},
		func() error {
			_, err := c.MediaInputs.TriggerMediaInputAction(mediainputs.NewTriggerMediaInputActionParams().
				WithInputName(cfg.InputName).WithMediaAction(restartAction))
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return p.OBS.fail(c, fmt.Errorf("OBS request: %w", err))
		}
	}

	// Wait until the sound finished so the queue plays sounds one by one.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(length + endPadding):
		return nil
	}
}

// ensureInput creates the media source in the current scene if missing.
// The source under its pre-0.2.7 default name is renamed instead, should a
// sound come before the scene check after connecting has renamed it.
func ensureInput(c *goobs.Client, name string) error {
	list, err := c.Inputs.GetInputList(inputs.NewGetInputListParams().WithInputKind(mediaSourceKind))
	if err != nil {
		return fmt.Errorf("list OBS inputs: %w", err)
	}
	legacy := false
	for _, in := range list.Inputs {
		if in.InputName == name {
			return nil
		}
		legacy = legacy || (name == config.DefaultOBSInputName && in.InputName == config.LegacyOBSInputName)
	}
	if legacy {
		if _, err := c.Inputs.SetInputName(inputs.NewSetInputNameParams().
			WithInputName(config.LegacyOBSInputName).WithNewInputName(name)); err != nil {
			return fmt.Errorf("rename OBS source %q to %q: %w", config.LegacyOBSInputName, name, err)
		}
		return nil
	}
	scene, err := c.Scenes.GetCurrentProgramScene(&scenes.GetCurrentProgramSceneParams{})
	if err != nil {
		return fmt.Errorf("current OBS scene: %w", err)
	}
	_, err = c.Inputs.CreateInput(inputs.NewCreateInputParams().
		WithSceneName(scene.SceneName).
		WithInputName(name).
		WithInputKind(mediaSourceKind).
		WithInputSettings(map[string]any{
			"is_local_file":       true,
			"restart_on_activate": false,
			"close_when_inactive": true,
		}).
		WithSceneItemEnabled(true))
	if err != nil {
		return fmt.Errorf("create OBS media source %q: %w", name, err)
	}
	return nil
}
