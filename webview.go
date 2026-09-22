//go:build windows
// +build windows

package zsy_webview

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/ncruces/zenity"
	"github.com/shuyu2001/zsy_webview/internal/w32"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
	"golang.org/x/sys/windows"
)

// ============================================================================
// 1. 常量与 Win32 底层 API 声明
// ============================================================================

const (
	LOGPIXELSX = 88
	LOGPIXELSY = 90

	wmKeyFirst = 0x0100
	wmKeyLast  = 0x0109

	swMinimize = 6

	wmDpiChanged                 = 0x02E0
	dwmwaSystemBackdropType      = 38
	dwmwaMicaEffectOld           = 1029
	dwmwaUseImmersiveDarkMode    = 20
	dwmwaUseImmersiveDarkModeOld = 19
	wsExNoRedirectionBitmap      = 0x00200000
)

var (
	windowContext     sync.Map
	registerClassOnce sync.Once
	wndProcCallback   uintptr

	modUser32          = windows.NewLazySystemDLL("user32.dll")
	procGetWindowLongW = modUser32.NewProc("GetWindowLongW")
	procSetWindowLongW = modUser32.NewProc("SetWindowLongW")
	procBeginPaint     = modUser32.NewProc("BeginPaint")
	procEndPaint       = modUser32.NewProc("EndPaint")

	modDwmapi                        = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmExtendFrameIntoClientArea = modDwmapi.NewProc("DwmExtendFrameIntoClientArea")
)

func init() {
	// 启用 Per-Monitor DPI 感知
	if w32.SetProcessDpiAwarenessContext.Find() == nil {
		aware := -4
		w32.SetProcessDpiAwarenessContext.Call(uintptr(aware))
	}
	wndProcCallback = windows.NewCallback(wndproc)
}

// ============================================================================
// 2. 结构体与类型定义
// ============================================================================

// BackdropType 定义 Windows 11 系统背景材质类型
// 注意：BackdropMica 仅对桌面壁纸生效；若要穿透并模糊其他所有窗口，请使用 BackdropAcrylic
type BackdropType int32

const (
	BackdropAuto    BackdropType = 0
	BackdropNone    BackdropType = 1
	BackdropMica    BackdropType = 2 // 仅穿透桌面壁纸
	BackdropAcrylic BackdropType = 3 // 穿透并模糊底层所有窗口
	BackdropMicaAlt BackdropType = 4 // 仅穿透桌面壁纸
)

type margins struct {
	cxLeftWidth    int32
	cxRightWidth   int32
	cyTopHeight    int32
	cyBottomHeight int32
}

// WebviewOptions 创建 WebView 实例的配置参数
type WebviewOptions struct {
	Title       string
	Width       int
	Height      int
	Host        string
	DebugPort   bool
	Transparent bool
	Icon        uintptr

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

type rpcMessage struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// window 封装所有与宿主原生窗体（Win32 HWND）相关的操作
type window struct {
	hwnd uintptr
	wv   *webview
}

// Webview 是对外提供的主控制接口
type Webview struct {
	Window *window
	wv     *webview
}

// webview 结构体保存内部核心数据与状态
type webview struct {
	hwnd       uintptr
	mainthread uintptr

	Window *window

	debugPort int

	mu     sync.RWMutex
	routes map[string]route
	host   string
	route  bool
	center bool

	transparent  bool
	backdropType BackdropType

	dpix float64
	dpiy float64

	browser   *edge.Chromium
	autofocus bool

	maxsz w32.Point
	minsz w32.Point

	bindings  map[string]interface{}
	dispatchq []func()

	destroyed    int32
	browserReady int32

	isChild bool

	onCloseCallback  func() bool
	onResizeCallback func(width, height int)
	onMoveCallback   func(x, y int)
	onFocusCallback  func()
	onBlurCallback   func()

	resourceCallbacks   []func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) bool
	userMessageCallback func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs)
}

// ============================================================================
// 3. 实例生命周期管理与工厂方法
// ============================================================================

// NewWithOptions 根据指定的配置选项初始化并创建主 Webview 实例
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

	dpiX, dpiY := getDPIScale()

	w := &webview{
		bindings:    make(map[string]interface{}),
		routes:      make(map[string]route, 64),
		host:        opts.Host,
		dpix:        dpiX,
		dpiy:        dpiY,
		center:      opts.Center,
		route:       !opts.DisableRoute,
		autofocus:   opts.AutoFocus,
		transparent: opts.Transparent,
		browser:     opts.Chromium,
		dispatchq:   make([]func(), 0, 32),
		isChild:     false,
	}

	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()

	if !w.createWindow(opts) {
		return nil
	}

	wv := &Webview{wv: w, Window: newWindow(w.hwnd, w)}
	w.Window = wv.Window

	if opts.Icon > 0 {
		w.setIcon(opts.Icon)
	}

	w.browser.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		switch message {
		case "__drag__":
			wv.Window.DragWindow()
			return
		case "__close__":
			wv.Window.CloseWindow()
			return
		}
		wv.msgcb(message)

		w.mu.RLock()
		userCb := w.userMessageCallback
		w.mu.RUnlock()
		if userCb != nil {
			userCb(message, sender, args)
		}
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

	if opts.Transparent {
		w.browser.SetTransparent()
	}

	atomic.StoreInt32(&w.browserReady, 1)
	w.browser.Resize()

	w.browser.WebResourceRequestedCallback = func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("webview: WebResourceRequested callback panic: %v", r)
			}
		}()

		w.mu.RLock()
		cbs := make([]func(*edge.ICoreWebView2WebResourceRequest, *edge.ICoreWebView2WebResourceRequestedEventArgs) bool, len(w.resourceCallbacks))
		copy(cbs, w.resourceCallbacks)
		w.mu.RUnlock()

		for _, cb := range cbs {
			if cb != nil && cb(request, args) {
				return
			}
		}

		if w.route {
			w.handleInternalRoute(request, args)
		}
	}

	if w.route {
		w.browser.AddWebResourceRequestedFilter(w.host+"*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL)
	}

	return wv
}

