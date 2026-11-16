package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type Locale string

const (
	LocaleZH Locale = "zh"
	LocaleEN Locale = "en"
)

// Settings is persisted at ~/.octop/desktop-settings.json
type Settings struct {
	Locale         Locale `json:"locale"`
	Autostart      bool   `json:"autostart"`
	MinimizeToTray bool   `json:"minimizeToTray"`
	PreventSleep   bool   `json:"preventSleep"`
	Port           int    `json:"port,omitempty"`
	// ServerURL optionally points the shell at a remote Octop instance instead
	// of the bundled portable runtime. Empty means local portable mode.
	ServerURL string `json:"serverURL,omitempty"`
}

func defaultSettings() Settings {
	return Settings{
		Locale:         LocaleEN,
		Autostart:      false,
		MinimizeToTray: true,
		PreventSleep:   false,
		Port:           8088,
	}
}

func octopHome() string {
	if v := os.Getenv("OCTOP_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".octop"
	}
	return filepath.Join(home, ".octop")
}

func portableDir() string {
	return filepath.Join(octopHome(), "portable")
}

func settingsPath() string {
	return filepath.Join(octopHome(), "desktop-settings.json")
}

type settingsStore struct {
	mu  sync.Mutex
	cur Settings
}

func loadSettings() Settings {
	s := defaultSettings()
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	var legacy struct {
		PreventSleepMac bool `json:"preventSleepMac"`
	}
	_ = json.Unmarshal(data, &legacy)
	if !s.PreventSleep {
		s.PreventSleep = legacy.PreventSleepMac
	}
	if s.Port == 0 {
		s.Port = 8088
	}
	if s.Locale != LocaleZH {
		s.Locale = LocaleEN
	}
	s.ServerURL = normalizeServerURL(s.ServerURL)
	return s
}

// normalizeServerURL trims whitespace, tolerates a missing scheme by assuming
// http, drops the trailing slash and rejects anything that is not an http(s)
// URL. An empty input means local portable mode and always returns "".
func normalizeServerURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// A bare host:port like "192.168.1.10:8088" fails url.Parse, so prefix http
	// when there is no scheme at all. An explicit scheme (http/https/ftp/…) is
	// kept as-is and validated against the allowlist below.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	return strings.TrimRight(raw, "/")
}

func (st *settingsStore) get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur
}

func (st *settingsStore) save(next Settings) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if next.Port == 0 {
		next.Port = 8088
	}
	next.ServerURL = normalizeServerURL(next.ServerURL)
	if err := os.MkdirAll(octopHome(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath(), data, 0o644); err != nil {
		return err
	}
	st.cur = next
	return nil
}

func greenPlat() string {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	switch osName {
	case "darwin":
		osName = "darwin"
	case "windows":
		osName = "windows"
	default:
		osName = "linux"
	}
	switch arch {
	case "arm64":
		arch = "arm64"
	default:
		arch = "amd64"
	}
	return osName + "-" + arch
}
