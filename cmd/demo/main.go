package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"log"
	"text/template"

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
		DisableMaximize: true,
		StartMaximized:  false,
		Chromium:        chromium,
		AutoFocus:       true,
	})

	if w == nil {
		return
	}

	defer w.Destroy()

	var buf bytes.Buffer

	var data = map[string]string{"Title": "认证授权", "SubTitle": "这是测试认证"}
	tmpl, _ := template.New("ui").Parse(html)

	if err := tmpl.Execute(&buf, data); err != nil {
		log.Fatal(err)
	}

	chromium.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		var uri, _ = sender.GetSource()
		if uri == "about:blank" || uri == "" {
			return
		}
		fmt.Println(uri)
	}

	w.SetSizeWithHint(420, 500, zsy_webview.HintMinimize)
	w.Window.DisableMaximizeButton()
	w.SetHtml(buf.String())
	w.Eval(fmt.Sprintf(`setStatus(%q,"#ef4444")`, "请重新登录"))

	w.Run()
}