// NewChildWindow 共享主窗口的 WebView2 环境创建一个子窗口
func (w *Webview) NewChildWindow(opts WebviewOptions) *Webview {
	if w.wv == nil || w.wv.browser == nil {
		log.Println("Parent webview not initialized")
		return nil
	}

	if opts.Host == "" {
		opts.Host = w.wv.host
	}

	childChromium := edge.NewChromiumWithEnvironment(w.wv.browser.Environment())
	opts.Chromium = childChromium

	childWv := &webview{
		bindings:    make(map[string]interface{}),
		routes:      w.wv.routes,
		host:        opts.Host,
		dpix:        w.wv.dpix,
		dpiy:        w.wv.dpiy,
		center:      opts.Center,
		route:       !opts.DisableRoute,
		autofocus:   opts.AutoFocus,
		transparent: opts.Transparent,
		browser:     childChromium,
		dispatchq:   make([]func(), 0, 32),
		isChild:     true,
	}

	childWv.mainthread = w.wv.mainthread

	if !childWv.createWindow(opts) {
		return nil
	}

	child := &Webview{wv: childWv, Window: newWindow(childWv.hwnd, childWv)}
	childWv.Window = child.Window

	if opts.Icon > 0 {
		childWv.setIcon(opts.Icon)
	}

	childChromium.SetPermission(
		edge.CoreWebView2PermissionKindClipboardRead,
		edge.CoreWebView2PermissionStateAllow,
	)

	if success := childChromium.Embed(childWv.hwnd); !success {
		log.Println("Failed to embed Chromium into child window")
		w32.User32DestroyWindow.Call(childWv.hwnd)
		return nil
	}

	if settings, err := childChromium.GetSettings(); err == nil {
		settings.PutAreDefaultContextMenusEnabled(opts.Debug)
		settings.PutAreDevToolsEnabled(opts.Debug)
		settings.PutIsPinchZoomEnabled(opts.Debug)
		settings.PutIsStatusBarEnabled(opts.Debug)
		settings.PutIsSwipeNavigationEnabled(opts.Debug)
		settings.PutAreBrowserAcceleratorKeysEnabled(opts.Debug)
		settings.PutIsZoomControlEnabled(opts.Debug)
	}

	if opts.Transparent {
		childChromium.SetTransparent()
	}

	childChromium.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		switch message {
		case "__drag__":
			child.Window.DragWindow()
			return
		case "__close__":
			child.Window.CloseWindow()
			return
		}
		child.msgcb(message)

		childWv.mu.RLock()
		userCb := childWv.userMessageCallback
		childWv.mu.RUnlock()
		if userCb != nil {
			userCb(message, sender, args)
		}
	}

	atomic.StoreInt32(&childWv.browserReady, 1)
	childChromium.Resize()

	childChromium.WebResourceRequestedCallback = func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("child webview: WebResourceRequested callback panic: %v", r)
			}
		}()

		childWv.mu.RLock()
		cbs := make([]func(*edge.ICoreWebView2WebResourceRequest, *edge.ICoreWebView2WebResourceRequestedEventArgs) bool, len(childWv.resourceCallbacks))
		copy(cbs, childWv.resourceCallbacks)
		childWv.mu.RUnlock()

		for _, cb := range cbs {
			if cb != nil && cb(request, args) {
				return
			}
		}

		w.wv.mu.RLock()
		parentCbs := make([]func(*edge.ICoreWebView2WebResourceRequest, *edge.ICoreWebView2WebResourceRequestedEventArgs) bool, len(w.wv.resourceCallbacks))
		copy(parentCbs, w.wv.resourceCallbacks)
		w.wv.mu.RUnlock()

		for _, cb := range parentCbs {
			if cb != nil && cb(request, args) {
				return
			}
		}

		if childWv.route {
			childWv.handleInternalRoute(request, args)
		}
	}

	if childWv.route {
		childChromium.AddWebResourceRequestedFilter(childWv.host+"*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL)
	}

	return child
}

// EnsureSingleInstance 创建系统互斥体以确保单实例运行
func EnsureSingleInstance(uniqueAppID string) (windows.Handle, bool) {
	namePtr, _ := windows.UTF16PtrFromString("Local\\" + uniqueAppID)
	hMutex, _, err := w32.CreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if hMutex == 0 {
		return 0, false
	}
	if err == syscall.Errno(183) { // ERROR_ALREADY_EXISTS
		windows.CloseHandle(windows.Handle(hMutex))
		return 0, false
	}
	return windows.Handle(hMutex), true
}

// Run 进入 Win32 消息循环并阻塞运行
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

// Destroy 销毁当前 Webview 实例并请求关闭窗口
func (w *Webview) Destroy() {
	w32.User32PostMessageW.Call(w.wv.hwnd, w32.WM_CLOSE, 0, 0)
}

// Dispatch 将闭包任务安全分发到主 UI 线程执行
func (w *Webview) Dispatch(fn func()) {
	w.wv.mu.Lock()
	w.wv.dispatchq = append(w.wv.dispatchq, fn)
	w.wv.mu.Unlock()
	w32.User32PostMessageW.Call(w.wv.hwnd, w32.WMApp, 0, 0)
}

