package zsy_webview

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	MOD_ALT      = 0x0001
	MOD_CONTROL  = 0x0002
	MOD_SHIFT    = 0x0004
	MOD_WIN      = 0x0008
	MOD_NOREPEAT = 0x4000
	WM_HOTKEY    = 0x0312

	// 自定义 Windows 线程消息，用于跨线程交互
	WM_USER            = 0x0400
	WM_USER_REGISTER   = WM_USER + 1
	WM_USER_UNREGISTER = WM_USER + 2
)

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procRegisterHK    = user32.NewProc("RegisterHotKey")
	procGetMessage    = user32.NewProc("GetMessageW")
	procUnregisterHK  = user32.NewProc("UnregisterHotKey")
	procPostThreadMsg = user32.NewProc("PostThreadMessageW")
	procPeekMessage   = user32.NewProc("PeekMessageW")

	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThID = kernel32.NewProc("GetCurrentThreadId")
)

// MSG 结构体（兼容 32 位与 64 位对齐）
type MSG struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// 内部交互指令结构
type hotkeyCmd struct {
	id   uintptr
	mods uintptr
	vk   uintptr
	fn   func()
	err  chan error
}

type unregCmd struct {
	id  uintptr
	err chan error
}

// 全局单例管理
var (
	loopOnce     sync.Once
	loopThreadID uint32
	loopReady    = make(chan struct{})

	regMutex sync.Mutex
	nextID   uintptr = 1

	// 任务通道
	regChan   = make(chan hotkeyCmd, 10)
	unregChan = make(chan unregCmd, 10)
)

// 启动单例后台消息循环
func ensureMessageLoop() {
	loopOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			// 强制 Windows 系统为当前线程创建消息队列
			var msg MSG
			procPeekMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, WM_USER, WM_USER, 0) // PM_NOREMOVE = 0

			// 获取当前线程 ID 供外部投递消息
			tid, _, _ := procGetCurrentThID.Call()
			loopThreadID = uint32(tid)

			// 信号：通知主线程初始化完成
			close(loopReady)

			// 仅在当前线程内维护的 handlers，无并发冲突隐患
			handlers := make(map[uintptr]func())

			for {
				res, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
				if res <= 0 {
					break
				}

				switch msg.Message {
				case WM_HOTKEY:
					id := msg.WParam
					if fn, ok := handlers[id]; ok {
						// 异步执行回调，防止阻塞消息循环
						go fn()
					}

				case WM_USER_REGISTER:
					// 消费所有待注册热键
				DrainReg:
					for {
						select {
						case cmd := <-regChan:
							ret, _, err := procRegisterHK.Call(0, cmd.id, cmd.mods, cmd.vk)
							if ret == 0 {
								cmd.err <- err
							} else {
								handlers[cmd.id] = cmd.fn
								cmd.err <- nil
							}
						default:
							break DrainReg
						}
					}

				case WM_USER_UNREGISTER:
					// 消费所有待注销热键
				DrainUnreg:
					for {
						select {
						case cmd := <-unregChan:
							ret, _, err := procUnregisterHK.Call(0, cmd.id)
							if ret == 0 {
								cmd.err <- err
							} else {
								delete(handlers, cmd.id)
								cmd.err <- nil
							}
						default:
							break DrainUnreg
						}
					}
				}
			}
		}()
		<-loopReady // 等待初始化就绪
	})
}

