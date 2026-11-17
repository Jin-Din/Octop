package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed assets/*
var assets embed.FS

const trayDoubleClick = 400 * time.Millisecond

// App is the Wails service bound to the shell UI.
type App struct {
	app            *application.App
	window         *application.WebviewWindow
	settingsWindow *application.WebviewWindow
	tray           *application.SystemTray
	store          *settingsStore
	sleep          *sleepGuard
	cmd            *exec.Cmd
	mu             sync.Mutex
	quitting       bool

	trayClickMu    sync.Mutex
	lastTrayClick  time.Time
	trayClickTimer *time.Timer
	drops          dropServer
	ownsListenPort bool

	// onboarding gates boot() until first-run configuration is finished. The
	// channel is created lazily only for first run and closed once the user
	// completes the onboarding dialog; nil means no gating. The popover itself
	// is presented after the app starts because it is a tray-attached window
	// and can only be shown through the running SystemTray.
	onboardingMu      sync.Mutex
	onboardingDone    chan struct{}
	onboardingPending bool

	// uiReady gates boot() until every WebView2 window has finished embedding.
	// Navigating the main window (showDashboard) while the hidden settings
	// window is still embedding re-enters the native message loop during
	// Chromium init and crashes WebView2 with a nil-pointer panic.
	uiReadyMu    sync.Mutex
	uiReadyTotal int
	uiReadySeen  int
	uiReady      chan struct{}
}

// armUIReady creates the readiness gate for the given number of windows. Each
// window reports via reportWindowReady on its WindowRuntimeReady event; boot()
// blocks until all of them have reported.
func (a *App) armUIReady(windowCount int) {
	a.uiReadyMu.Lock()
	defer a.uiReadyMu.Unlock()
	a.uiReadyTotal = windowCount
	a.uiReadySeen = 0
	a.uiReady = make(chan struct{})
}

func (a *App) reportWindowReady() {
	a.uiReadyMu.Lock()
	a.uiReadySeen++
	ready := a.uiReadySeen >= a.uiReadyTotal
	ch := a.uiReady
	a.uiReadyMu.Unlock()
	if ready && ch != nil {
		select {
		case <-ch:
			// already closed
		default:
			close(ch)
		}
	}
}

// waitUIReady blocks until every window's webview runtime is ready. It returns
// immediately if the gate was never armed.
func (a *App) waitUIReady() {
	a.uiReadyMu.Lock()
	ch := a.uiReady
	a.uiReadyMu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (a *App) ServiceName() string { return "desktop" }

func (a *App) ServiceStartup(context.Context, application.ServiceOptions) error { return nil }

func (a *App) ServiceShutdown() error {
	a.sleep.stop()
	a.mu.Lock()
	cmd := a.cmd
	a.cmd = nil
	ownsPort := a.ownsListenPort
	a.ownsListenPort = false
	a.mu.Unlock()
	stopOctop(cmd)
	if ownsPort {
		clearDesktopPort()
	}
	return nil
}

func (a *App) GetSettings() Settings {
	return a.store.get()
}

func (a *App) GetSettingsStatus() (Settings, error) {
	s := a.store.get()
	if a.app == nil {
		return s, nil
	}
	status, err := a.app.Autostart.Status()
	if err != nil {
		return s, fmt.Errorf("read autostart status: %w", err)
	}
	if s.Autostart != status.Enabled {
		s.Autostart = status.Enabled
		if err := a.store.save(s); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (a *App) SaveSettings(next Settings) (Settings, error) {
	cur, err := a.GetSettingsStatus()
	if err != nil {
		return cur, err
	}
	autostart, err := a.setAutostart(next.Autostart)
	if err != nil {
		return cur, err
	}
	if err := a.sleep.set(next.PreventSleep); err != nil {
		if _, rollbackErr := a.setAutostart(cur.Autostart); rollbackErr != nil {
			log.Printf("rollback autostart after sleep prevention failure: %v", rollbackErr)
		}
		return cur, err
	}
	next.Autostart = autostart
	normalized, err := validateServerURL(next.ServerURL)
	if err != nil {
		if _, rollbackErr := a.setAutostart(cur.Autostart); rollbackErr != nil {
			log.Printf("rollback autostart after invalid server URL: %v", rollbackErr)
		}
		return cur, err
	}
	next.ServerURL = normalized
	if err := a.store.save(next); err != nil {
		return cur, err
	}
	saved := a.store.get()
	a.applyDashboardPrefs(saved)
	return saved, nil
}

func (a *App) ShowMain() {
	a.showWindow()
}

// RuntimeAvailable reports whether this build can serve the local portable
// runtime. Production shells embed it; development/lightweight shells may ship
// a portable zip next to the executable. A lightweight remote-only shell
// returns false, so first-run setup must require a remote address.
func (a *App) RuntimeAvailable() bool {
	if len(embeddedPortable) > 0 {
		return true
	}
	if zipPath, err := bundledPortableZip(); err == nil && zipPath != "" {
		return true
	}
	return false
}

// IsFirstRun reports whether configuration has never been completed.
func (a *App) IsFirstRun() bool {
	return !hasSavedSettings()
}

// beginOnboarding arms the first-run gate. The settings window is created with
// the onboarding URL on first run and preloads while hidden; it announces
// readiness via the desktop:onboarding-ready event, and presentOnboarding()
// then shows it once the system tray is running (a tray-attached window cannot
// be shown before app.Run()).
func (a *App) beginOnboarding() {
	a.onboardingMu.Lock()
	defer a.onboardingMu.Unlock()
	if a.onboardingDone == nil {
		a.onboardingDone = make(chan struct{})
	}
	a.onboardingPending = true
}

// presentOnboarding shows the first-run popover in response to the onboarding
// page's ready event, by which time the system tray is running. It is a no-op if
// onboarding was already finished or never armed.
func (a *App) presentOnboarding() {
	a.onboardingMu.Lock()
	pending := a.onboardingPending
	a.onboardingPending = false
	a.onboardingMu.Unlock()
	if !pending || a.tray == nil {
		return
	}
	a.tray.ShowWindow()
}

func (a *App) onboardingChannel() chan struct{} {
	a.onboardingMu.Lock()
	defer a.onboardingMu.Unlock()
	return a.onboardingDone
}

// inOnboarding reports whether the first-run gate is currently active. During
// onboarding the settings popover must stay visible and cannot be dismissed.
func (a *App) inOnboarding() bool {
	a.onboardingMu.Lock()
	defer a.onboardingMu.Unlock()
	return a.onboardingDone != nil
}

// CompleteOnboarding is called by the UI after a successful first-run save and
// releases the boot gate.
func (a *App) CompleteOnboarding() {
	a.onboardingMu.Lock()
	ch := a.onboardingDone
	if ch != nil {
		close(ch)
		a.onboardingDone = nil
	}
	a.onboardingPending = false
	a.onboardingMu.Unlock()
	if a.tray != nil {
		a.tray.HideWindow()
	} else if a.settingsWindow != nil {
		a.settingsWindow.Hide()
	}
	a.showWindow()
}

// ShowSettings opens the settings popover in normal mode (e.g. from a failed
// remote-connection error screen).
func (a *App) ShowSettings() {
	a.openSettings(false)
}

// openSettings points the tray-attached popover at the right mode and presents
// it via the running SystemTray. During onboarding the URL carries onboarding=1
// so the window renders the first-run layout.
func (a *App) openSettings(onboarding bool) {
	if a.settingsWindow == nil || a.tray == nil {
		return
	}
	target := "/?settings=1"
	if onboarding {
		target = "/?settings=1&onboarding=1"
	}
	a.settingsWindow.SetURL(target)
	a.tray.ShowWindow()
}

func (a *App) HideSettings() {
	if a.settingsWindow == nil {
		return
	}
	// During first-run onboarding the popover is modal: hiding it would leave
	// boot() blocked forever. The UI routes the close button to Quit instead.
	if a.inOnboarding() {
		return
	}
	if a.tray != nil {
		a.tray.HideWindow()
	} else {
		a.settingsWindow.Hide()
	}
}

func (a *App) Quit() {
	a.requestQuit()
}

func (a *App) setAutostart(on bool) (bool, error) {
	if a.app == nil {
		return false, fmt.Errorf("autostart is unavailable before the application starts")
	}
	if on {
		if err := a.app.Autostart.Enable(); err != nil {
			return false, fmt.Errorf("enable autostart: %w", err)
		}
	} else if err := a.app.Autostart.Disable(); err != nil {
		return false, fmt.Errorf("disable autostart: %w", err)
	}
	status, err := a.app.Autostart.Status()
	if err != nil {
		return false, fmt.Errorf("read autostart status: %w", err)
	}
	if status.Enabled != on {
		return status.Enabled, fmt.Errorf("autostart state did not update")
	}
	return status.Enabled, nil
}

func (a *App) applyDashboardPrefs(s Settings) {
	if a.window == nil {
		return
	}
	js := fmt.Sprintf(
		`(function(){try{localStorage.setItem('octop:ui-locale',%s);}catch(e){}})();`,
		jsonString(string(s.Locale)),
	)
	a.window.ExecJS(js)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (a *App) setStatus(msg string) {
	if a.app == nil {
		return
	}
	a.app.Event.Emit("desktop:status", msg)
}

// emitBootFatal reports a remote-connection failure without falling back to the
// local runtime. The UI shows a retry/settings screen instead of silently
// starting a portable instance (which would re-trigger the admin wizard).
func (a *App) emitBootFatal(locale Locale, url string, cause error) {
	if a.app == nil {
		return
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	a.app.Event.Emit("desktop:boot-fatal", map[string]string{
		"title":  desktopText(locale, copyFatalRemoteTitle),
		"hint":   desktopText(locale, copyFatalRemoteHint),
		"url":    url,
		"detail": detail,
	})
	// The fatal screen renders inside the main window, so make sure that window
	// is visible. After a relaunch the shell can start hidden, and without this
	// the user would see no UI at all while boot is blocked on the dead remote.
	a.showWindow()
}

// resolveRemoteURL picks the remote Octop address to connect to. Precedence:
// the persisted settings ServerURL, then the OCTOP_DESKTOP_URL env var. An empty
// result means the shell should fall back to the bundled portable runtime.
func (a *App) resolveRemoteURL() string {
	if a.store != nil {
		if url := normalizeServerURL(a.store.get().ServerURL); url != "" {
			return url
		}
	}
	return strings.TrimSpace(os.Getenv("OCTOP_DESKTOP_URL"))
}

func (a *App) boot() {
	// Block until every WebView2 window is fully embedded before performing any
	// navigation, so showDashboard cannot race the hidden settings window's
	// Chromium initialization and crash the webview.
	a.waitUIReady()
	locale := LocaleEN
	if a.store != nil {
		locale = a.store.get().Locale
	}
	// First run: block until the user finishes the onboarding dialog. The gate
	// itself is armed synchronously in main() before app.Run() so the
	// ApplicationStarted handler can never observe an unset pending flag. The
	// settings file only exists after a successful save, so this never gates
	// existing installs.
	if ch := a.onboardingChannel(); ch != nil {
		<-ch
		// Re-read locale/URL chosen during onboarding.
		if a.store != nil {
			locale = a.store.get().Locale
		}
	}
	if url := a.resolveRemoteURL(); url != "" {
		a.setStatus(desktopText(locale, copyStatusConnecting))
		if err := waitHealth(locale, url, 60*time.Second); err != nil {
			a.emitBootFatal(locale, url, err)
			return
		}
		a.showDashboard(url)
		return
	}
	a.setStatus(desktopText(locale, copyStatusCheckingRuntime))
	if err := ensurePortable(locale, a.setStatus); err != nil {
		a.setStatus(err.Error())
		return
	}
	root := portableDir()
	a.mu.Lock()
	stopOctop(a.cmd)
	a.cmd = nil
	a.mu.Unlock()
	host, preferred := resolveListen()
	if existing := findRunningDashboard(host, preferred); existing != "" {
		a.setStatus(desktopText(locale, copyStatusUsingExisting))
		if err := waitHealth(locale, existing, 30*time.Second); err != nil {
			a.setStatus(err.Error())
			return
		}
		a.showDashboard(existing)
		return
	}
	port, err := chooseFreePort(host, preferred)
	if err != nil {
		var busy noFreePortError
		var bind cannotBindError
		switch {
		case errors.As(err, &busy):
			a.setStatus(desktopText(locale, copyErrorNoFreePort, busy.From, busy.To))
		case errors.As(err, &bind):
			a.setStatus(desktopText(locale, copyErrorCannotBind, bind.Host, bind.Port, bind.Err.Error()))
		default:
			a.setStatus(err.Error())
		}
		return
	}
	a.mu.Lock()
	cmd, err := startOctop(root, port)
	a.cmd = cmd
	a.mu.Unlock()
	if err != nil {
		a.setStatus(err.Error())
		return
	}
	base := dashboardURL(host, port)
	a.setStatus(desktopText(locale, copyStatusStartingService))
	if err := waitHealth(locale, base, 2*time.Minute); err != nil {
		a.setStatus(err.Error())
		return
	}
	if err := writeDesktopPort(port); err != nil {
		log.Printf("desktop port: %v", err)
	} else {
		a.mu.Lock()
		a.ownsListenPort = true
		a.mu.Unlock()
	}
	a.showDashboard(base)
}

func (a *App) showDashboard(base string) {
	if a.window == nil {
		return
	}
	a.window.SetURL(base)
	a.scheduleDragOverlay()
	s := a.store.get()
	go func() {
		time.Sleep(800 * time.Millisecond)
		a.applyDashboardPrefs(s)
	}()
	a.setStatus(desktopText(s.Locale, copyStatusReady))
}

func (a *App) hideToTray() {
	if a.window == nil {
		return
	}
	a.window.Hide()
}

func (a *App) showWindow() {
	if a.window == nil {
		return
	}
	if a.window.IsMinimised() {
		a.window.UnMinimise()
	}
	a.window.Show()
	a.window.Focus()
}

func (a *App) toggleMainWindow() {
	if a.window == nil {
		return
	}
	if a.window.IsVisible() && !a.window.IsMinimised() {
		a.hideToTray()
		return
	}
	a.showWindow()
}

func (a *App) onTrayLeftClick() {
	a.trayClickMu.Lock()
	defer a.trayClickMu.Unlock()
	if a.trayClickTimer != nil {
		a.trayClickTimer.Stop()
		a.trayClickTimer = nil
	}
	now := time.Now()
	if !a.lastTrayClick.IsZero() && now.Sub(a.lastTrayClick) < trayDoubleClick {
		a.lastTrayClick = time.Time{}
		go a.toggleMainWindow()
		return
	}
	a.lastTrayClick = now
	a.trayClickTimer = time.AfterFunc(trayDoubleClick, func() {
		a.trayClickMu.Lock()
		a.trayClickTimer = nil
		a.trayClickMu.Unlock()
		a.showWindow()
	})
}

func (a *App) installDragOverlay() {
	if a.window == nil {
		return
	}
	a.window.ExecJS(dragOverlayJS())
	a.window.ExecJS(fileDropJS())
}

func (a *App) scheduleDragOverlay() {
	go func() {
		for range 40 {
			time.Sleep(250 * time.Millisecond)
			a.installDragOverlay()
			a.installExternalLinks()
		}
	}()
}

func (a *App) requestQuit() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	if a.app != nil {
		a.app.Quit()
	}
}

func main() {
	store := &settingsStore{cur: loadSettings()}
	api := &App{
		store: store,
		sleep: &sleepGuard{},
	}

	app := application.New(application.Options{
		Name:        "Octop",
		Description: "Octop desktop",
		Services: []application.Service{
			application.NewService(api),
		},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})
	api.app = app
	attachOpenURLEventListener(app, api.OpenExternal)
	attachFileDropEventListener(app, api)
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(_ *application.ApplicationEvent) {
		applyAppIcon(app)
	})
	// The onboarding page preloads in the hidden settings window and announces
	// itself when ready. By then the system tray is running, so the
	// tray-attached popover can be shown reliably (calling ShowWindow during
	// ApplicationStarted races the native tray startup).
	app.Event.On("desktop:onboarding-ready", func(_ *application.CustomEvent) {
		api.presentOnboarding()
	})

	// Wait for both webviews to finish embedding before boot() navigates, to
	// avoid re-entering the native message loop during Chromium initialization.
	api.armUIReady(2)

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:                "Octop",
		Width:                1200,
		Height:               800,
		URL:                  "/",
		Frameless:            true,
		AllowSimpleEventEmit: true,
		EnableFileDrop:       true,
		BackgroundColour:     application.NewRGB(247, 248, 250),
	})
	api.window = win
	win.OnWindowEvent(events.Common.WindowRuntimeReady, func(_ *application.WindowEvent) {
		api.reportWindowReady()
	})
	app.Event.On("desktop:toggle-maximise", func(_ *application.CustomEvent) {
		win.ToggleMaximise()
	})
	app.Event.On("desktop:minimise", func(_ *application.CustomEvent) {
		win.Minimise()
	})
	app.Event.On("desktop:close", func(_ *application.CustomEvent) {
		api.hideToTray()
	})
	installDragOverlay := func(_ *application.WindowEvent) { api.scheduleDragOverlay() }
	win.OnWindowEvent(events.Mac.WebViewDidFinishNavigation, installDragOverlay)
	win.OnWindowEvent(events.Windows.WebViewNavigationCompleted, installDragOverlay)
	win.OnWindowEvent(events.Linux.WindowLoadFinished, installDragOverlay)
	// On first run the hidden popover preloads the onboarding page so it can
	// announce readiness; existing installs keep the normal settings page.
	settingsInitialURL := "/?settings=1"
	if api.IsFirstRun() {
		settingsInitialURL = "/?settings=1&onboarding=1"
	}
	settingsWin := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Octop 设置",
		Width:            settingsWindowWidth,
		Height:           settingsWindowOuterHeight(),
		URL:              settingsInitialURL,
		Hidden:           true,
		Frameless:        true,
		AlwaysOnTop:      true,
		DisableResize:    true,
		BackgroundColour: application.NewRGB(255, 255, 255),
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
		},
	})
	api.settingsWindow = settingsWin
	settingsWin.OnWindowEvent(events.Common.WindowRuntimeReady, func(_ *application.WindowEvent) {
		api.reportWindowReady()
	})
	app.Event.RegisterApplicationEventHook(events.Mac.ApplicationShouldHandleReopen, func(event *application.ApplicationEvent) {
		event.Cancel()
		restoreMainAfterDockClick(api.window, api.settingsWindow)
	})

	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		api.mu.Lock()
		quit := api.quitting
		api.mu.Unlock()
		if quit {
			return
		}
		e.Cancel()
		api.hideToTray()
	})
	win.OnWindowEvent(events.Common.WindowMinimise, func(_ *application.WindowEvent) {
		if api.store.get().MinimizeToTray {
			api.hideToTray()
		}
	})
	settingsWin.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		if !api.inOnboarding() {
			settingsWin.Hide()
		}
	})
	settingsWin.OnWindowEvent(events.Common.WindowLostFocus, func(_ *application.WindowEvent) {
		if !api.inOnboarding() {
			settingsWin.Hide()
		}
	})

	tray := app.SystemTray.New()
	api.tray = tray
	applyTrayIcon(tray)
	tray.SetTooltip("Octop")
	tray.AttachWindow(settingsWin).WindowOffset(6)
	showSettings := func() { tray.ShowWindow() }
	if trayLeftClickShowsSettings(runtime.GOOS) {
		tray.OnClick(showSettings)
		tray.OnRightClick(showSettings)
	} else {
		tray.OnClick(func() { api.onTrayLeftClick() })
		configureTrayMenu(tray, api)
	}

	if _, err := api.setAutostart(store.get().Autostart); err != nil {
		log.Printf("sync autostart: %v", err)
	}
	if err := api.sleep.set(store.get().PreventSleep); err != nil {
		log.Printf("enable sleep prevention: %v", err)
	}

	api.scheduleDragOverlay()
	// Arm the first-run gate synchronously before Run() so boot() blocks on a
	// ready channel and the ApplicationStarted handler always sees the pending
	// popover. Existing installs have a settings file and are not gated.
	if api.IsFirstRun() {
		api.beginOnboarding()
	}
	go api.boot()

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
