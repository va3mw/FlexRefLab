package main

import (
	"embed"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/va3mw/FlexRefLab/internal/logx"
)

//go:embed all:frontend/dist
var assets embed.FS

const version = "0.1.0"

func main() {
	l := logx.New(filepath.Join(appDataDir(), "logs"))
	l.Log(logx.Info, "app", "Flex Reference Lab %s starting", version)

	app := NewApp(l)
	lock := &options.SingleInstanceLock{UniqueId: "va3mw-flexreflab-1"}
	if os.Getenv("FLEXREFLAB_NO_LOCK") != "" {
		lock = nil
	}
	err := wails.Run(&options.App{
		Title:     "Flex Reference Lab " + version,
		Width:     1320,
		Height:    920,
		MinWidth:  1000,
		MinHeight: 660,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour:   &options.RGBA{R: 245, G: 246, B: 248, A: 1},
		OnStartup:          app.startup,
		OnShutdown:         app.shutdown,
		Bind:               []interface{}{app},
		SingleInstanceLock: lock,
		Windows:            &windows.Options{},
	})
	if err != nil {
		l.Log(logx.Error, "app", "wails: %v", err)
	}
}
