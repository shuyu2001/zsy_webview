//go:build windows

package w32

import (
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// --- 1. 核心 DLL 载入 (必须定义在 NewProc 之前) ---
	user32   = windows.NewLazySystemDLL("user32")
	kernel32 = windows.NewLazySystemDLL("kernel32")
	gdi32    = windows.NewLazySystemDLL("gdi32")
	ole32    = windows.NewLazySystemDLL("ole32")
	shlwapi  = windows.NewLazySystemDLL("shlwapi")

	// --- 2. OLE32 / 核心 API ---
	Ole32CoInitializeEx = ole32.NewProc("CoInitializeEx")

	// --- 3. Kernel32 API ---
	Kernel32GetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	GetModuleHandleW           = kernel32.NewProc("GetModuleHandleW")

	// --- 4. Shlwapi API ---
	shlwapiSHCreateMemStream = shlwapi.NewProc("SHCreateMemStream")

	// --- 5. GDI32 API ---
	GetDeviceCaps = gdi32.NewProc("GetDeviceCaps")

	// --- 6. User32 API ---
	GetDC                         = user32.NewProc("GetDC")
	ReleaseDC                     = user32.NewProc("ReleaseDC")
	User32LoadImageW              = user32.NewProc("LoadImageW")
	User32GetSystemMetrics        = user32.NewProc("GetSystemMetrics")
	User32RegisterClassExW        = user32.NewProc("RegisterClassExW")
	User32CreateWindowExW         = user32.NewProc("CreateWindowExW")
	User32DestroyWindow           = user32.NewProc("DestroyWindow")
	User32ShowWindow              = user32.NewProc("ShowWindow")
	User32UpdateWindow            = user32.NewProc("UpdateWindow")
	User32SetFocus                = user32.NewProc("SetFocus")
	User32GetMessageW             = user32.NewProc("GetMessageW")
	User32TranslateMessage        = user32.NewProc("TranslateMessage")
	User32DispatchMessageW        = user32.NewProc("DispatchMessageW")
	User32DefWindowProcW          = user32.NewProc("DefWindowProcW")
	User32GetClientRect           = user32.NewProc("GetClientRect")
	User32PostQuitMessage         = user32.NewProc("PostQuitMessage")
	User32SetWindowTextW          = user32.NewProc("SetWindowTextW")
	User32PostThreadMessageW      = user32.NewProc("PostThreadMessageW")
	User32GetWindowLongPtrW       = user32.NewProc("GetWindowLongPtrW")
	User32SetWindowLongPtrW       = user32.NewProc("SetWindowLongPtrW")
	User32AdjustWindowRect        = user32.NewProc("AdjustWindowRect")
	User32SetWindowPos            = user32.NewProc("SetWindowPos")
	User32GetAncestor             = user32.NewProc("GetAncestor")
	User32IsDialogMessage         = user32.NewProc("IsDialogMessage")
	User32PostMessageW            = user32.NewProc("PostMessageW")
	User32SendMessageW            = user32.NewProc("SendMessageW")
	SetWindowPlacement            = user32.NewProc("SetWindowPlacement")
	GetWindowPlacement            = user32.NewProc("GetWindowPlacement")
	GetSystemMenu                 = user32.NewProc("GetSystemMenu")
	EnableMenuItem                = user32.NewProc("EnableMenuItem")
	SetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
)

// ==========================================
// 结构体定义 (Structures)
// ==========================================

type Point struct {
	X, Y int32
}

type Rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type POINT struct {
	X, Y int32
}

type WINDOWPLACEMENT struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    POINT
	MaxPosition    POINT
	NormalPosition Rect
}

type MinMaxInfo struct {
	PtReserved     Point
	PtMaxSize      Point
	PtMaxPosition  Point
	PtMinTrackSize Point
	PtMaxTrackSize Point
}

type Msg struct {
	Hwnd     syscall.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       Point
	LPrivate uint32
}

type WndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CnClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

// ==========================================
// 常量定义 (Constants)
// ==========================================

const (
	// 特殊句柄或标志
	CW_USEDEFAULT = 0x80000000
	GARoot        = 2
)

const (
	// --- COM 初始化标志 (COM Initialization Flags) ---
	COINIT_APARTMENTTHREADED = 0x2
	COINIT_MULTITHREADED     = 0x0
	COINIT_DISABLE_OLE1DDE   = 0x4
	COINIT_SPEED_OVER_MEMORY = 0x8
)

const (
	// --- 窗口显示样式 (Show Window Flags - SW) ---
	SW_HIDE          = 0
	SW_SHOWNORMAL    = 1
	SW_SHOWMINIMIZED = 2
	SW_MAXIMIZE      = 3
	SW_SHOW          = 5
	SW_MINIMIZE      = 6
	SW_RESTORE       = 9
)

