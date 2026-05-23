package zsy_webview

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/shuyu2001/zsy_webview/internal/w32"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
	"golang.org/x/sys/windows"
)

const (
	LOGPIXELSX = 88
	LOGPIXELSY = 90

	// 键盘消息范围，用于过滤无谓的 IsDialogMessage 调用
	wmKeyFirst = 0x0100
	wmKeyLast  = 0x0109
)

func init() {
	aware := -4
	w32.SetProcessDpiAwarenessContext.Call(uintptr(aware))
}

func getDPIScale() (float64, float64) {
	hdc, _, _ := w32.GetDC.Call(0)
	if hdc == 0 {
		return 1.0, 1.0
	}
	defer w32.ReleaseDC.Call(0, hdc)

	dpiX, _, _ := w32.GetDeviceCaps.Call(hdc, LOGPIXELSX)
	dpiY, _, _ := w32.GetDeviceCaps.Call(hdc, LOGPIXELSY)

	if dpiX == 0 || dpiY == 0 {
		return 1.0, 1.0
	}

	return float64(dpiX) / 96.0, float64(dpiY) / 96.0
}

type WebviewOptions struct {
	Title     string
	Width     int
	Height    int
	Host      string
	DebugPort bool
	Icon      uintptr

	Center         bool
	StartMaximized bool
	StartMinimized bool

	Frameless     bool
	HideInTaskbar bool
	AlwaysOnTop   bool

	DisableResize   bool
	DisableMaximize bool
	DisableRoute    bool
	AutoFocus       bool

	Chromium *edge.Chromium
	Debug    bool
}

type route struct {
	content []byte // 优化: 提前转换为 []byte 避免高频请求时的 GC 内存分配
	headers string
	path    string
}

type window struct {
	hwnd uintptr
}

func (w *webview) setIcon(id uintptr) {
	hInstance, _, _ := w32.GetModuleHandleW.Call(0)

	hIcon, _, _ := w32.User32LoadImageW.Call(
		hInstance,
		id,
		1,
		0,
		0,
		0x40,
	)

	w32.User32SendMessageW.Call(w.hwnd, 0x0080, 0, hIcon)
	w32.User32SendMessageW.Call(w.hwnd, 0x0080, 1, hIcon)
}

func (w *window) Maximize() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_MAXIMIZE))
}

func (w *window) MinimizeWindow(hwnd uintptr) {
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.WS_MINIMIZEBOX))
}

func (w *window) RestoreWindow(hwnd uintptr) {
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.SW_RESTORE))
}

func (w *window) HideWindow(hwnd uintptr) {
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.SW_HIDE))
}

func (w *window) ShowWindow(hwnd uintptr) {
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.SW_SHOW))
}

func (w *window) CloseWindow(hwnd uintptr) {
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.WM_CLOSE), 0, 0)
}

func (w *window) DisableMaximizeButton() {
	style := w32.GetWindowLong(w.hwnd, w32.GWLStyle)
	style &^= w32.WS_MAXIMIZEBOX
	w32.SetWindowLong(w.hwnd, w32.GWLStyle, style)
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
}

func (w *window) EnableMaximizeButton() {
	style := w32.GetWindowLong(w.hwnd, w32.GWLStyle)
	style |= w32.WS_MAXIMIZEBOX
	w32.SetWindowLong(w.hwnd, w32.GWLStyle, style)
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
}

func newWindow(hwnd uintptr) *window {
	return &window{hwnd: hwnd}
}

type Webview struct {
	Window *window
	wv     *webview
}

type webview struct {
	hwnd       uintptr
	mainthread uintptr

	Window *window

	debugPort int

	routes map[string]route
	host   string
	route  bool
	center bool

	dpix float64
	dpiy float64

	browser   *edge.Chromium
	autofocus bool

	dpi bool

	maxsz w32.Point
	minsz w32.Point

	mu        sync.Mutex
	bindings  map[string]interface{}
	dispatchq []func()

	destroyed    int32
	browserReady int32
}

var windowContext sync.Map

func getWindowContext(wnd uintptr) *webview {
	if v, ok := windowContext.Load(wnd); ok {
		return v.(*webview)
	}
	return nil
}

func setWindowContext(wnd uintptr, data *webview) {
	windowContext.Store(wnd, data)
}

func deleteWindowContext(wnd uintptr) {
	windowContext.Delete(wnd)
}

func (w *Webview) Dispatch(fn func()) {
	w.wv.mu.Lock()
	w.wv.dispatchq = append(w.wv.dispatchq, fn)
	w.wv.mu.Unlock()
	w32.User32PostMessageW.Call(w.wv.hwnd, w32.WMApp, 0, 0)
}

