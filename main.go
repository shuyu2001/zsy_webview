package main

import (
	_ "embed"
	"fmt"

	"github.com/wailsapp/go-webview2/pkg/edge"
)

type Action struct {
	Action string `json:"action"`
	URL    string `json:"url"`
}

//go:embed test.html
var html string

func main() {
	initDPIAwareness()

	chromium := edge.NewChromium()

	var host = "http://shuyuz.app/"

	w := NewWithOptions(WebviewOptions{
		Title:           "My App",
		Width:           1280,
		Host:            host,
		Height:          800,
		Debug:           true,
		Center:          true,
		DisableRoute:    false,
		DisableMaximize: false,
		DPIAware:        true,
		StartMaximized:  false,
		Chromium:        chromium,
		AutoFocus:       true,
	})

	if w == nil {
		return
	}

	defer w.Destroy()

	w.AddHtmlContentRoute(host, html)

	w.Navigate(host)

	w.NavigationCompletedCallback(func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		w.Bind("hello", func() {
			fmt.Println("8888")
		})

		chromium.JSONMessageCallback = edge.WrapJSONCallback(func(data Action) {
			fmt.Println("message = ", data)
		})
	})

	w.Run()
}