// HWND 获取当前窗口原生句柄
func (w *Webview) HWND() uintptr {
	return w.wv.hwnd
}

// ============================================================================
// 4. 原生窗体操作 (window 结构体方法)
// ============================================================================

func newWindow(hwnd uintptr, wv *webview) *window {
	return &window{hwnd: hwnd, wv: wv}
}

// SetTitle 设置窗体标题文本
func (w *window) SetTitle(title string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	w32.User32SetWindowTextW.Call(w.hwnd, uintptr(unsafe.Pointer(titlePtr)))
}

// SetMinSize 设置窗口最小限制尺寸（自动考虑 DPI）
func (w *window) SetMinSize(width, height int) {
	w.wv.minsz = w32.Point{
		X: int32(float64(width) * w.wv.dpix),
		Y: int32(float64(height) * w.wv.dpiy),
	}
}

// SetMaxSize 设置窗口最大限制尺寸（自动考虑 DPI）
func (w *window) SetMaxSize(width, height int) {
	w.wv.maxsz = w32.Point{
		X: int32(float64(width) * w.wv.dpix),
		Y: int32(float64(height) * w.wv.dpiy),
	}
}

// SetAlwaysOnTop 设置窗口是否总在最前
func (w *window) SetAlwaysOnTop(onTop bool) {
	var hwndInsertAfter uintptr = w32.HWND_NOTOPMOST
	if onTop {
		hwndInsertAfter = w32.HWND_TOPMOST
	}
	w32.User32SetWindowPos.Call(
		w.hwnd, hwndInsertAfter,
		0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOACTIVATE,
	)
}

// GetBounds 获取当前窗体逻辑坐标与尺寸（已消除 DPI 缩放）
func (w *window) GetBounds() (x, y, width, height int) {
	var rect w32.Rect
	w32.GetWindowRect.Call(w.hwnd, uintptr(unsafe.Pointer(&rect)))

	dpix := w.wv.dpix
	dpiy := w.wv.dpiy
	if dpix <= 0 {
		dpix = 1.0
	}
	if dpiy <= 0 {
		dpiy = 1.0
	}

	x = int(float64(rect.Left) / dpix)
	y = int(float64(rect.Top) / dpiy)
	width = int(float64(rect.Right-rect.Left) / dpix)
	height = int(float64(rect.Bottom-rect.Top) / dpiy)
	return
}

// SetBounds 显式设置窗体坐标与尺寸
func (w *window) SetBounds(x, y, width, height int) {
	realX := int(float64(x) * w.wv.dpix)
	realY := int(float64(y) * w.wv.dpiy)
	realW := int(float64(width) * w.wv.dpix)
	realH := int(float64(height) * w.wv.dpiy)

	w32.User32SetWindowPos.Call(
		w.hwnd, 0,
		uintptr(realX), uintptr(realY),
		uintptr(realW), uintptr(realH),
		w32.SWP_NOZOrder|w32.SWP_NOACTIVATE,
	)

	w.wv.browser.Resize()
	_ = w.wv.browser.NotifyParentWindowPositionChanged()
}

// SetPosition 移动窗体到指定屏幕坐标
func (w *window) SetPosition(x, y int) {
	realX := int(float64(x) * w.wv.dpix)
	realY := int(float64(y) * w.wv.dpiy)

	w32.User32SetWindowPos.Call(
		w.hwnd, 0,
		uintptr(realX), uintptr(realY),
		0, 0,
		w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_NOACTIVATE,
	)
	_ = w.wv.browser.NotifyParentWindowPositionChanged()
}

// SetSize 设置窗体逻辑尺寸，并在 center=true 时居中屏幕
func (w *window) SetSize(width, height int) {
	realW := int(float64(width) * w.wv.dpix)
	realH := int(float64(height) * w.wv.dpiy)

	screenW, _, _ := w32.User32GetSystemMetrics.Call(0)
	screenH, _, _ := w32.User32GetSystemMetrics.Call(1)

	x := (int(screenW) - realW) / 2
	y := (int(screenH) - realH) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	flags := uintptr(w32.SWP_NOZOrder)
	if !w.wv.center {
		flags |= w32.SWP_NOMOVE
	}

	w32.User32SetWindowPos.Call(
		w.hwnd, 0,
		uintptr(x), uintptr(y),
		uintptr(realW), uintptr(realH),
		flags,
	)
	w.wv.browser.Resize()
}

// SetSizeAndMax 设置窗体默认还原尺寸并将其最大化
func (w *window) SetSizeAndMax(width, height int) {
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

	w32.GetWindowPlacement.Call(w.hwnd, uintptr(unsafe.Pointer(&wp)))

	wp.NormalPosition = w32.Rect{
		Left:   int32(x),
		Top:    int32(y),
		Right:  int32(x + realW),
		Bottom: int32(y + realH),
	}
	wp.ShowCmd = w32.SW_MAXIMIZE

	w32.SetWindowPlacement.Call(w.hwnd, uintptr(unsafe.Pointer(&wp)))
	w.wv.browser.Resize()
}

// SetResizable 控制窗体是否允许拖拽调整尺寸
func (w *window) SetResizable(resizable bool) {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	if resizable {
		style |= uintptr(w32.WS_THICKFRAME)
	} else {
		style &^= uintptr(w32.WS_THICKFRAME)
	}
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
}

