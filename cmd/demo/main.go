package main

import (
	"embed"

	"github.com/shuyu2001/zsy_webview"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
)

type Action struct {
	Action string `json:"action"`
	URL    string `json:"url"`
}

//go:embed static/*
var staticFiles embed.FS

func main() {

	chromium := edge.NewChromium()

	var host = "http://shuyuz.app/"

	w := zsy_webview.NewWithOptions(zsy_webview.WebviewOptions{
		Title:           "My App",
		Width:           1280,
		Host:            host,
		Icon:            1,
		Height:          800,
		DebugPort:       true,
		Debug:           true,
		Center:          true,
		DisableRoute:    false,
		DisableMaximize: false,
		StartMaximized:  false,
		Chromium:        chromium,
		AutoFocus:       true,
	})

	if w == nil {
		return
	}

	w.RegisterEmbedFS(staticFiles, "static")

	defer w.Destroy()

	w.Navigate(host)

	w.Run()
}