func (w *Webview) Navigate(url string) {
	w.wv.browser.Navigate(url)
}

func (w *Webview) AddRoute(path string, content string, headers string) {
	// 提前转为 []byte 缓存，在响应路由请求时实现真正零分配
	w.wv.routes[path] = route{path: path, content: []byte(content), headers: headers}
}

func (w *Webview) AddHtmlContentRoute(path string, content string) {
	w.AddRoute(path, content, "Content-Type: text/html; charset=utf-8")
}

func (w *Webview) Destroy() {
	w32.User32PostMessageW.Call(w.wv.hwnd, w32.WM_CLOSE, 0, 0)
}

func (w *Webview) SetHtml(html string) {
	w.wv.browser.NavigateToString(html)
}

func (w *Webview) Init(js string) {
	w.wv.browser.Init(js)
}

func (w *Webview) Eval(js string) {
	w.wv.browser.Eval(js)
}

func (w *Webview) NavigationCompletedCallback(fn func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs)) {
	w.wv.browser.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
		fn(sender, args)
	}
}

func jsString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func safeFocus(browser *edge.Chromium) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("webview: Focus() recovered from panic: %v", r)
		}
	}()
	browser.Focus()
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	// 优化: 安全防护。CGO回调出现Panic会导致系统崩溃或异常退出
	defer func() {
		if r := recover(); r != nil {
			log.Printf("webview: wndproc panic: %v", r)
		}
	}()

	w := getWindowContext(hwnd)
	if w == nil {
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

	browserOK := atomic.LoadInt32(&w.browserReady) == 1
	isDestroyed := atomic.LoadInt32(&w.destroyed) == 1

	if !browserOK || isDestroyed {
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

	// 优化: 将统一拦截的 DefWindowProcW 放到函数尾部，精简代码消除反复调用带来的开销
	switch msg {
	case w32.WMMove, w32.WMMoving:
		if msg == w32.WMMove {
			_ = w.browser.NotifyParentWindowPositionChanged()
		}

	case w32.WMNCLButtonDown:
		w32.User32SetFocus.Call(w.hwnd)

	case w32.WMSize:
		if wp != 1 {
			w.browser.Resize()
		}

	case w32.WMActivate:
		isMinimized := (wp >> 16) != 0
		isInactive := (wp & 0xffff) == 0

		if !isInactive && !isMinimized && w.autofocus {
			safeFocus(w.browser)
		}

	case w32.WM_CLOSE:
		w32.User32DestroyWindow.Call(hwnd)
		return 0

	case w32.WMDestroy:
		atomic.StoreInt32(&w.destroyed, 1)
		deleteWindowContext(hwnd)
		w32.User32PostQuitMessage.Call(0)
		return 0

	case w32.WMGetMinMaxInfo:
		lpmmi := (*w32.MinMaxInfo)(unsafe.Pointer(lp))
		hasChanged := false
		if w.maxsz.X > 0 && w.maxsz.Y > 0 {
			lpmmi.PtMaxSize = w.maxsz
			lpmmi.PtMaxTrackSize = w.maxsz
			hasChanged = true
		}
		if w.minsz.X > 0 && w.minsz.Y > 0 {
			lpmmi.PtMinTrackSize = w.minsz
			hasChanged = true
		}
		if hasChanged {
			return 0
		}
	}

	r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (w *webview) createWindow(opts WebviewOptions) bool {
	var hinstance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &hinstance); err != nil {
		log.Printf("GetModuleHandleEx failed: %v", err)
		return false
	}

	icow, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCxIcon)
	icoh, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCyIcon)
	icon, _, _ := w32.User32LoadImageW.Call(0, 32512, icow, icoh, 0x00008000)

	className, err := windows.UTF16PtrFromString("webview_class")
	if err != nil {
		log.Printf("UTF16PtrFromString(className) failed: %v", err)
		return false
	}

	wc := w32.WndClassExW{
		CbSize:        uint32(unsafe.Sizeof(w32.WndClassExW{})),
		HInstance:     hinstance,
		LpszClassName: className,
		HIcon:         windows.Handle(icon),
		HIconSm:       windows.Handle(icon),
		LpfnWndProc:   windows.NewCallback(wndproc),
		HbrBackground: w32.COLOR_WINDOW + 1,
		Style:         0x0003, // CS_HREDRAW | CS_VREDRAW
	}

	if ret, _, _ := w32.User32RegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		log.Printf("RegisterClassExW failed (class may already exist)")
	}

	windowWidth := opts.Width
	if windowWidth <= 0 {
		windowWidth = 800
	}
	windowHeight := opts.Height
	if windowHeight <= 0 {
		windowHeight = 600
	}

	windowWidth = int(float64(windowWidth) * w.dpix)
	windowHeight = int(float64(windowHeight) * w.dpiy)

	var exStyle uint32
	var style uint32 = w32.WS_OVERLAPPEDWINDOW

	if opts.Frameless {
		style = w32.WS_POPUP | w32.WS_CLIPCHILDREN | w32.WS_CLIPSIBLINGS
	} else {
		if opts.DisableResize {
			style &^= w32.WS_THICKFRAME
			style &^= w32.WS_MAXIMIZEBOX
		} else {
			style |= w32.WS_THICKFRAME
		}

		if opts.DisableMaximize {
			style &^= w32.WS_MAXIMIZEBOX
		} else if !opts.DisableResize {
			style |= w32.WS_MAXIMIZEBOX
		}
	}

	if opts.HideInTaskbar {
		exStyle |= 0x00000080
	} else {
		exStyle |= 0x00040000
	}

	posX, posY := int(w32.CW_USEDEFAULT), int(w32.CW_USEDEFAULT)
	if opts.Center {
		screenW, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CXSCREEN)
		screenH, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CYSCREEN)
		posX = (int(screenW) - windowWidth) / 2
		posY = (int(screenH) - windowHeight) / 2
		if posX < 0 {
			posX = 0
		}
		if posY < 0 {
			posY = 0
		}
	}

	windowName, err := windows.UTF16PtrFromString(opts.Title)
	if err != nil {
		log.Printf("UTF16PtrFromString(title) failed: %v", err)
		return false
	}

	w.hwnd, _, _ = w32.User32CreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		uintptr(style),
		uintptr(posX), uintptr(posY),
		uintptr(windowWidth), uintptr(windowHeight),
		0, 0, uintptr(hinstance), 0,
	)
	if w.hwnd == 0 {
		log.Println("CreateWindowExW failed: hwnd is 0")
		return false
	}

	setWindowContext(w.hwnd, w)

	if opts.AlwaysOnTop {
		w32.User32SetWindowPos.Call(
			w.hwnd, w32.HWND_TOPMOST,
			0, 0, 0, 0,
			w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOACTIVATE,
		)
	}

	showMode := uintptr(w32.SW_SHOW)
	switch {
	case opts.StartMaximized:
		showMode = w32.SW_MAXIMIZE
	case opts.StartMinimized:
		showMode = w32.SW_SHOWMINIMIZED
	}

	w32.User32ShowWindow.Call(w.hwnd, showMode)
	w32.User32UpdateWindow.Call(w.hwnd)
	w32.User32SetFocus.Call(w.hwnd)

	return true
}