// SetMinimizable 控制窗体是否允许最小化
func (w *window) SetMinimizable(minimizable bool) {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	if minimizable {
		style |= uintptr(w32.WS_MINIMIZEBOX)
	} else {
		style &^= uintptr(w32.WS_MINIMIZEBOX)
	}
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
}

// SetClosable 启用或禁用系统菜单的关闭按钮
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

// Maximize 最大化窗口
func (w *window) Maximize() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_MAXIMIZE))
}

// MinimizeWindow 最小化窗口
func (w *window) MinimizeWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(swMinimize))
}

// RestoreWindow 还原窗口至常规大小
func (w *window) RestoreWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_RESTORE))
}

// HideWindow 隐藏窗口
func (w *window) HideWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_HIDE))
}

// ShowWindow 显示窗口
func (w *window) ShowWindow() {
	w32.User32ShowWindow.Call(w.hwnd, uintptr(w32.SW_SHOW))
}

// CloseWindow 发送关闭消息以请求关闭窗体
func (w *window) CloseWindow() {
	w32.User32PostMessageW.Call(w.hwnd, w32.WM_CLOSE, 0, 0)
}

// DisableMaximizeButton 禁用最大化按钮
func (w *window) DisableMaximizeButton() {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	style &^= w32.WS_MAXIMIZEBOX
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
	w32.User32SetWindowPos.Call(w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED)
}

// EnableMaximizeButton 启用最大化按钮
func (w *window) EnableMaximizeButton() {
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle))
	style |= w32.WS_MAXIMIZEBOX
	w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(w32.GWLStyle), style)
	w32.User32SetWindowPos.Call(w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED)
}

// DragWindow 处理无边框窗口的鼠标拖拽移动
func (w *window) DragWindow() {
	w32.ReleaseCapture.Call()
	const (
		wmNCLButtonDown = 0x00A1
		htCaption       = 2
	)
	w32.User32SendMessageW.Call(w.hwnd, wmNCLButtonDown, htCaption, 0)
}

// SetDarkMode 为窗口启用或禁用 Windows 沉浸式暗色模式
func (w *window) SetDarkMode(enable bool) {
	var val int32
	if enable {
		val = 1
	}
	ret, _, _ := w32.DwmSetWindowAttribute.Call(
		w.hwnd,
		dwmwaUseImmersiveDarkMode,
		uintptr(unsafe.Pointer(&val)),
		unsafe.Sizeof(val),
	)
	if ret != 0 {
		w32.DwmSetWindowAttribute.Call(
			w.hwnd,
			dwmwaUseImmersiveDarkModeOld,
			uintptr(unsafe.Pointer(&val)),
			unsafe.Sizeof(val),
		)
	}
}

// EnableNoRedirectionBitmap 启用 WS_EX_NOREDIRECTIONBITMAP 样式以支持原生 DirectComposition
func (w *window) EnableNoRedirectionBitmap() {
	cur, _, _ := procGetWindowLongW.Call(w.hwnd, ^uintptr(19)) // GWL_EXSTYLE
	procSetWindowLongW.Call(w.hwnd, ^uintptr(19), cur|uintptr(wsExNoRedirectionBitmap))
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
}

// SetSystemBackdrop 设置 Windows 11 DWM 原生系统背景材质 (Mica, Acrylic 等)
func (w *window) SetSystemBackdrop(backdrop BackdropType) {
	w.wv.backdropType = backdrop
	w.wv.transparent = true

	// 1. 扩展 DWM 帧到整个客户区
	w.wv.applyDwmFrame()

	// 2. 应用系统 Backdrop
	w.wv.applyBackdrop()

	// 3. 刷新窗口样式并设置浏览器透明背景
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
	if w.wv.browser != nil {
		w.wv.browser.SetTransparent()
	}
}

// SetTransparentBackground 将窗体及 WebView2 配置为完全透明背景
func (w *window) SetTransparentBackground() {
	w.wv.transparent = true

	var none int32 = 1
	w32.DwmSetWindowAttribute.Call(
		w.hwnd,
		dwmwaSystemBackdropType,
		uintptr(unsafe.Pointer(&none)),
		unsafe.Sizeof(none),
	)

	w.wv.applyDwmFrame()

	if w.wv.browser != nil {
		_ = w.wv.browser.SetTransparent()
	}

	// 4. 触发 DWM 刷新非客户区渲染树
	w32.User32SetWindowPos.Call(
		w.hwnd, 0, 0, 0, 0, 0,
		w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOZOrder|w32.SWP_FRAMECHANGED,
	)
}

// OpenFileDialog 弹出单文件选择对话框
func (w *window) OpenFileDialog(title string, options ...zenity.Option) (string, error) {
	opts := append([]zenity.Option{zenity.Title(title), zenity.Attach(w.hwnd)}, options...)
	return zenity.SelectFile(opts...)
}

// OpenFileMultipleDialog 弹出多文件选择对话框
func (w *window) OpenFileMultipleDialog(title string, options ...zenity.Option) ([]string, error) {
	opts := append([]zenity.Option{zenity.Title(title), zenity.Attach(w.hwnd)}, options...)
	return zenity.SelectFileMultiple(opts...)
}

// SelectFolderDialog 弹出文件夹选择对话框
func (w *window) SelectFolderDialog(title string, options ...zenity.Option) (string, error) {
	opts := append([]zenity.Option{zenity.Title(title), zenity.Directory(), zenity.Attach(w.hwnd)}, options...)
	return zenity.SelectFile(opts...)
}

