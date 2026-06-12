package main

import (
	"embed"
	"fmt"

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

	chromium.NavigationStartingCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationStartingEventArgs) {
		var uri, _ = args.GetUri()
		fmt.Println("uri = ", uri)
		if uri == "https://www.shuyuz.com/" {
			fmt.Println("进行拦截 ", uri)
			w.Stop()
		}
	}

	chromium.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		var isSuccess, err = args.GetIsSuccess()
		if isSuccess && err == nil {
			var uri, _ = sender.GetSource()
			fmt.Println("加载完毕 ", uri)
		}
	}

	defer w.Destroy()

	w.AddHotKey(func(w *zsy_webview.Webview) {
		fmt.Println("刷新")
		w.Reload()
	}, "f5")

	w.AddHotKey(func(w *zsy_webview.Webview) {
		w.Window.ShowWindow()
	}, "f6")

	w.AddHotKey(func(w *zsy_webview.Webview) {
		w.Navigate("https://www.shuyuz.com")
	}, "f7")

	w.Navigate(host)

	w.Run()
}