func (w *Webview) SetSize(width, height int) {
	width = int(float64(width) * w.wv.dpix)
	height = int(float64(height) * w.wv.dpiy)

	screenW, _, _ := w32.User32GetSystemMetrics.Call(0)
	screenH, _, _ := w32.User32GetSystemMetrics.Call(1)

	x := (int(screenW) - width) / 2
	y := (int(screenH) - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	flags := w32.SWP_NOZOrder
	if !w.wv.center {
		flags |= w32.SWP_NOMOVE
	}

	w32.User32SetWindowPos.Call(
		w.wv.hwnd, 0,
		uintptr(x), uintptr(y),
		uintptr(width), uintptr(height),
		uintptr(flags),
	)

	w.wv.browser.Resize()
}

// safeExecute 为派遣的任务提供恐慌捕获，防止单点业务错误把整个UI线程崩掉
func safeExecute(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("webview: Dispatch task panic: %v", r)
		}
	}()
	fn()
}

func (w *Webview) Run() {
	var msg w32.Msg

	localQ := make([]func(), 0, 16)

	for {
		ret, _, _ := w32.User32GetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)), 0, 0, 0,
		)
		switch int32(ret) {
		case -1:
			log.Println("GetMessageW error")
			return
		case 0:
			return
		}

		if msg.Message == w32.WMApp {
			w.wv.mu.Lock()
			localQ = append(localQ[:0], w.wv.dispatchq...)

			// 优化: 主动置 nil 释放闭包引用，防止极易被忽略的切片内存泄漏 (Memory Leak)
			for i := range w.wv.dispatchq {
				w.wv.dispatchq[i] = nil
			}
			w.wv.dispatchq = w.wv.dispatchq[:0]
			w.wv.mu.Unlock()

			for _, fn := range localQ {
				safeExecute(fn)
			}
			continue
		}

		// 优化: 【决定流畅度的关键】
		// IsDialogMessage 只应对键盘事件进行拦截转换，原本对于鼠标高频移动 (MouseMove)
		// 或绘图 (Paint) 同样调用此 CGO，会大幅度削弱流畅度。这里增加键盘消息区间的判断。
		if msg.Message >= wmKeyFirst && msg.Message <= wmKeyLast {
			ancestor, _, _ := w32.User32GetAncestor.Call(uintptr(msg.Hwnd), w32.GARoot)
			isDialog, _, _ := w32.User32IsDialogMessage.Call(ancestor, uintptr(unsafe.Pointer(&msg)))
			if isDialog != 0 {
				continue
			}
		}

		w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *Webview) PostWebMessageAsJSON(data interface{}) error {
	var buff, _ = json.Marshal(&data)
	return w.wv.browser.PostWebMessageAsJson(string(buff))
}

