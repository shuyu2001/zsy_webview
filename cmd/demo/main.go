package main

import (
	_ "embed"
	"fmt"

	"github.com/shuyu2001/zsy_webview"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
)

type Action struct {
	Action string `json:"action"`
	URL    string `json:"url"`
}

//go:embed test.html
var html string

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

	defer w.Destroy()

	w.AddHtmlContentRoute(host, html)

	w.Navigate("https://www.shuyuz.com")

	w.AddHtmlContentRoute()

	w.NavigationCompletedCallback(func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		w.Bind("hello", func() {
			fmt.Println("8888")
		})

		chromium.JSONMessageCallback = edge.WrapJSONCallback(func(data Action) {
			fmt.Println("message = ", data)
		})
	})

	chromium.AddWebResourceRequestedFilter("https://www.shuyuz.com/ping*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_FETCH)

	w.Run()
}
