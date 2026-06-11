//go:build windows
// +build windows

package edge

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ICoreWebView2NavigationStartingEventArgs struct {
	vtbl *ICoreWebView2NavigationStartingEventArgsVtbl
}

type ICoreWebView2NavigationStartingEventArgsVtbl struct {
	QueryInterface     ComProc
	AddRef             ComProc
	Release            ComProc
	GetUri             ComProc
	GetNavigationId    ComProc
	GetIsUserInitiated ComProc
	GetIsRedirected    ComProc
	GetRequestHeaders  ComProc
	GetCancel          ComProc
	PutCancel          ComProc
}

func (i *ICoreWebView2NavigationStartingEventArgs) PutCancel(cancel bool) error {

	hr, _, _ := i.vtbl.PutCancel.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&cancel)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return syscall.Errno(hr)
	}
	return nil
}

func (i *ICoreWebView2NavigationStartingEventArgs) GetUri() (string, error) {

	// Create *uint16 to hold result
	var _uri *uint16
	hr, _, _ := i.vtbl.GetUri.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&_uri)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	uri := windows.UTF16PtrToString(_uri)
	windows.CoTaskMemFree(unsafe.Pointer(_uri))
	return uri, nil
}
