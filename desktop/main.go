//go:build windows || darwin

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"

	"github.com/HunterLzap/slopguard/internal/bridge"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	panelWidth       = 420
	panelHeight      = 560
	minPanelHeight   = 520
	maxPanelHeight   = 640
	trayOffset       = 8
	dashboardChanged = "slopguard:dashboard-changed"
)

//go:embed assets/slopguard.png
var appIcon []byte

//go:embed assets/tray.png
var trayIcon []byte

//go:embed assets/tray-pending.png
var pendingIcon []byte

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		code := bridge.RunCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, core.ExecRunner{}, nil)
		stop()
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("SlopGuard 启动失败", "error", err)
		os.Exit(1)
	}
}

func run() (err error) {
	ctx := context.Background()
	configDir, err := core.GlobalConfigDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	store, err := reviewer.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	runner := core.ExecRunner{}
	picker := &nativePicker{}
	agents := bridge.AgentSwitch{Home: home, ConfigDir: configDir, Executable: executable}
	service, err := newAppService(store, runner, nil, agents, newAutostart(executable, runner), picker)
	if err != nil {
		return err
	}
	assets, err := panelAssets()
	if err != nil {
		return err
	}
	app := application.New(application.Options{
		Name:     "SlopGuard",
		Icon:     appIcon,
		Services: []application.Service{application.NewService(service)},
		Assets:   application.AssetOptions{Handler: assets},
		Mac:      application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true, WebviewUserDataPath: filepath.Join(configDir, "webview"),
		},
	})
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "panel", Title: "SlopGuard", URL: "/",
		Width: panelWidth, Height: panelHeight, MinWidth: panelWidth, MaxWidth: panelWidth,
		MinHeight: minPanelHeight, MaxHeight: maxPanelHeight,
		Frameless: true, Hidden: true, AlwaysOnTop: true, DisableResize: true, HideOnEscape: true,
		Windows: application.WindowsWindow{HiddenOnTaskbar: true},
	})
	picker.app, picker.window = app, window
	window.OnWindowEvent(events.Common.WindowLostFocus, func(_ *application.WindowEvent) {
		if !picker.open.Load() {
			window.Hide()
		}
	})
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon).SetDarkModeIcon(trayIcon).AttachWindow(window).WindowOffset(trayOffset)
	tray.SetTooltip("SlopGuard")
	window.OnWindowEvent(events.Common.WindowShow, func(_ *application.WindowEvent) {
		app.Event.Emit(dashboardChanged)
	})
	window.OnWindowEvent(events.Common.WindowDidResize, func(_ *application.WindowEvent) {
		if window.IsVisible() {
			if err := tray.PositionWindow(window, trayOffset); err != nil {
				slog.Error("面板定位失败", "error", err)
			}
		}
	})
	menu := app.Menu.New()
	menu.Add("打开面板").OnClick(func(_ *application.Context) { tray.ShowWindow() })
	menu.Add("退出").OnClick(func(_ *application.Context) { app.Quit() })
	tray.SetMenu(menu)
	service.onProposals = func(pending bool) {
		application.InvokeAsync(func() {
			icon := trayIcon
			if pending {
				icon = pendingIcon
			}
			tray.SetIcon(icon).SetDarkModeIcon(icon)
			app.Event.Emit(dashboardChanged)
		})
	}
	return app.Run()
}

type nativePicker struct {
	app    *application.App
	window application.Window
	open   atomic.Bool
}

func (p *nativePicker) PickDirectory(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !p.open.CompareAndSwap(false, true) {
		return "", fmt.Errorf("项目选择窗口已打开")
	}
	defer p.open.Store(false)
	return p.app.Dialog.OpenFile().SetTitle("选择受保护项目").
		CanChooseFiles(false).CanChooseDirectories(true).AttachToWindow(p.window).PromptForSingleSelection()
}
