package zsy_webview

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
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

	wmKeyFirst = 0x0100
	wmKeyLast  = 0x0109
)

func init() {
	// 安全检测：只在系统支持 SetProcessDpiAwarenessContext 时调用，防止在旧版本 Windows 上崩溃
	if w32.SetProcessDpiAwarenessContext.Find() == nil {
		aware := -4 // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
		w32.SetProcessDpiAwarenessContext.Call(uintptr(aware))
	}
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
	content []byte
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

func (w *window) SetResizable(resizable bool) {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	if resizable {
		style |= uintptr(w32.WS_THICKFRAME)
	} else {
		style &^= uintptr(w32.WS_THICKFRAME)
	}
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
}

func (w *window) SetMinimizable(minimizable bool) {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	if minimizable {
		style |= uintptr(w32.WS_MINIMIZEBOX)
	} else {
		style &^= uintptr(w32.WS_MINIMIZEBOX)
	}
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
}

func (w *window) SetClosable(closable bool) {
	hMenu, _, _ := w32.GetSystemMenu.Call(w.hwnd, 0)
	if hMenu != 0 {
		var enable uintptr = w32.MF_BYCOMMAND
		if !closable {
			enable |= w32.MF_GRAYED
		}
		w32.EnableMenuItem.Call(hMenu, w32.SC_CLOSE, enable)
	}
}

func (w *window) Maximize() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_MAXIMIZE))
}

func (w *window) MinimizeWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.WS_MINIMIZEBOX))
}

func (w *window) RestoreWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_RESTORE))
}

func (w *window) HideWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_HIDE))
}

func (w *window) ShowWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_SHOW))
}

func (w *window) CloseWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.WM_CLOSE), 0, 0)
}

func (w *window) DisableMaximizeButton() {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(
		w.hwnd,
		uintptr(w32.GWLStyle),
	)
	style &^= w32.WS_MAXIMIZEBOX
	w32.User32SetWindowLongPtrW.Call(
		w.hwnd,
		uintptr(w32.GWLStyle),
		style,
	)
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
}

func (w *window) EnableMaximizeButton() {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(
		w.hwnd,
		uintptr(w32.GWLStyle),
	)
	style |= w32.WS_MAXIMIZEBOX
	w32.User32SetWindowLongPtrW.Call(
		w.hwnd,
		uintptr(w32.GWLStyle),
		style,
	)
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

func (w *Webview) Resize() {
	w.wv.browser.Resize()
}

func (w *Webview) Navigate(url string) {
	w.wv.browser.Navigate(url)
}

// AddRoute 新增写锁保护，避免多线程并发修改 w.routes Map 导致运行时 crash
func (w *Webview) AddRoute(path string, content string, headers string) {
	normalizedPath := strings.TrimSuffix(path, "/")
	w.wv.mu.Lock()
	w.wv.routes[normalizedPath] = route{path: path, content: []byte(content), headers: headers}
	w.wv.mu.Unlock()
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

func jsString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func safeFocus(browser *edge.Chromium) {
	defer func() {
		recover() // 静默恢复，Focus panic 不应崩溃整个 UI 线程
	}()
	browser.Focus()
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
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

	if atomic.LoadInt32(&w.destroyed) == 1 || atomic.LoadInt32(&w.browserReady) == 0 {
		r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

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
		return 0

	case w32.WMActivate:
		isMinimized := (wp >> 16) != 0
		isInactive := (wp & 0xffff) == 0

		if !isInactive && !isMinimized && w.autofocus {
			safeFocus(w.browser)
		}
		return 0

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
		Style:         0x0003,
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
		if opts.DisableResize || opts.DisableMaximize {
			style &^= w32.WS_THICKFRAME
			style &^= w32.WS_MAXIMIZEBOX
		} else {
			style |= w32.WS_THICKFRAME
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

func (w *Webview) HWND() uintptr {
	return w.wv.hwnd
}

func (w *Webview) SetSizeAndMax(width, height int) {
	realW := int(float64(width) * w.wv.dpix)
	realH := int(float64(height) * w.wv.dpiy)

	screenW, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CXSCREEN)
	screenH, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CYSCREEN)

	x := (int(screenW) - realW) / 2
	y := (int(screenH) - realH) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	var wp w32.WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))

	w32.GetWindowPlacement.Call(
		w.wv.hwnd,
		uintptr(unsafe.Pointer(&wp)),
	)

	wp.NormalPosition = w32.Rect{
		Left:   int32(x),
		Top:    int32(y),
		Right:  int32(x + realW),
		Bottom: int32(y + realH),
	}

	wp.ShowCmd = w32.SW_MAXIMIZE

	w32.SetWindowPlacement.Call(
		w.wv.hwnd,
		uintptr(unsafe.Pointer(&wp)),
	)

	w.wv.browser.Resize()
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

	localQ := make([]func(), 0, 32)

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
			localQ, w.wv.dispatchq = w.wv.dispatchq, localQ[:0]
			w.wv.mu.Unlock()

			for _, fn := range localQ {
				if fn != nil {
					safeExecute(fn)
				}
			}
			for i := range localQ {
				localQ[i] = nil
			}
			localQ = localQ[:0]
			continue
		}

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

func (w *Webview) AddBrowerArgs(value string) {
	w.wv.browser.AdditionalBrowserArgs = append(w.wv.browser.AdditionalBrowserArgs, value)
}

func (w *Webview) RegisterEmbedFS(rootFS embed.FS, rootPath string) {
	fs.WalkDir(rootFS, rootPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		content, err := fs.ReadFile(rootFS, p)
		if err != nil {
			return nil
		}

		relPath, _ := filepath.Rel(rootPath, p)
		routePath := relPath

		if routePath == "index.html" {
			routePath = ""
		}

		ext := strings.ToLower(filepath.Ext(p))
		mimeType := mime.TypeByExtension(ext)

		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		headers := "Content-Type: " + mimeType

		// 归一化路径中的反斜杠
		routePath = strings.ReplaceAll(routePath, "\\", "/")

		// 如果是主页，额外注册一个不含 index.html 后缀的 Host 作为备用基础地址
		if routePath == "" {
			w.AddRoute(strings.TrimSuffix(w.wv.host, "/"), string(content), headers)
			w.AddRoute(w.wv.host+"index.html", string(content), headers)
		}

		fullURL := w.wv.host + routePath
		w.AddRoute(fullURL, string(content), headers)

		return nil
	})
}

