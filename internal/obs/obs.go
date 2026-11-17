// Package obs talks to OBS Studio via obs-websocket v5 (built into OBS 28+).
// Protocol: https://github.com/obsproject/obs-websocket/blob/master/docs/generated/protocol.md
//
// In OBS-output mode darkbot plays sounds through ONE media source
// (default name "darkbot-sound"). For every sound it sets the file, volume
// and the audio tracks (1–6), then restarts the source.
//
// OBS only outputs audio of sources that are in the active scene. darkbot
// creates the source in the current program scene; to hear sounds in every
// scene, add it to your other scenes too ("Add existing source"), or put it in
// a scene you nest everywhere.
package obs

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/mediainputs"
	"github.com/andreykaipov/goobs/api/requests/scenes"
	"github.com/andreykaipov/goobs/api/typedefs"

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
)

// ErrNotConnected is returned when OBS is not connected.
var ErrNotConnected = i18n.E("err.obsOffline")

// Client is a thread-safe wrapper around a goobs connection.
type Client struct {
	mu     sync.Mutex
	c      *goobs.Client
	onDrop func(error)
}

// New creates an unconnected client. onDrop is called when a live
// connection fails a request (OBS closed etc.).
func New(onDrop func(error)) *Client { return &Client{onDrop: onDrop} }

// Connect (re)connects to OBS.
func (o *Client) Connect(host string, port int, password string) (version string, err error) {
	o.Disconnect()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	opts := []goobs.Option{goobs.WithResponseTimeoutDuration(connectTimeout)}
	if password != "" {
		opts = append(opts, goobs.WithPassword(password))
	}
	c, err := goobs.New(addr, opts...)
	if err != nil {
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
	return fmt.Sprintf("OBS %s, websocket %s", v.ObsVersion, v.ObsWebSocketVersion), nil
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
func (o *Client) fail(err error) error {
	o.Disconnect()
	if o.onDrop != nil {
		o.onDrop(err)
	}
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
		return p.OBS.fail(err)
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
			return p.OBS.fail(fmt.Errorf("OBS request: %w", err))
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
func ensureInput(c *goobs.Client, name string) error {
	list, err := c.Inputs.GetInputList(inputs.NewGetInputListParams().WithInputKind(mediaSourceKind))
	if err != nil {
		return fmt.Errorf("list OBS inputs: %w", err)
	}
	for _, in := range list.Inputs {
		if in.InputName == name {
			return nil
		}
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
