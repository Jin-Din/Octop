//go:build !darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// configureTrayMenu gives the Windows/Linux system tray a native context menu
// with a reachable "quit" entry. On these platforms the settings popover is
// opened from the menu rather than from a bare right-click.
//
// macOS keeps its menu-bar behaviour (left click opens the popover) and does
// not use this menu; see tray_menu_darwin.go.
func configureTrayMenu(tray *application.SystemTray, api *App) {
	menu := application.NewMenu()
	menu.Add("显示主界面").OnClick(func(*application.Context) {
		api.ShowMain()
	})
	menu.Add("设置…").OnClick(func(*application.Context) {
		tray.ShowWindow()
	})
	menu.AddSeparator()
	menu.Add("退出 Octop").OnClick(func(*application.Context) {
		api.Quit()
	})
	tray.SetMenu(menu)
	tray.OnRightClick(func() { tray.OpenMenu() })
}