// SaveFileDialog 弹出保存文件对话框
func (w *window) SaveFileDialog(title string, defaultName string, options ...zenity.Option) (string, error) {
	homeDir, _ := os.UserHomeDir()
	downloadsDir := filepath.Join(homeDir, "Downloads")

	defaultFullPath := defaultName
	if defaultName != "" {
		if !filepath.IsAbs(defaultName) {
			defaultFullPath = filepath.Join(downloadsDir, defaultName)
		}
	} else {
		defaultFullPath = downloadsDir + string(filepath.Separator)
	}

	opts := []zenity.Option{
		zenity.Title(title),
		zenity.Attach(w.hwnd),
		zenity.ConfirmOverwrite(),
		zenity.Filename(defaultFullPath),
	}
	opts = append(opts, options...)
	return zenity.SelectFileSave(opts...)
}

// PromptDialog 弹出带有输入框的文本输入对话框
func (w *window) PromptDialog(title, text, defaultValue string) (string, error) {
	return zenity.Entry(text,
		zenity.Title(title),
		zenity.EntryText(defaultValue),
		zenity.Attach(w.hwnd),
	)
}

// ShowInfo 显示信息提示框
func (w *window) ShowInfo(title, message string) {
	tPtr, _ := windows.UTF16PtrFromString(title)
	mPtr, _ := windows.UTF16PtrFromString(message)
	const mbIconInfo = 0x00000040
	_, _ = windows.MessageBox(windows.HWND(w.hwnd), mPtr, tPtr, mbIconInfo)
}

// ShowWarning 显示警告提示框
func (w *window) ShowWarning(title, message string) {
	tPtr, _ := windows.UTF16PtrFromString(title)
	mPtr, _ := windows.UTF16PtrFromString(message)
	const mbIconWarning = 0x00000030
	_, _ = windows.MessageBox(windows.HWND(w.hwnd), mPtr, tPtr, mbIconWarning)
}

// ShowError 显示错误提示框
func (w *window) ShowError(title, message string) {
	tPtr, _ := windows.UTF16PtrFromString(title)
	mPtr, _ := windows.UTF16PtrFromString(message)
	const mbIconError = 0x00000010
	_, _ = windows.MessageBox(windows.HWND(w.hwnd), mPtr, tPtr, mbIconError)
}

// ShowConfirm 显示确认提示框（确定/取消）
func (w *window) ShowConfirm(title, message string) bool {
	tPtr, _ := windows.UTF16PtrFromString(title)
	mPtr, _ := windows.UTF16PtrFromString(message)
	const (
		mbOkCancel     = 0x00000001
		mbIconQuestion = 0x00000020
		idOk           = 1
	)
	ret, _ := windows.MessageBox(windows.HWND(w.hwnd), mPtr, tPtr, mbOkCancel|mbIconQuestion)
	return ret == idOk
}

// SelectColorDialog 弹出颜色选择器并返回十六进制颜色值
func (w *window) SelectColorDialog(title string, defaultColorHex string) string {
	c, err := zenity.SelectColor(
		zenity.Title(title),
		zenity.Attach(w.hwnd),
	)
	if err != nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// SelectItemDialog 弹出单选列表框
func (w *window) SelectItemDialog(title, text string, items []string) (string, error) {
	return zenity.List(text, items,
		zenity.Title(title),
		zenity.Attach(w.hwnd),
		zenity.DisallowEmpty(),
	)
}

// SelectMultipleItemsDialog 弹出多选复选列表框
func (w *window) SelectMultipleItemsDialog(title, text string, items []string) ([]string, error) {
	return zenity.ListMultiple(text, items,
		zenity.Title(title),
		zenity.Attach(w.hwnd),
		zenity.CheckList(),
	)
}

// ShowNotification 显示系统通知消息气泡
func (w *window) ShowNotification(title, message string, iconPath string) error {
	opts := []zenity.Option{zenity.Title(title)}
	if iconPath != "" {
		opts = append(opts, zenity.Icon(iconPath))
	}
	return zenity.Notify(message, opts...)
}

// ============================================================================
// 5. 浏览器控制与 JS 交互
// ============================================================================

func jsString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Navigate 导航跳转到指定 URL
func (w *Webview) Navigate(url string) {
	w.wv.browser.Navigate(url)
}

// SetHtml 直接加载并渲染 HTML 字符串内容
func (w *Webview) SetHtml(html string) {
	w.wv.browser.NavigateToString(html)
}

// Init 注入在页面每次加载时最先执行的初始化 JavaScript 代码
func (w *Webview) Init(js string) {
	w.wv.browser.Init(js)
}

// Eval 在当前页面上下文中立即执行一段 JavaScript
func (w *Webview) Eval(js string) {
	w.wv.browser.Eval(js)
}

// Resize 触发 WebView2 控件重新计算并适应宿主客户区大小
func (w *Webview) Resize() {
	w.wv.browser.Resize()
}

// Reload 重新加载当前页面
func (w *Webview) Reload() {
	w.wv.browser.Reload()
}

// Stop 停止当前页面的加载
func (w *Webview) Stop() {
	w.wv.browser.Stop()
}

// GoBack 浏览器后退
func (w *Webview) GoBack() {
	w.wv.browser.GoBack()
}

// GoForward 浏览器前进
func (w *Webview) GoForward() {
	w.wv.browser.GoForward()
}

// GetURL 获取当前页面的完整 URL 地址
func (w *Webview) GetURL() string {
	url, err := w.wv.browser.GetCurrentURL()
	if err != nil {
		return ""
	}
	return url
}

// OpenDevTools 打开 Chromium 开发者工具调试窗口
func (w *Webview) OpenDevTools() {
	w.wv.browser.OpenDevToolsWindow()
}

// AddBrowerArgs 追加启动 Chromium 时的附加命令行参数
func (w *Webview) AddBrowerArgs(value string) {
	w.wv.browser.AdditionalBrowserArgs = append(w.wv.browser.AdditionalBrowserArgs, value)
}

// GetDebugPort 获取当前实例启用的远程调试端口号
func (w *Webview) GetDebugPort() int {
	return w.wv.debugPort
}

// OpenURLInDefaultBrowser 使用系统默认浏览器打开指定的外部链接
func (w *Webview) OpenURLInDefaultBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// OnMessageCallback 注册接收来自前端 window.chrome.webview.postMessage 的消息回调
func (w *Webview) OnMessageCallback(fn func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs)) func() {
	w.wv.mu.Lock()
	w.wv.userMessageCallback = fn
	w.wv.mu.Unlock()

	return func() {
		w.wv.mu.Lock()
		w.wv.userMessageCallback = nil
		w.wv.mu.Unlock()
	}
}