func (w *Webview) AddHotKey(fn func(w *Webview), hotKeys ...string) {
	AddWebviewEvent(w, fn, hotKeys...)
}

func (w *Webview) Reload() {
	w.wv.browser.Reload()
}

func (w *Webview) Stop() {
	w.wv.browser.Stop()
}

func (w *Webview) GoBack() {
	w.wv.browser.GoBack()
}

func (w *Webview) GoForward() {
	w.wv.browser.GoForward()
}

func NewWithOptions(opts WebviewOptions) *Webview {
	if opts.Chromium == nil {
		log.Fatal("Chromium instance must be provided via WebviewOptions.Chromium")
		return nil
	}

	if opts.DebugPort {
		var safePort int
		for {
			p := FastRandPort9000()
			if !IsPortUsed(p) {
				safePort = p
				break
			}
		}
		opts.Chromium.AdditionalBrowserArgs = append(opts.Chromium.AdditionalBrowserArgs, fmt.Sprintf("--remote-debugging-port=%d", safePort))
		opts.Chromium.AdditionalBrowserArgs = append(opts.Chromium.AdditionalBrowserArgs, "--remote-debugging-address=127.0.0.1")
	}

	if opts.Host == "" {
		opts.Host = "http://shuyuz.app/"
	} else if !strings.HasSuffix(opts.Host, "/") {
		opts.Host += "/"
	}

	_ = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)

	var dpiX, dpiY = getDPIScale()

	w := &webview{
		bindings:  make(map[string]interface{}),
		routes:    make(map[string]route, 64),
		host:      opts.Host,
		dpix:      dpiX,
		dpiy:      dpiY,
		center:    opts.Center,
		route:     !opts.DisableRoute,
		autofocus: opts.AutoFocus,
		browser:   opts.Chromium,
		dispatchq: make([]func(), 0, 32),
	}

	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()

	if !w.createWindow(opts) {
		return nil
	}

	var wv = Webview{wv: w, Window: newWindow(w.hwnd)}
	w.Window = wv.Window // 双向绑定结构体字段，确保安全引用

	w.setIcon(opts.Icon)

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
		w.browser.AddWebResourceRequestedFilter(w.host+"*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL)
		var env = w.browser.Environment()
		w.browser.WebResourceRequestedCallback = func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
			var uri, err = request.GetUri()
			if err != nil {
				return
			}

			lookupURI := uri
			if idx := strings.IndexAny(lookupURI, "?#"); idx != -1 {
				lookupURI = lookupURI[:idx]
			}

			lookupURI = strings.TrimSuffix(lookupURI, "/")

			// 加锁读保护
			w.mu.Lock()
			route, found := w.routes[lookupURI]
			w.mu.Unlock()

			if !found {
				hostBase := strings.TrimSuffix(w.host, "/")
				if strings.HasPrefix(lookupURI, hostBase) {
					lastSlash := strings.LastIndex(lookupURI, "/")
					hasExt := false
					if lastSlash != -1 {
						hasExt = strings.Contains(lookupURI[lastSlash:], ".")
					} else {
						hasExt = strings.Contains(lookupURI, ".")
					}

					if !hasExt {
						w.mu.Lock()
						route, found = w.routes[hostBase]
						w.mu.Unlock()
					}
				}
			}

			if found {
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
