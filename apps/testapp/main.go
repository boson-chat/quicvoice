// Command testapp is a desktop test client for quicvoice: pick devices, join
// a room, talk, and watch per-participant levels, loss and latency.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := application.New(application.Options{
		Name:        "quicvoice test",
		Description: "quicvoice test client",
		Services: []application.Service{
			application.NewService(NewVoiceService()),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "quicvoice test",
		Width:     980,
		Height:    680,
		MinWidth:  720,
		MinHeight: 520,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 44,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
