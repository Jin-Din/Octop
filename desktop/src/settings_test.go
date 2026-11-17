package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSettingsMigratesLegacyPreventSleepMac(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("OCTOP_HOME", temp)
	if err := os.WriteFile(
		filepath.Join(temp, "desktop-settings.json"),
		[]byte(`{"preventSleepMac":true}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	if !loadSettings().PreventSleep {
		t.Fatal("legacy preventSleepMac should migrate to preventSleep")
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays local", "", ""},
		{"whitespace stays local", "   ", ""},
		{"trims surrounding spaces", "  http://192.168.1.10:8088/  ", "http://192.168.1.10:8088"},
		{"drops trailing slash", "https://host.example/octop/", "https://host.example/octop"},
		{"assumes http when scheme missing", "192.168.1.10:8088", "http://192.168.1.10:8088"},
		{"keeps https", "https://10.0.0.5:9000", "https://10.0.0.5:9000"},
		{"rejects non-http scheme", "ftp://host/file", ""},
		{"rejects hostless input", "http://", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeServerURL(tc.in); got != tc.want {
				t.Fatalf("normalizeServerURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSettingsPersistsServerURL(t *testing.T) {
	t.Setenv("OCTOP_HOME", t.TempDir())
	store := &settingsStore{cur: defaultSettings()}

	if err := store.save(Settings{
		Locale:    LocaleZH,
		Port:      8088,
		ServerURL: "  http://192.168.1.10:8088/ ",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	saved := store.get()
	if saved.ServerURL != "http://192.168.1.10:8088" {
		t.Fatalf("saved ServerURL = %q, want normalized", saved.ServerURL)
	}

	loaded := loadSettings()
	if loaded.ServerURL != "http://192.168.1.10:8088" {
		t.Fatalf("loaded ServerURL = %q, want normalized value", loaded.ServerURL)
	}

	// Clearing the field must persist local portable mode.
	if err := store.save(Settings{Locale: LocaleZH, Port: 8088, ServerURL: "   "}); err != nil {
		t.Fatalf("save cleared: %v", err)
	}
	if store.get().ServerURL != "" {
		t.Fatalf("cleared ServerURL = %q, want empty", store.get().ServerURL)
	}
	if loadSettings().ServerURL != "" {
		t.Fatal("expected local portable mode after clearing ServerURL")
	}
}

func TestValidateServerURL(t *testing.T) {
	t.Run("valid and normalised", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{"", ""},
			{"   ", ""},
			{"  http://192.168.1.10:8088/  ", "http://192.168.1.10:8088"},
			{"192.168.1.10:8088", "http://192.168.1.10:8088"},
			{"https://host.example/octop/", "https://host.example/octop"},
		}
		for _, tc := range cases {
			got, err := validateServerURL(tc.in)
			if err != nil {
				t.Fatalf("validateServerURL(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("validateServerURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("invalid returns sentinel and empty value", func(t *testing.T) {
		for _, in := range []string{
			"ftp://host/file",
			"http://",
			"not a url :::",
			"//no-scheme-host",
		} {
			got, err := validateServerURL(in)
			if !errors.Is(err, ErrInvalidServerURL) {
				t.Fatalf("validateServerURL(%q) error = %v, want ErrInvalidServerURL", in, err)
			}
			if got != "" {
				t.Fatalf("validateServerURL(%q) = %q, want empty value on error", in, got)
			}
		}
	})
}

func TestHasSavedSettings(t *testing.T) {
	t.Run("first run without settings file", func(t *testing.T) {
		t.Setenv("OCTOP_HOME", t.TempDir())
		if hasSavedSettings() {
			t.Fatal("expected first run when desktop-settings.json is absent")
		}
	})

	t.Run("configured after settings file exists", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("OCTOP_HOME", home)
		if err := os.WriteFile(
			filepath.Join(home, "desktop-settings.json"),
			[]byte(`{}`),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		if !hasSavedSettings() {
			t.Fatal("expected configured once desktop-settings.json exists")
		}
	})
}
