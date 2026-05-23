package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/wailsapp/go-webview2/internal/w32"
	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

var (
	user32DLL = windows.NewLazySystemDLL("user32.dll")
	shcoreDLL = windows.NewLazySystemDLL("shcore.dll")

	procSetProcessDpiAwarenessContext = user32DLL.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForWindow               = user32DLL.NewProc("GetDpiForWindow")
	procSetProcessDpiAwareness        = shcoreDLL.NewProc("SetProcessDpiAwareness")
)

const (
	dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)
	dpiAwarenessContextPerMonitorAware   = ^uintptr(2)

	processPerMonitorDpiAware = 2

	baseDPI = 96
)

func initDPIAwareness() {
	ret, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2)
	if ret != 0 {
		return
	}
	ret, _, _ = procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAware)
	if ret != 0 {
		return
	}
	procSetProcessDpiAwareness.Call(processPerMonitorDpiAware) //nolint:errcheck
}

func getDPIForWindow(hwnd uintptr) uint32 {
	if hwnd != 0 {
		dpi, _, _ := procGetDpiForWindow.Call(hwnd)
		if dpi > 0 {
			return uint32(dpi)
		}
	}
	return baseDPI
}

func DPIScale(hwnd uintptr) (scaleX, scaleY float64) {
	dpi := float64(getDPIForWindow(hwnd))
	s := dpi / baseDPI
	return s, s
}

func ScaleForDPI(hwnd uintptr, logicalPx int) int {
	dpi := getDPIForWindow(hwnd)
	return int(float64(logicalPx) * float64(dpi) / baseDPI)
}

type WebviewOptions struct {
	Title  string
	Width  int
	Height int
	Host   string

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

	DPIAware bool

	Chromium *edge.Chromium
	Debug    bool
}

type route struct {
	content string
	headers string
	path    string
}

type window struct {
	hwnd uintptr
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
	w32.User32ShowWindow.Call(hwnd, uintptr(w32.WMClose), 0, 0)
}

func newWindow(hwnd uintptr) *window {
	return &window{hwnd: hwnd}
}

type webview struct {
	hwnd       uintptr
	mainthread uintptr

	Window *window

	routes map[string]route
	host   string
	route  bool
	center bool

	browser   *edge.Chromium
	autofocus bool

	dpi bool

	maxsz w32.Point
	minsz w32.Point

	mu        sync.Mutex
	bindings  map[string]interface{}
	dispatchq []func()

	// 原子标志
	destroyed    int32
	browserReady int32
}

var (
	windowContext     = make(map[uintptr]interface{})
	windowContextSync sync.RWMutex
)

func getWindowContext(wnd uintptr) interface{} {
	windowContextSync.RLock()
	defer windowContextSync.RUnlock()
	return windowContext[wnd]
}

func setWindowContext(wnd uintptr, data interface{}) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	windowContext[wnd] = data
}

func deleteWindowContext(wnd uintptr) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	delete(windowContext, wnd)
}

func (w *webview) Dispatch(fn func()) {
	w.mu.Lock()
	w.dispatchq = append(w.dispatchq, fn)
	w.mu.Unlock()
	w32.User32PostMessageW.Call(w.hwnd, w32.WMApp, 0, 0)
}

func (w *webview) Navigate(url string) {
	w.browser.Navigate(url)
}

func (w *webview) AddRoute(path string, content string, headers string) {
	w.routes[path] = route{path: path, content: content, headers: headers}
}

func (w *webview) AddHtmlContentRoute(path string, content string) {
	w.AddRoute(path, content, "Content-Type: text/html; charset=utf-8")
}

func (w *webview) Destroy() {
	w32.User32PostMessageW.Call(w.hwnd, w32.WMClose, 0, 0)
}

func (w *webview) SetHtml(html string) {
	w.browser.NavigateToString(html)
}

func (w *webview) Init(js string) {
	w.browser.Init(js)
}

func (w *webview) Eval(js string) {
	w.browser.Eval(js)
}