const (
	// --- 系统指标参数 (System Metrics - SM) ---
	SM_CXSCREEN         = 0  // 屏幕宽度
	SM_CYSCREEN         = 1  // 屏幕高度
	SystemMetricsCxIcon = 11 // 图标标准宽度
	SystemMetricsCyIcon = 12 // 图标标准高度
)

const (
	// --- LoadImageW 标志位 (LoadImage Flags) ---
	LR_SHARED = 0x00008000 // 自动管理内存，防止句柄泄露
)

const (
	// --- 基础窗口样式 (Window Styles - WS) ---
	WSOverlapped        = 0x00000000
	WSCaption           = 0x00C00000
	WSSysMenu           = 0x00080000
	WS_THICKFRAME       = 0x00040000 // 允许拉伸大小的边框 (也叫 WS_SIZEBOX)
	WS_MINIMIZEBOX      = 0x00020000 // 最小化按钮
	WS_MAXIMIZEBOX      = 0x00010000 // 最大化按钮
	WS_POPUP            = 0x80000000 // 弹出式窗口（无边框）
	WS_CLIPCHILDREN     = 0x02000000 // 排除子窗口重绘区域（防闪烁）
	WS_CLIPSIBLINGS     = 0x04000000 // 裁剪兄弟窗口
	WS_OVERLAPPEDWINDOW = (WSOverlapped | WSCaption | WSSysMenu | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX)
)

const (
	// --- 扩展窗口样式 (Extended Window Styles - WS_EX) ---
	WS_EX_TOOLWINDOW = 0x00000080 // 工具箱窗口（不在任务栏显示）
	WS_EX_APPWINDOW  = 0x00040000 // 顶级应用窗口（强制在任务栏显示）
)

const (
	// --- SetWindowPos 坐标与置顶标志 (SetWindowPos Flags) ---
	HWND_TOPMOST     = ^uintptr(0) // 表示将窗口置顶
	SWP_NOSIZE       = 0x0001      // 忽略宽度和高度参数（保持当前大小）
	SWP_NOMOVE       = 0x0002      // 忽略 X 和 Y 参数（保持当前位置）
	SWP_NOACTIVATE   = 0x0010      // 不激活窗口（保持当前焦点不变）
	SWP_NOZOrder     = 0x0004
	SWP_FRAMECHANGED = 0x0020
)

const (
	// --- Class Styles & Backgrounds 窗口类属性 ---
	CS_VREDRAW   = 0x0001 // 垂直拉伸时重绘整个窗口
	CS_HREDRAW   = 0x0002 // 水平拉伸时重绘整个窗口
	COLOR_WINDOW = 5      // 标准窗口背景色
)

const (
	// --- 窗口消息 (Window Messages - WM) ---
	WM_CLOSE        = 0x0010
	WMDestroy       = 0x0002
	WMMove          = 0x0003
	WMSize          = 0x0005
	WMQuit          = 0x0012
	WMActivate      = 0x0006
	WMGetMinMaxInfo = 0x0024
	WMNCLButtonDown = 0x00A1
	WMMoving        = 0x0216
	WMApp           = 0x8000
)

const (
	GWLStyle = ^uintptr(15)

	SC_CLOSE = 0xF060

	MF_BYCOMMAND = 0x00000000
	MF_GRAYED    = 0x00000001
)

func Utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}

	// 查找 NUL 终止符
	end := unsafe.Pointer(p)
	n := 0
	for *(*uint16)(end) != 0 {
		end = unsafe.Pointer(uintptr(end) + unsafe.Sizeof(*p))
		n++
	}

	// 安全切片转换
	s := (*[(1 << 30) - 1]uint16)(unsafe.Pointer(p))[:n:n]
	return string(utf16.Decode(s))
}

func SHCreateMemStream(data []byte) (uintptr, error) {
	if len(data) == 0 {
		return 0, windows.ERROR_INVALID_PARAMETER
	}
	ret, _, err := shlwapiSHCreateMemStream.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
	)
	if ret == 0 {
		return 0, err
	}
	return ret, nil
}

func GetClientRect(hwnd uintptr) (Rect, error) {
	var rect Rect
	ret, _, err := User32GetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
	if ret == 0 {
		return Rect{}, err
	}
	return rect, nil
}

func DefWindowProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	ret, _, _ := User32DefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return ret
}

func DestroyWindow(hwnd uintptr) error {
	ret, _, err := User32DestroyWindow.Call(hwnd)
	if ret == 0 {
		return err
	}
	return nil
}