// PostWebMessageAsJSON 向前端发送 JSON 格式消息
func (w *Webview) PostWebMessageAsJSON(data interface{}) error {
	buff, err := json.Marshal(&data)
	if err != nil {
		return err
	}
	return w.wv.browser.PostWebMessageAsJson(string(buff))
}

// PostWebMessageAsString 向前端发送纯文本消息
func (w *Webview) PostWebMessageAsString(str string) error {
	return w.wv.browser.PostWebMessageAsString(str)
}

// Emit 向前端派发一个全局 CustomEvent 自定义事件
func (w *Webview) Emit(eventName string, data interface{}) {
	buff, _ := json.Marshal(&data)
	js := fmt.Sprintf(`window.dispatchEvent(new CustomEvent('%s', { detail: %s }));`, eventName, string(buff))
	w.Eval(js)
}

// AddHotKey 注册热键响应回调
func (w *Webview) AddHotKey(fn func(w *Webview), hotKeys ...string) {
	AddWebviewEvent(w, fn, hotKeys...)
}

// OnNavigationCompleted 注册导航完成事件监听器
func (w *Webview) OnNavigationCompleted(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs)) {
	w.wv.browser.NavigationCompletedCallback = cb
}

// OnNavigationStarting 注册导航即将开始事件监听器
func (w *Webview) OnNavigationStarting(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationStartingEventArgs)) {
	w.wv.browser.NavigationStartingCallback = cb
}

// OnSourceChanged 注册页面源地址变更事件监听器
func (w *Webview) OnSourceChanged(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2SourceChangedEventArgs)) {
	w.wv.browser.SourceChangedCallback = cb
}

// OnProcessFailed 注册 Chromium 渲染/GPU 进程崩溃/故障事件监听器
func (w *Webview) OnProcessFailed(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2ProcessFailedEventArgs)) {
	w.wv.browser.ProcessFailedCallback = cb
}

// CaptureScreenshot 使用前端 Canvas 截图并返回 base64 格式的 PNG 图片数据
func (w *Webview) CaptureScreenshot(callback func(base64png string)) {
	if callback == nil {
		return
	}

	oldCallback := w.wv.browser.MessageCallback
	w.wv.browser.MessageCallback = func(message string, sender *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(message), &msg); err == nil && msg.Type == "__screenshot__" {
			w.wv.browser.MessageCallback = oldCallback
			callback(msg.Data)
			return
		}
		if oldCallback != nil {
			oldCallback(message, sender, args)
		}
	}

	js := `
	(function() {
		function runCapture() {
			html2canvas(document.documentElement, {
				useCORS: true,
				allowTaint: true,
				scale: window.devicePixelRatio || 1,
				windowWidth: document.documentElement.scrollWidth,
				windowHeight: document.documentElement.scrollHeight
			}).then(function(canvas) {
				window.chrome.webview.postMessage(JSON.stringify({
					type: '__screenshot__',
					data: canvas.toDataURL('image/png')
				}));
			}).catch(function(e) {
				console.error("html2canvas error:", e);
			});
		}

		if (typeof html2canvas === 'undefined') {
			var s = document.createElement('script');
			s.src = 'https://cdnjs.cloudflare.com/ajax/libs/html2canvas/1.4.1/html2canvas.min.js';
			s.onload = function() { setTimeout(runCapture, 150); };
			document.head.appendChild(s);
		} else {
			setTimeout(runCapture, 150);
		}
	})();
	`
	w.Eval(js)
}

// SaveScreenshot 截取网页屏幕并将其保存为本地图片文件
func (w *Webview) SaveScreenshot(filename string, callback func(err error)) {
	w.CaptureScreenshot(func(base64png string) {
		if base64png == "" {
			if callback != nil {
				callback(fmt.Errorf("截图失败，数据为空"))
			}
			return
		}

		if idx := strings.Index(base64png, ","); idx != -1 {
			base64png = base64png[idx+1:]
		}

		data, err := base64.StdEncoding.DecodeString(base64png)
		if err != nil {
			if callback != nil {
				callback(err)
			}
			return
		}

		err = os.WriteFile(filename, data, 0644)
		if callback != nil {
			callback(err)
		}
	})
}

// ============================================================================
// 6. 虚拟路由与资源拦截
// ============================================================================

// AddRoute 添加内存静态资源虚拟路由映射
func (w *Webview) AddRoute(path string, content string, headers string) {
	normalizedPath := strings.TrimSuffix(path, "/")
	w.wv.mu.Lock()
	w.wv.routes[normalizedPath] = route{path: path, content: []byte(content), headers: headers}
	w.wv.mu.Unlock()
}