func (w *webview) NavigationCompletedCallback(fn func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs)) {
	w.browser.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
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
	w, ok := getWindowContext(hwnd).(*webview)
	if !ok {
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

	browserOK := atomic.LoadInt32(&w.browserReady) == 1
	isDestroyed := atomic.LoadInt32(&w.destroyed) == 1

	switch msg {
	case w32.WMMove, w32.WMMoving:
		if browserOK && !isDestroyed {
			_ = w.browser.NotifyParentWindowPositionChanged()
		}

	case w32.WMNCLButtonDown:
		w32.User32SetFocus.Call(w.hwnd)
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r

	case w32.WMSize:
		if browserOK && !isDestroyed {
			w.browser.Resize()
		}

	case w32.WMActivate:
		if wp != 0 && w.autofocus && browserOK && !isDestroyed {
			safeFocus(w.browser)
		}

	case w32.WMClose:
		w32.User32DestroyWindow.Call(hwnd)

	case w32.WMDestroy:
		atomic.StoreInt32(&w.destroyed, 1)
		deleteWindowContext(hwnd)
		w32.User32PostQuitMessage.Call(0)
		return 0

	case w32.WMGetMinMaxInfo:
		lpmmi := (*w32.MinMaxInfo)(unsafe.Pointer(lp))
		if w.maxsz.X > 0 && w.maxsz.Y > 0 {
			lpmmi.PtMaxSize = w.maxsz
			lpmmi.PtMaxTrackSize = w.maxsz
		}
		if w.minsz.X > 0 && w.minsz.Y > 0 {
			lpmmi.PtMinTrackSize = w.minsz
		}

	default:
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

	return 0
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

	if opts.DPIAware {
		scaleX, scaleY := DPIScale(0)
		windowWidth = int(float64(windowWidth) * scaleX)
		windowHeight = int(float64(windowHeight) * scaleY)
	}

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
			w32.SWPNoMove|w32.SWPNoSize|w32.SWPNoActivate,
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

func (w *webview) SetSize(width, height int) {
	if w.dpi {
		scaleX, scaleY := DPIScale(w.hwnd)
		width = int(float64(width) * scaleX)
		height = int(float64(height) * scaleY)
	}

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

	flags := w32.SWPNoZOrder
	if !w.center {
		flags |= w32.SWPNoMove
	}

	w32.User32SetWindowPos.Call(
		w.hwnd, 0,
		uintptr(x), uintptr(y),
		uintptr(width), uintptr(height),
		uintptr(flags),
	)

	w.browser.Resize()
}

func (w *webview) Run() {
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
			w.mu.Lock()
			localQ = localQ[:0]
			localQ = append(localQ, w.dispatchq...)
			w.dispatchq = w.dispatchq[:0]
			w.mu.Unlock()

			for _, fn := range localQ {
				fn()
			}
			continue
		}

		ancestor, _, _ := w32.User32GetAncestor.Call(uintptr(msg.Hwnd), w32.GARoot)
		isDialog, _, _ := w32.User32IsDialogMessage.Call(ancestor, uintptr(unsafe.Pointer(&msg)))
		if isDialog != 0 {
			continue
		}

		w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *webview) PostWebMessageAsJSON(data interface{}) error {
	var buff, _ = json.Marshal(&data)
	return w.browser.PostWebMessageAsJson(string(buff))
}

func (w *webview) PostWebMessageAsString(str string) error {
	return w.browser.PostWebMessageAsString(str)
}

type rpcMessage struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func NewWithOptions(opts WebviewOptions) *webview {
	if opts.Chromium == nil {
		log.Fatal("Chromium instance must be provided via WebviewOptions.Chromium")
		return nil
	}

	if opts.Host == "" {
		opts.Host = "http://shuyuz.app/"
	}

	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)

	w := &webview{
		bindings:  make(map[string]interface{}),
		routes:    make(map[string]route),
		host:      opts.Host,
		dpi:       opts.DPIAware,
		center:    opts.Center,
		route:     !opts.DisableRoute,
		autofocus: opts.AutoFocus,
		browser:   opts.Chromium,
	}

	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()

	if !w.createWindow(opts) {
		return nil
	}

	w.browser.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		w.msgcb(message)
	}

	w.Window = newWindow(w.hwnd)

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

	if w.route {
		w.browser.AddWebResourceRequestedFilter(w.host+"*", edge.COREWEBVIEW2_HOST_RESOURCE_ACCESS_KIND_ALLOW)
		var env = w.browser.Environment()
		w.browser.WebResourceRequestedCallback = func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
			var uri, err = request.GetUri()
			if err != nil {
				return
			}
			if route, found := w.routes[uri]; found {
				var res, err = env.CreateWebResourceResponse([]byte(route.content), 200, "OK", route.headers)
				fmt.Println("err = ", err)
				if err != nil {
					return
				}
				args.PutResponse(res)
			}
		}
	}

	w.browser.Resize()

	return w
}
