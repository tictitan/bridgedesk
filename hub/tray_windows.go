//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Native Windows system-tray icon so BridgeDesk is managed like a normal desktop app:
// double-click to open the dashboard, right-click for a menu (open, copy key, pause/resume, quit).

var (
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procLoadIconW         = user32.NewProc("LoadIconW")
	procLoadCursorW       = user32.NewProc("LoadCursorW")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenuW       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procSetMenuDefaultItem = user32.NewProc("SetMenuDefaultItem")
)

const (
	wmTrayIcon      = 0x8000 + 1 // WM_APP+1
	nimAdd          = 0x0
	nimModify       = 0x1
	nimDelete       = 0x2
	nifMessage      = 0x1
	nifIcon         = 0x2
	nifTip          = 0x4
	wmRButtonUp     = 0x0205
	wmLButtonDblClk = 0x0203
	wmCommand       = 0x0111
	wmDestroy       = 0x0002
	mfString        = 0x0
	mfSeparator     = 0x800
	tpmRightButton  = 0x0002
	idiApplication  = 32512
	idcArrow        = 32512

	cmdOpen    = 1
	cmdCopy    = 2
	cmdToggle  = 3
	cmdElevate = 4
	cmdQuit    = 9
)

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
}

type point struct{ X, Y int32 }
type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}
type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type tray struct {
	srv   *Server
	hwnd  uintptr
	nid   notifyIconData
	hIcon uintptr
	quit  func()
}

var theTray *tray

// runTray creates the tray icon and pumps the Windows message loop. Blocks; call on its own goroutine.
func runTray(srv *Server, quit func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("BridgeDeskTray")
	hIcon, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
	hCursor, _, _ := procLoadCursorW.Call(0, uintptr(idcArrow))

	t := &tray{srv: srv, hIcon: hIcon, quit: quit}
	theTray = t

	wc := wndClassExW{
		style:         0,
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		hIcon:         hIcon,
		hCursor:       hCursor,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, _ := syscall.UTF16PtrFromString("BridgeDesk")
	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	t.hwnd = hwnd

	t.nid = notifyIconData{
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayIcon,
		hIcon:            hIcon,
	}
	t.nid.cbSize = uint32(unsafe.Sizeof(t.nid))
	t.setTip(fmt.Sprintf("BridgeDesk — %s", srv.cfg.Name))
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid)))

	// message loop
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
}

func (t *tray) setTip(s string) {
	src := syscall.StringToUTF16(s)
	for i := range t.nid.szTip {
		if i < len(src) {
			t.nid.szTip[i] = src[i]
		} else {
			t.nid.szTip[i] = 0
		}
	}
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	t := theTray
	switch uint32(message) {
	case wmTrayIcon:
		switch uint32(lParam) {
		case wmLButtonDblClk:
			openDashboard(t.srv)
		case wmRButtonUp:
			showMenu(t)
		}
		return 0
	case wmCommand:
		switch uint32(wParam) & 0xFFFF {
		case cmdOpen:
			openDashboard(t.srv)
		case cmdCopy:
			clipboardSet(t.srv.cfg.AccessKey)
			showBalloon(t, "Access key copied", t.srv.cfg.AccessKey)
		case cmdToggle:
			t.srv.cfg.ListenLAN = !t.srv.cfg.ListenLAN
			t.srv.cfg.Save()
			state := "paused"
			if t.srv.cfg.ListenLAN {
				state = "resumed"
			}
			showBalloon(t, "BridgeDesk", "LAN sharing "+state+". Restart to apply fully.")
		case cmdElevate:
			if relaunchElevated() { // elevated copy is starting; hand over the ports
				if t.quit != nil {
					t.quit()
				}
				procPostQuitMessage.Call(0)
			} else {
				showBalloon(t, "BridgeDesk", "Could not restart as administrator (UAC declined).")
			}
		case cmdQuit:
			if t.quit != nil {
				t.quit()
			}
			procPostQuitMessage.Call(0)
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

func showMenu(t *tray) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	add := func(id int, label string) {
		p, _ := syscall.UTF16PtrFromString(label)
		procAppendMenuW.Call(hMenu, mfString, uintptr(id), uintptr(unsafe.Pointer(p)))
	}
	add(cmdOpen, "Open BridgeDesk")
	add(cmdCopy, "Copy access key")
	toggleLabel := "Pause network sharing"
	if !t.srv.cfg.ListenLAN {
		toggleLabel = "Resume network sharing"
	}
	add(cmdToggle, toggleLabel)
	if !isElevated() {
		add(cmdElevate, "Restart as administrator")
	}
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	add(cmdQuit, "Quit")
	procSetMenuDefaultItem.Call(hMenu, cmdOpen, 0)

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWin.Call(t.hwnd)
	procTrackPopupMenu.Call(hMenu, tpmRightButton, uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	procDestroyMenu.Call(hMenu)
}

func openDashboard(s *Server) {
	openTarget(fmt.Sprintf("http://127.0.0.1:%d/?key=%s", s.cfg.LocalPort, s.cfg.AccessKey))
}

// showBalloon reuses the tray icon to show a short notification.
func showBalloon(t *tray, title, text string) {
	desktopToast(title, text)
}