func (w *Webview) PostWebMessageAsString(str string) error {
	return w.wv.browser.PostWebMessageAsString(str)
}

type rpcMessage struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (w *Webview) Emit(eventName string, data interface{}) {
	var buff, _ = json.Marshal(&data)
	var js = fmt.Sprintf(`window.dispatchEvent(new CustomEvent('%s', { detail: %s }));`,
		eventName, string(buff))

	w.Eval(js)
}

func (w *Webview) GetURL() string {
	var url, err = w.wv.browser.GetCurrentURL()
	if err != nil {
		return ""
	}
	return url
}

func (w *Webview) GetDebugPort() int {
	return w.wv.debugPort
}

func NewWithOptions(opts WebviewOptions) *Webview {
	if opts.Chromium == nil {
		log.Fatal("Chromium instance must be provided via WebviewOptions.Chromium")
		return nil
	}

	if opts.Host == "" {
		opts.Host = "http://shuyuz.app/"
	}

	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)

	var dpiX, dpiY = getDPIScale()

	w := &webview{
		bindings:  make(map[string]interface{}),
		routes:    make(map[string]route),
		host:      opts.Host,
		dpix:      dpiX,
		dpiy:      dpiY,
		center:    opts.Center,
		route:     !opts.DisableRoute,
		autofocus: opts.AutoFocus,
		browser:   opts.Chromium,
	}

	if opts.DebugPort {
		var safePort int
		for {
			p := FastRandPort9000() // 假设其它包下提供了这个方法
			if !IsPortUsed(p) {
				safePort = p
				break
			}
		}
		args := fmt.Sprintf("--remote-debugging-port=%d --remote-debugging-address=127.0.0.1", safePort)
		os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", args)
		w.debugPort = safePort
	}

	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()

	if !w.createWindow(opts) {
		return nil
	}

	var wv = Webview{wv: w, Window: newWindow(w.hwnd)}

	w.setIcon(opts.Icon)

	// 注意：此处的 msgcb 假定您在工程内的其他文件对其做了定义
	w.browser.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		wv.msgcb(message)
	}

	w.browser.SetPermission(
		edge.CoreWebView2PermissionKindClipboardRead,
		edge.CoreWebView2PermissionStateAllow,
	)

	if success := w.browser.Embed(w.hwnd); !success {
		os.Exit(0)
	}

	if settings, err := opts.Chromium.GetSettings(); err == nil {
		settings.PutAreDefaultContextMenusEnabled(opts.Debug)
		settings.PutAreDevToolsEnabled(opts.Debug)
		settings.PutIsPinchZoomEnabled(opts.Debug)
		settings.PutIsStatusBarEnabled(opts.Debug)
		settings.PutIsSwipeNavigationEnabled(opts.Debug)
		settings.PutAreBrowserAcceleratorKeysEnabled(opts.Debug)
		settings.PutIsZoomControlEnabled(opts.Debug)
	}

	atomic.StoreInt32(&w.browserReady, 1)

	w.browser.Resize()

	if w.route {
		w.browser.AddWebResourceRequestedFilter(w.host+"*", edge.COREWEBVIEW2_HOST_RESOURCE_ACCESS_KIND_ALLOW)
		var env = w.browser.Environment()
		w.browser.WebResourceRequestedCallback = func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
			var uri, err = request.GetUri()
			if err != nil {
				return
			}

			if route, found := w.routes[uri]; found {
				// 优化: 直接使用初始化时准备好的 route.content (字节数组)，完全消除零散堆分配
				var res, err = env.CreateWebResourceResponse(route.content, 200, "OK", route.headers)
				if err != nil {
					return
				}
				args.PutResponse(res)
			}
		}
	}

	return &wv
}