// AddEvent 注册全局按键事件
// 返回一个用于注销该热键的 cancel 函数和可能发生的错误
func AddEvent(fn func(), keys ...string) (cancel func(), err error) {
	mods, vk := parseKeySlice(keys)
	if vk == 0 {
		return nil, fmt.Errorf("无法识别的按键序列: %v", keys)
	}

	ensureMessageLoop()

	// 保证 ID 生成安全
	regMutex.Lock()
	id := nextID
	nextID++
	regMutex.Unlock()

	errChan := make(chan error, 1)
	regChan <- hotkeyCmd{
		id:   id,
		mods: mods,
		vk:   vk,
		fn:   fn,
		err:  errChan,
	}

	// 唤醒后台线程处理注册
	ret, _, postErr := procPostThreadMsg.Call(uintptr(loopThreadID), WM_USER_REGISTER, 0, 0)
	if ret == 0 {
		return nil, fmt.Errorf("发送注册请求失败: %w", postErr)
	}

	// 同步等待注册结果，避免静默失败
	if err := <-errChan; err != nil {
		return nil, fmt.Errorf("系统注册热键失败: %w", err)
	}

	// 返回注销函数，调用即可释放热键
	cancelFunc := func() {
		unregErrChan := make(chan error, 1)
		unregChan <- unregCmd{
			id:  id,
			err: unregErrChan,
		}
		procPostThreadMsg.Call(uintptr(loopThreadID), WM_USER_UNREGISTER, 0, 0)
		<-unregErrChan // 同步等待注销完毕
	}

	return cancelFunc, nil
}

// AddWebviewEvent 注册 Webview 相关的按键事件
func AddWebviewEvent(w *Webview, fn func(w *Webview), keys ...string) (func(), error) {
	var wrapFn = func() {
		w.Dispatch(func() {
			fn(w)
		})
	}
	return AddEvent(wrapFn, keys...)
}

// 完整的键名映射表（保留原样）
var keyMap = map[string]uintptr{
	"a": 0x41, "b": 0x42, "c": 0x43, "d": 0x44, "e": 0x45, "f": 0x46, "g": 0x47, "h": 0x48,
	"i": 0x49, "j": 0x4A, "k": 0x4B, "l": 0x4C, "m": 0x4D, "n": 0x4E, "o": 0x4F, "p": 0x50,
	"q": 0x51, "r": 0x52, "s": 0x53, "t": 0x54, "u": 0x55, "v": 0x56, "w": 0x57, "x": 0x58,
	"y": 0x59, "z": 0x5A,
	"0": 0x30, "1": 0x31, "2": 0x32, "3": 0x33, "4": 0x34, "5": 0x35, "6": 0x36, "7": 0x37, "8": 0x38, "9": 0x39,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75, "f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
	"esc": 0x1B, "enter": 0x0D, "space": 0x20, "tab": 0x09, "backspace": 0x08, "delete": 0x2E, "insert": 0x2D,
	"home": 0x24, "end": 0x23, "pgup": 0x21, "pgdn": 0x22, "pause": 0x13, "print": 0x2C, "caps": 0x14,
	"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
	"num0": 0x60, "num1": 0x61, "num2": 0x62, "num3": 0x63, "num4": 0x64, "num5": 0x65, "num6": 0x66, "num7": 0x67, "num8": 0x68, "num9": 0x69,
	"num*": 0x6A, "num+": 0x6B, "num-": 0x6D, "num.": 0x6E, "num/": 0x6F,
	";": 0xBA, "=": 0xBB, ",": 0xBC, "-": 0xBD, ".": 0xBE, "/": 0xBF, "`": 0xC0,
	"[": 0xDB, "\\": 0xDC, "]": 0xDD, "'": 0xDE,
}

func parseKeySlice(keys []string) (uintptr, uintptr) {
	var mods uintptr = MOD_NOREPEAT
	var vk uintptr = 0

	for _, k := range keys {
		lowered := strings.ToLower(strings.TrimSpace(k))
		switch lowered {
		case "ctrl", "control":
			mods |= MOD_CONTROL
		case "alt", "menu":
			mods |= MOD_ALT
		case "shift":
			mods |= MOD_SHIFT
		case "win", "windows":
			mods |= MOD_WIN
		default:
			if code, ok := keyMap[lowered]; ok {
				vk = code
			}
		}
	}
	return mods, vk
}
