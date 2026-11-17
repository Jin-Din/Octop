//go:build darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// configureTrayMenu is intentionally a no-op on macOS. The menu-bar icon keeps
// its existing behaviour: a left click opens the settings popover.
func configureTrayMenu(_ *application.SystemTray, _ *App) {}