// AddHtmlContentRoute 快速注册 HTML 虚拟路由
func (w *Webview) AddHtmlContentRoute(path string, content string) {
	w.AddRoute(path, content, "Content-Type: text/html; charset=utf-8")
}

// AddWebResourceRequestedFilter 添加网络资源请求拦截的 URL 过滤器
func (w *Webview) AddWebResourceRequestedFilter(uri string, resourceContext edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT) {
	w.wv.browser.AddWebResourceRequestedFilter(uri, resourceContext)
}

// OnWebResourceRequested 注册自定义网络资源请求拦截回调
func (w *Webview) OnWebResourceRequested(cb func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) bool) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.resourceCallbacks = append(w.wv.resourceCallbacks, cb)
}

// RegisterEmbedFS 将 Go 嵌入的文件系统 (embed.FS) 挂载注册为虚拟路由
func (w *Webview) RegisterEmbedFS(rootFS embed.FS, rootPath string) {
	_ = fs.WalkDir(rootFS, rootPath, func(p string, d fs.DirEntry, err error) error {
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
		routePath = strings.ReplaceAll(routePath, "\\", "/")

		if routePath == "" {
			w.AddRoute(strings.TrimSuffix(w.wv.host, "/"), string(content), headers)
			w.AddRoute(w.wv.host+"index.html", string(content), headers)
		}

		fullURL := w.wv.host + routePath
		w.AddRoute(fullURL, string(content), headers)
		return nil
	})
}

// handleInternalRoute 处理内部路由匹配与响应构建
func (w *webview) handleInternalRoute(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
	uri, err := request.GetUri()
	if err != nil {
		return
	}

	lookupURI := uri
	if idx := strings.IndexAny(lookupURI, "?#"); idx != -1 {
		lookupURI = lookupURI[:idx]
	}
	lookupURI = strings.TrimSuffix(lookupURI, "/")

	w.mu.RLock()
	routeItem, found := w.routes[lookupURI]
	w.mu.RUnlock()

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
				w.mu.RLock()
				routeItem, found = w.routes[hostBase]
				w.mu.RUnlock()
			}
		}
	}

	if found {
		env := w.browser.Environment()
		res, err := env.CreateWebResourceResponse(routeItem.content, 200, "OK", routeItem.headers)
		if err != nil {
			return
		}
		args.PutResponse(res)
	}
}

// ============================================================================
// 7. Win32 内部辅助与窗口过程处理
// ============================================================================

// getDPIScale 获取主屏幕当前的 DPI 缩放比率
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

func safeExecute(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("webview: Dispatch task panic: %v", r)
		}
	}()
	fn()
}

func safeFocus(browser *edge.Chromium) {
	defer func() { _ = recover() }()
	browser.Focus()
}

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

func (w *webview) setIcon(id uintptr) {
	hInstance, _, _ := w32.GetModuleHandleW.Call(0)
	hIcon, _, _ := w32.User32LoadImageW.Call(hInstance, id, 1, 0, 0, 0x40)
	w32.User32SendMessageW.Call(w.hwnd, 0x0080, 0, hIcon) // ICON_SMALL
	w32.User32SendMessageW.Call(w.hwnd, 0x0080, 1, hIcon) // ICON_BIG
}

// createWindow 注册 Win32 窗口类并创建原生窗口
func (w *webview) createWindow(opts WebviewOptions) bool {
	var hinstance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &hinstance); err != nil {
		log.Printf("GetModuleHandleEx failed: %v", err)
		return false
	}

	className, err := windows.UTF16PtrFromString("webview_class")
	if err != nil {
		log.Printf("UTF16PtrFromString(className) failed: %v", err)
		return false
	}

	registerClassOnce.Do(func() {
		icow, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCxIcon)
		icoh, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCyIcon)
		icon, _, _ := w32.User32LoadImageW.Call(0, 32512, icow, icoh, 0x00008000)

		wc := w32.WndClassExW{
			CbSize:        uint32(unsafe.Sizeof(w32.WndClassExW{})),
			HInstance:     hinstance,
			LpszClassName: className,
			HIcon:         windows.Handle(icon),
			HIconSm:       windows.Handle(icon),
			LpfnWndProc:   wndProcCallback,
			HbrBackground: 0, // 透明模式绝不使用实色画刷
		}
		if ret, _, _ := w32.User32RegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
			log.Println("RegisterClassExW failed (class may already exist)")
		}
	})

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

	var exStyle uint32
	if opts.HideInTaskbar {
		exStyle |= 0x00000080
	} else {
		exStyle |= 0x00040000
	}
	if opts.Transparent {
		exStyle |= wsExNoRedirectionBitmap
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

	if opts.Transparent {
		w.applyDwmFrame()
	}

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

// applyDwmFrame 将 DWM 窗口边框扩展至整个客户区
func (w *webview) applyDwmFrame() {
	m := margins{-1, -1, -1, -1}
	procDwmExtendFrameIntoClientArea.Call(
		w.hwnd,
		uintptr(unsafe.Pointer(&m)),
	)
}

// applyBackdrop 调用原生 DWM API 配置系统磨砂材质
func (w *webview) applyBackdrop() {
	val := int32(w.backdropType)
	ret, _, _ := w32.DwmSetWindowAttribute.Call(
		w.hwnd,
		dwmwaSystemBackdropType,
		uintptr(unsafe.Pointer(&val)),
		unsafe.Sizeof(val),
	)
	if ret != 0 && w.backdropType == BackdropMica {
		var micaTrue int32 = 1
		w32.DwmSetWindowAttribute.Call(
			w.hwnd,
			dwmwaMicaEffectOld,
			uintptr(unsafe.Pointer(&micaTrue)),
			unsafe.Sizeof(micaTrue),
		)
	}
}

// OnClose 注册窗口即将关闭的拦截回调。
// 回调返回 true 表示允许关闭；返回 false 表示拦截并取消关闭操作（可用于“未保存内容”确认弹窗）。
func (w *Webview) OnClose(cb func() bool) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.onCloseCallback = cb
}

