// Package config handles loading, saving and locating smsd's configuration
// and the XDG base directories the daemon uses for data, state and logs.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds all user-tunable settings. It is serialised as JSON under
// ~/.config/smsd/config.json. Zero values are replaced by defaults on load.
type Config struct {
	// ADBPath is the adb executable to use. If empty, "adb" is looked up on PATH.
	ADBPath string `json:"adb_path"`
	// DevicePollSeconds is how often the device list is polled for connect/
	// disconnect events. This is a cheap local operation.
	DevicePollSeconds int `json:"device_poll_seconds"`
	// SMSPollSeconds is how often a connected device is queried for new SMS.
	SMSPollSeconds int `json:"sms_poll_seconds"`
	// ContactsRefreshMinutes is how often the contact cache is refreshed while
	// a device is connected.
	ContactsRefreshMinutes int `json:"contacts_refresh_minutes"`
	// NotifyEnabled toggles desktop notifications for newly received SMS.
	NotifyEnabled bool `json:"notify_enabled"`
	// NotifyTimeoutSeconds is how long a notification stays on screen. 0 means
	// use the default; a negative value keeps it up until dismissed. Some
	// notification daemons ignore the hint.
	NotifyTimeoutSeconds int `json:"notify_timeout_seconds"`
	// UIAddr is the loopback address the read-only web viewer binds to.
	// Port 0 selects a random free port each launch.
	UIAddr string `json:"ui_addr"`
	// LogMaxBytes is the size at which the log file is rotated.
	LogMaxBytes int64 `json:"log_max_bytes"`
}

// Default returns a Config populated with sane defaults.
func Default() Config {
	return Config{
		ADBPath:                "",
		DevicePollSeconds:      2,
		SMSPollSeconds:         2,
		ContactsRefreshMinutes: 15,
		NotifyEnabled:          true,
		NotifyTimeoutSeconds:   30,
		UIAddr:                 "127.0.0.1:0",
		LogMaxBytes:            5 * 1024 * 1024,
	}
}

// Paths groups the XDG-derived directories and files smsd uses.
type Paths struct {
	ConfigDir string // ~/.config/smsd
	DataDir   string // ~/.local/share/smsd
	StateDir  string // ~/.local/state/smsd

	ConfigFile string // ~/.config/smsd/config.json
	DBFile     string // ~/.local/share/smsd/smsd.db
	LogFile    string // ~/.local/state/smsd/log.txt
}

func xdgDir(envVar, fallbackRel string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallbackRel), nil
}

// ResolvePaths computes the standard smsd paths, honouring XDG_* overrides.
func ResolvePaths() (Paths, error) {
	confBase, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Paths{}, err
	}
	dataBase, err := xdgDir("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return Paths{}, err
	}
	stateBase, err := xdgDir("XDG_STATE_HOME", ".local/state")
	if err != nil {
		return Paths{}, err
	}

	p := Paths{
		ConfigDir: filepath.Join(confBase, "smsd"),
		DataDir:   filepath.Join(dataBase, "smsd"),
		StateDir:  filepath.Join(stateBase, "smsd"),
	}
	p.ConfigFile = filepath.Join(p.ConfigDir, "config.json")
	p.DBFile = filepath.Join(p.DataDir, "smsd.db")
	p.LogFile = filepath.Join(p.StateDir, "log.txt")
	return p, nil
}

// EnsureDirs creates the config, data and state directories if missing.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.ConfigDir, p.DataDir, p.StateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return nil
}

// Load reads the config file, writing a default file if none exists. Missing
// or zero fields are backfilled from Default so upgrades add new keys cleanly.
func Load(p Paths) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(p.ConfigFile)
	if os.IsNotExist(err) {
		// First run: persist defaults so the user has something to edit.
		if err := Save(p, cfg); err != nil {
			return cfg, err
		}
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config: %w", err)
	}
	applyDefaults(&cfg)
	return cfg, nil
}

// Save writes the config as pretty-printed JSON.
func Save(p Paths, cfg Config) error {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.ConfigFile, append(data, '\n'), 0o644)
}

func applyDefaults(cfg *Config) {
	d := Default()
	if cfg.DevicePollSeconds <= 0 {
		cfg.DevicePollSeconds = d.DevicePollSeconds
	}
	if cfg.SMSPollSeconds <= 0 {
		cfg.SMSPollSeconds = d.SMSPollSeconds
	}
	if cfg.ContactsRefreshMinutes <= 0 {
		cfg.ContactsRefreshMinutes = d.ContactsRefreshMinutes
	}
	if cfg.NotifyTimeoutSeconds == 0 {
		cfg.NotifyTimeoutSeconds = d.NotifyTimeoutSeconds
	}
	if cfg.UIAddr == "" {
		cfg.UIAddr = d.UIAddr
	}
	if cfg.LogMaxBytes <= 0 {
		cfg.LogMaxBytes = d.LogMaxBytes
	}
}
