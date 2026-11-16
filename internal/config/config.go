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
	"os"
	"path/filepath"
	"sync"
)

const (
	appDirName     = "darkbot"
	configFileName = "config.json"
	tokenFileName  = "token.json"
	filePerm       = 0o600
	dirPerm        = 0o700
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
	DefaultOBSHost        = "localhost"
	DefaultOBSPort        = 4455 // obs-websocket v5 default port
	DefaultOBSInputName   = "darkbot-sound"
	DefaultSoundVolume    = 1.0
	DefaultMasterVolume   = 1.0
	MaxVolume             = 1.0 // 100 %: HTML audio elements cannot amplify
	DefaultCooldownSecond = 0
)

// Sound maps one channel point reward to one audio file.
type Sound struct {
	ID string `json:"id"`
	// RewardID is the Twitch custom reward ID (stable even if the title changes).
	RewardID string `json:"reward_id"`
	// RewardTitle is used for display and as fallback match if RewardID is empty.
	RewardTitle string `json:"reward_title"`
	File        string `json:"file"`
	// Volume is a linear factor, 1.0 = 100 %.
	Volume float64 `json:"volume"`
	// CooldownSeconds blocks the sound for everyone after it was triggered.
	CooldownSeconds int `json:"cooldown_seconds"`
	// UserCooldownSeconds blocks the sound for the same viewer.
	UserCooldownSeconds int  `json:"user_cooldown_seconds"`
	Enabled             bool `json:"enabled"`
}

// OBS holds the obs-websocket connection and the media-source output settings.
type OBS struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
	// AutoConnect connects to OBS on app start.
	AutoConnect bool `json:"auto_connect"`
	// InputName is the media source darkbot creates/uses in OBS-output mode.
	InputName string `json:"input_name"`
	// Tracks are OBS audio tracks 1..6 the media source is routed to.
	Tracks [OBSTrackCount]bool `json:"tracks"`
}

// Config is the whole persisted app configuration.
type Config struct {
	TwitchClientID string     `json:"twitch_client_id"`
	OutputMode     OutputMode `json:"output_mode"`
	// AudioDeviceID is the web audio output "sinkId" chosen in the UI;
	// empty = system default output.
	AudioDeviceID   string  `json:"audio_device_id"`
	AudioDeviceName string  `json:"audio_device_name"`
	MasterVolume    float64 `json:"master_volume"`
	OBS             OBS     `json:"obs"`
	Sounds          []Sound `json:"sounds"`
}

// Default returns a config with sensible defaults.
func Default() Config {
	return Config{
		OutputMode:   OutputDevice,
		MasterVolume: DefaultMasterVolume,
		OBS: OBS{
			Host:      DefaultOBSHost,
			Port:      DefaultOBSPort,
			InputName: DefaultOBSInputName,
			Tracks:    [OBSTrackCount]bool{true}, // track 1 only
		},
	}
}

// normalize fixes zero/invalid values (e.g. from older or hand-edited files).
func (c *Config) normalize() {
	if c.OutputMode != OutputDevice && c.OutputMode != OutputOBS {
		c.OutputMode = OutputDevice
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
	if c.OBS.InputName == "" {
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
	}
}

// Clone returns a deep copy so callers never share the Sounds slice.
func (c Config) Clone() Config {
	out := c
	out.Sounds = append([]Sound(nil), c.Sounds...)
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
}

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

// Open loads the config from dir (missing file = defaults).
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, cfg: Default()}
	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		// first start: keep defaults
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	default:
		cfg := Default()
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", filepath.Join(dir, configFileName), err)
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
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(filePerm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		// Chmod is a no-op/unsupported on Windows; ignore only that case.
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}