// OnResize 注册窗口尺寸改变回调，传入已消除 DPI 缩放的逻辑尺寸 (width, height)。
func (w *Webview) OnResize(cb func(width, height int)) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.onResizeCallback = cb
}

// OnMove 注册窗口位置移动回调，传入已消除 DPI 缩放的逻辑坐标 (x, y)。
// 自动支持多显示器负坐标处理。
func (w *Webview) OnMove(cb func(x, y int)) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.onMoveCallback = cb
}

// OnFocus 注册窗口获得焦点（被激活）时的回调。
func (w *Webview) OnFocus(cb func()) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.onFocusCallback = cb
}

// OnBlur 注册窗口失去焦点（切换到其他应用）时的回调。
func (w *Webview) OnBlur(cb func()) {
	w.wv.mu.Lock()
	defer w.wv.mu.Unlock()
	w.wv.onBlurCallback = cb
}

// wndproc Win32 主窗口过程回调函数
// wndproc Win32 主窗口过程回调函数
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
	case 0x0014: // WM_ERASEBKGND
		if w.transparent {
			return 1
		}

	case 0x000F: // WM_PAINT
		if w.transparent {
			var ps [64]byte
			procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))
			procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps[0])))
			return 0
		}

	case 0x0086: // WM_NCACTIVATE
		if w.transparent {
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, 1, lp)
			return r
		}

	case w32.WMMove:
		_ = w.browser.NotifyParentWindowPositionChanged()
		w.mu.RLock()
		moveCb := w.onMoveCallback
		dpix, dpiy := w.dpix, w.dpiy
		w.mu.RUnlock()

		if moveCb != nil {
			// Win32 LOWORD/HIWORD 转换为有符号 16 位整数以正确支持副显示器负坐标
			xPos := int(int16(lp & 0xFFFF))
			yPos := int(int16((lp >> 16) & 0xFFFF))
			if dpix <= 0 {
				dpix = 1.0
			}
			if dpiy <= 0 {
				dpiy = 1.0
			}
			logicalX := int(float64(xPos) / dpix)
			logicalY := int(float64(yPos) / dpiy)
			moveCb(logicalX, logicalY)
		}

	case w32.WMNCLButtonDown:
		w32.User32SetFocus.Call(w.hwnd)

	case w32.WMSize:
		if wp != 1 { // 1 代表 SIZE_MINIMIZED，最小化时不调整渲染尺寸
			w.browser.Resize()
		}

		w.mu.RLock()
		resizeCb := w.onResizeCallback
		dpix, dpiy := w.dpix, w.dpiy
		w.mu.RUnlock()

		if resizeCb != nil {
			rawW := int(lp & 0xFFFF)
			rawH := int((lp >> 16) & 0xFFFF)
			if dpix <= 0 {
				dpix = 1.0
			}
			if dpiy <= 0 {
				dpiy = 1.0
			}
			logicalW := int(float64(rawW) / dpix)
			logicalH := int(float64(rawH) / dpiy)
			resizeCb(logicalW, logicalH)
		}
		return 0

	case w32.WMActivate:
		isMinimized := (wp >> 16) != 0
		isInactive := (wp & 0xffff) == 0
		if !isInactive && !isMinimized && w.autofocus {
			safeFocus(w.browser)
		}

		w.mu.RLock()
		focusCb := w.onFocusCallback
		blurCb := w.onBlurCallback
		w.mu.RUnlock()

		if isInactive {
			if blurCb != nil {
				blurCb()
			}
		} else {
			if focusCb != nil {
				focusCb()
			}
		}
		return 0

	case wmDpiChanged:
		w.dpix = float64(wp&0xFFFF) / 96.0
		w.dpiy = float64(wp>>16) / 96.0
		if lp != 0 {
			rect := (*w32.Rect)(unsafe.Pointer(lp))
			w32.User32SetWindowPos.Call(
				hwnd, 0,
				uintptr(rect.Left),
				uintptr(rect.Top),
				uintptr(rect.Right-rect.Left),
				uintptr(rect.Bottom-rect.Top),
				w32.SWP_NOZOrder|w32.SWP_NOACTIVATE,
			)
		}
		if w.transparent {
			w.applyDwmFrame()
		}
		return 0

	case w32.WM_CLOSE:
		w.mu.RLock()
		closeCb := w.onCloseCallback
		w.mu.RUnlock()

		// 执行用户自定义拦截回调
		if closeCb != nil {
			allowClose := closeCb()
			if !allowClose {
				// 用户选择取消关闭，直接 return 0 阻止系统销毁窗体
				return 0
			}
		}

		w32.User32DestroyWindow.Call(hwnd)
		return 0

	case w32.WMDestroy:
		atomic.StoreInt32(&w.destroyed, 1)
		deleteWindowContext(hwnd)
		if !w.isChild {
			w32.User32PostQuitMessage.Call(0)
		}
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
