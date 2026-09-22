package main

import (
	_ "embed"
	"fmt"

	"github.com/shuyu2001/zsy_webview"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
)

//go:embed static/index.html
var html string

func main() {
	// 1. 初始化 Chromium 核心
	chromium := edge.NewChromium()

	// 2. 创建主窗口
	wv := zsy_webview.NewWithOptions(zsy_webview.WebviewOptions{
		Title:       "Windows 11 现代磨砂视效演示",
		Width:       1000,
		Height:      650,
		Frameless:   false,
		Transparent: true,
		Center:      true,
		Chromium:    chromium,
		Debug:       true, // 开启右键检查审查元素
	})

	wv.Window.SetTransparentBackground()

	wv.OnClose(func() bool {
		fmt.Println("关闭窗口")
		return true
	})

	// 1. 设置系统磨砂材质（亚克力或云母）
	wv.Window.SetSystemBackdrop(zsy_webview.BackdropMica)

	wv.Window.SetDarkMode(true)

	// 4. 载入带有透明通道的页面
	wv.SetHtml(html)

	// 5. 启动事件循环
	wv.Run()
}
