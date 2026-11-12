// Package agent implements the Android sync daemon and CLI logic.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultPackage  = "com.chucklefish.stardewvalley"
	DefaultSavesDir = "/data/media/0/Android/data/" + DefaultPackage + "/files/Saves"
)

// Config is stored at <dataDir>/config.json.
type Config struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
	SaveID    string `json:"save_id"` // empty = auto-detect when exactly one save exists
	SavesDir  string `json:"saves_dir"`
	Package   string `json:"package"`
	Notify    bool   `json:"notify"`
	// AutoDownload applies newer cloud saves automatically while the game is not running.
	AutoDownload bool `json:"auto_download"`
}

func DefaultConfig() Config {
	return Config{SavesDir: DefaultSavesDir, Package: DefaultPackage, Notify: true, AutoDownload: true}
}

func (c Config) Validate() error {
	if c.ServerURL == "" || c.Token == "" {
		return errors.New("server_url / token 未配置")
	}
	return nil
}

// Redacted returns a copy without the token (for status output).
func (c Config) Redacted() Config {
	if c.Token != "" {
		c.Token = "***"
	}
	return c
}

func ConfigPath(dataDir string) string { return filepath.Join(dataDir, "config.json") }

func LoadConfig(dataDir string) (Config, time.Time, error) {
	c := DefaultConfig()
	p := ConfigPath(dataDir)
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, time.Time{}, nil
	}
	if err != nil {
		return c, time.Time{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, time.Time{}, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, time.Time{}, fmt.Errorf("config.json: %w", err)
	}
	if c.SavesDir == "" {
		c.SavesDir = DefaultSavesDir
	}
	if c.Package == "" {
		c.Package = DefaultPackage
	}
	return c, st.ModTime(), nil
}

func SaveConfig(dataDir string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(ConfigPath(dataDir), b, 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// State is stored at <dataDir>/state.json.
type State struct {
	SaveID       string    `json:"save_id"`
	BaseRevision int       `json:"base_revision"`
	BaseHash     string    `json:"base_hash"`
	LastSync     time.Time `json:"last_sync,omitempty"`
	LastAction   string    `json:"last_action,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Conflict     *Conflict `json:"conflict,omitempty"`
	// PendingRemote is set when a newer cloud revision could not be applied
	// because the game was running or the version is too new.
	PendingRemote int `json:"pending_remote,omitempty"`
}

// Conflict describes a detected conflict awaiting user resolution.
type Conflict struct {
	DetectedAt     time.Time `json:"detected_at"`
	LocalHash      string    `json:"local_hash"`
	LocalGameDate  string    `json:"local_game_date"`
	LocalPlayed    string    `json:"local_played"`
	RemoteRev      int       `json:"remote_rev"`
	RemoteDevice   string    `json:"remote_device"`
	RemoteGameDate string    `json:"remote_game_date"`
	RemotePlayed   string    `json:"remote_played"`
}

func statePath(dataDir string) string { return filepath.Join(dataDir, "state.json") }

func LoadState(dataDir string) (State, error) {
	var s State
	b, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func SaveState(dataDir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(statePath(dataDir), b, 0o600)
}
