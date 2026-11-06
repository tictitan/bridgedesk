//go:build windows

package main

import (
	"errors"
	"image"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"unsafe"
)

const (
	smCXSCREEN        = 0
	smCYSCREEN        = 1
	smXVIRTUALSCREEN  = 76
	smYVIRTUALSCREEN  = 77
	smCXVIRTUALSCREEN = 78
	smCYVIRTUALSCREEN = 79
)

var (
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procGetCursorInfo       = user32.NewProc("GetCursorInfo")
	procGetIconInfo         = user32.NewProc("GetIconInfo")
	procDrawIconEx          = user32.NewProc("DrawIconEx")
	procMapVirtualKeyW      = user32.NewProc("MapVirtualKeyW")
	procVkKeyScanW          = user32.NewProc("VkKeyScanW")
	procLockWorkStation     = user32.NewProc("LockWorkStation")
)

func screenSupported() bool { return true }

func sysMetric(i int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int(int32(uint32(r)))
}

func screenSize() (int, int) { return sysMetric(smCXSCREEN), sysMetric(smCYSCREEN) }

// ---------------- monitors ----------------

type rect32 struct{ L, T, R, B int32 }

type monitorInfoEx struct {
	Size          uint32
	Monitor, Work rect32
	Flags         uint32
	Device        [32]uint16
}

var (
	monMu  sync.Mutex
	monAcc []Monitor
	// One callback for the process lifetime: syscall.NewCallback slots are never freed.
	monCb = syscall.NewCallback(func(h, hdc, lprc, lparam uintptr) uintptr {
		var mi monitorInfoEx
		mi.Size = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r != 0 {
			monAcc = append(monAcc, Monitor{
				X: int(mi.Monitor.L), Y: int(mi.Monitor.T),
				W: int(mi.Monitor.R - mi.Monitor.L), H: int(mi.Monitor.B - mi.Monitor.T),
				Primary: mi.Flags&1 != 0, Name: syscall.UTF16ToString(mi.Device[:]),
			})
		}
		return 1
	})
)

// listMonitors returns the displays, primary first then left-to-right.
func listMonitors() []Monitor {
	monMu.Lock()
	monAcc = nil
	procEnumDisplayMonitors.Call(0, 0, monCb, 0)
	mons := append([]Monitor(nil), monAcc...)
	monMu.Unlock()
	if len(mons) == 0 {
		w, h := screenSize()
		mons = []Monitor{{W: w, H: h, Primary: true, Name: "primary"}}
	}
	sort.SliceStable(mons, func(i, j int) bool {
		if mons[i].Primary != mons[j].Primary {
			return mons[i].Primary
		}
		if mons[i].X != mons[j].X {
			return mons[i].X < mons[j].X
		}
		return mons[i].Y < mons[j].Y
	})
	for i := range mons {
		mons[i].Index = i
	}
	return mons
}

// monitorRect resolves a monitor index (-1 = all screens) to virtual-desktop pixels.
func monitorRect(idx int) Monitor {
	if idx < 0 {
		return Monitor{Index: -1, X: sysMetric(smXVIRTUALSCREEN), Y: sysMetric(smYVIRTUALSCREEN),
			W: sysMetric(smCXVIRTUALSCREEN), H: sysMetric(smCYVIRTUALSCREEN), Name: "all"}
	}
	mons := listMonitors()
	if idx >= len(mons) {
		idx = 0
	}
	return mons[idx]
}

// captureScreen grabs the primary display (kept for callers that don't pick a monitor).
func captureScreen() (*image.RGBA, error) {
	img, _, err := captureMonitor(0)
	return img, err
}

// captureMonitor grabs one display (or all of them with idx -1) via GDI BitBlt, cursor included.
func captureMonitor(idx int) (*image.RGBA, Monitor, error) {
	m := monitorRect(idx)
	w, h := m.W, m.H
	if w <= 0 || h <= 0 {
		return nil, m, errors.New("no display")
	}
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, m, errors.New("GetDC failed")
	}
	defer procReleaseDC.Call(0, screenDC)
	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	defer procDeleteDC.Call(memDC)
	bmp, _, _ := procCreateCompatibleBitmap.Call(screenDC, uintptr(w), uintptr(h))
	defer procDeleteObject.Call(bmp)
	old, _, _ := procSelectObject.Call(memDC, bmp)
	defer procSelectObject.Call(memDC, old)

	const SRCCOPY = 0x00CC0020
	const CAPTUREBLT = 0x40000000
	if r, _, _ := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, uintptr(m.X), uintptr(m.Y), SRCCOPY|CAPTUREBLT); r == 0 {
		return nil, m, errors.New("BitBlt failed (the screen may be locked or on the secure desktop)")
	}
	drawCursor(memDC, m.X, m.Y)

	type bitmapInfoHeader struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		ClrUsed, ClrImportant  uint32
	}
	type bitmapInfo struct {
		Header bitmapInfoHeader
		Colors [1]uint32
	}
	bi := bitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h) // top-down
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = 0 // BI_RGB

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	buf := img.Pix
	const DIB_RGB_COLORS = 0
	r, _, _ := procGetDIBits.Call(memDC, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), DIB_RGB_COLORS)
	if r == 0 {
		return nil, m, errors.New("GetDIBits failed")
	}
	// GDI gives BGRA; swap to RGBA in place and set alpha opaque.
	for i := 0; i < len(buf); i += 4 {
		buf[i], buf[i+2] = buf[i+2], buf[i]
		buf[i+3] = 0xFF
	}
	return img, m, nil
}

// drawCursor paints the current mouse pointer into the captured bitmap (BitBlt leaves it out).
func drawCursor(memDC uintptr, originX, originY int) {
	type cursorInfo struct {
		Size, Flags uint32
		Cursor      uintptr
		X, Y        int32
	}
	type iconInfo struct {
		FIcon      int32
		XHot, YHot uint32
		Mask       uintptr
		Color      uintptr
	}
	ci := cursorInfo{}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 || ci.Flags&1 == 0 || ci.Cursor == 0 {
		return
	}
	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(ci.Cursor, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return
	}
	if ii.Mask != 0 {
		procDeleteObject.Call(ii.Mask)
	}
	if ii.Color != 0 {
		procDeleteObject.Call(ii.Color)
	}
	x := int(ci.X) - int(ii.XHot) - originX
	y := int(ci.Y) - int(ii.YHot) - originY
	const DI_NORMAL = 0x0003
	procDrawIconEx.Call(memDC, uintptr(x), uintptr(y), ci.Cursor, 0, 0, 0, 0, DI_NORMAL)
}

func lockSession() error {
	if r, _, e := procLockWorkStation.Call(); r == 0 {
		return e
	}
	return nil
}

// ---------------- input injection ----------------

type mouseInputStruct struct {
	dx, dy      int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keybdInputStruct struct {
	wVk, wScan  uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type inputUnion struct {
	typ uint32
	_   uint32 // padding to align the 64-bit union on amd64
	mi  mouseInputStruct
}

func sendInput(in *inputUnion) error {
	r, _, e := procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
	if r != 1 {
		return e
	}
	return nil
}

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove       = 0x0001
	mouseEventLeftDown   = 0x0002
	mouseEventLeftUp     = 0x0004
	mouseEventRightDown  = 0x0008
	mouseEventRightUp    = 0x0010
	mouseEventMiddleDown = 0x0020
	mouseEventMiddleUp   = 0x0040
	mouseEventWheel      = 0x0800
	mouseEventHWheel     = 0x1000
	mouseEventAbsolute   = 0x8000
	mouseEventVirtualDsk = 0x4000

	keyEventExtended = 0x0001
	keyEventKeyUp    = 0x0002
	keyEventUnicode  = 0x0004
)

// toAbsolute maps a 0..1 position on a monitor to SendInput's 0..65535 virtual-desktop space.
func toAbsolute(mon int, nx, ny float64) (int32, int32) {
	m := monitorRect(mon)
	px := float64(m.X) + clamp01(nx)*float64(m.W-1)
	py := float64(m.Y) + clamp01(ny)*float64(m.H-1)
	vx, vy := float64(sysMetric(smXVIRTUALSCREEN)), float64(sysMetric(smYVIRTUALSCREEN))
	vw, vh := float64(sysMetric(smCXVIRTUALSCREEN)), float64(sysMetric(smCYVIRTUALSCREEN))
	if vw <= 1 || vh <= 1 {
		return int32(clamp01(nx) * 65535), int32(clamp01(ny) * 65535)
	}
	return int32((px - vx) * 65535 / (vw - 1)), int32((py - vy) * 65535 / (vh - 1))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

const wheelUnitsPerStep = 40 // the dashboard sends ±3 per wheel notch → 120 = one Windows notch

func doMouse(p MouseInput) error {
	in := inputUnion{typ: inputMouse}
	positioned := false
	switch p.Action {
	case "move", "down", "up", "click", "dblclick":
		positioned = true
	case "scroll", "hscroll":
		positioned = p.X > 0 || p.Y > 0
	}
	if positioned {
		in.mi.dx, in.mi.dy = toAbsolute(p.Monitor, p.X, p.Y)
		in.mi.dwFlags = mouseEventMove | mouseEventAbsolute | mouseEventVirtualDsk
	}
	down, up := mouseEventLeftDown, mouseEventLeftUp
	switch p.Button {
	case "right":
		down, up = mouseEventRightDown, mouseEventRightUp
	case "middle":
		down, up = mouseEventMiddleDown, mouseEventMiddleUp
	}
	switch p.Action {
	case "move":
		return sendInput(&in)
	case "down":
		in.mi.dwFlags |= uint32(down)
		return sendInput(&in)
	case "up":
		in.mi.dwFlags |= uint32(up)
		return sendInput(&in)
	case "click":
		sendInput(&in)
		c := inputUnion{typ: inputMouse}
		c.mi.dwFlags = uint32(down)
		sendInput(&c)
		c.mi.dwFlags = uint32(up)
		return sendInput(&c)
	case "dblclick":
		sendInput(&in)
		for i := 0; i < 2; i++ {
			c := inputUnion{typ: inputMouse}
			c.mi.dwFlags = uint32(down)
			sendInput(&c)
			c.mi.dwFlags = uint32(up)
			sendInput(&c)
		}
		return nil
	case "scroll", "hscroll":
		if positioned {
			sendInput(&in)
		}
		if p.DeltaY != 0 {
			w := inputUnion{typ: inputMouse}
			w.mi.dwFlags = mouseEventWheel
			w.mi.mouseData = uint32(int32(-p.DeltaY * wheelUnitsPerStep))
			sendInput(&w)
		}
		if p.DeltaX != 0 {
			w := inputUnion{typ: inputMouse}
			w.mi.dwFlags = mouseEventHWheel
			w.mi.mouseData = uint32(int32(p.DeltaX * wheelUnitsPerStep))
			sendInput(&w)
		}
		return nil
	}
	return nil
}

// extendedVK are keys that need KEYEVENTF_EXTENDEDKEY to be told apart from their numpad twins.
var extendedVK = map[uint16]bool{
	0x21: true, 0x22: true, 0x23: true, 0x24: true, 0x25: true, 0x26: true, 0x27: true, 0x28: true,
	0x2C: true, 0x2D: true, 0x2E: true, 0x5B: true, 0x5C: true, 0x5D: true, 0x6F: true, 0x90: true,
	0xA3: true, 0xA5: true, 0xAD: true, 0xAE: true, 0xAF: true, 0xB0: true, 0xB1: true, 0xB2: true, 0xB3: true,
}

func keyUnit(vk uint16, up bool) inputUnion {
	in := inputUnion{typ: inputKeyboard}
	kb := (*keybdInputStruct)(unsafe.Pointer(&in.mi))
	kb.wVk = vk
	sc, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC: games/RDP read scan codes
	kb.wScan = uint16(sc)
	if extendedVK[vk] {
		kb.dwFlags |= keyEventExtended
	}
	if up {
		kb.dwFlags |= keyEventKeyUp
	}
	return in
}

// vkName maps friendly key names from the browser to Windows virtual-key codes.
var vkName = map[string]uint16{
	"enter": 0x0D, "return": 0x0D, "tab": 0x09, "escape": 0x1B, "esc": 0x1B,
	"backspace": 0x08, "delete": 0x2E, "del": 0x2E, "space": 0x20, "up": 0x26, "down": 0x28,
	"left": 0x25, "right": 0x27, "home": 0x24, "end": 0x23, "pageup": 0x21,
	"pagedown": 0x22, "insert": 0x2D, "ctrl": 0x11, "control": 0x11, "alt": 0x12,
	"shift": 0x10, "win": 0x5B, "meta": 0x5B, "cmd": 0x5B, "apps": 0x5D, "menu": 0x5D, "contextmenu": 0x5D,
	"capslock": 0x14, "printscreen": 0x2C, "prtsc": 0x2C, "pause": 0x13, "numlock": 0x90, "scrolllock": 0x91,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
	"volumemute": 0xAD, "volumedown": 0xAE, "volumeup": 0xAF,
	"medianext": 0xB0, "mediaprev": 0xB1, "mediastop": 0xB2, "mediaplaypause": 0xB3,
	"plus": 0xBB, "minus": 0xBD, ";": 0xBA, "=": 0xBB, ",": 0xBC, "-": 0xBD, ".": 0xBE, "/": 0xBF,
	"`": 0xC0, "[": 0xDB, "\\": 0xDC, "]": 0xDD, "'": 0xDE,
}

// vkForChar resolves a single character to a virtual key using the PC's keyboard layout.
func vkForChar(c byte) (uint16, bool) {
	if c >= 'a' && c <= 'z' {
		return uint16(c - 32), true
	}
	if c >= '0' && c <= '9' {
		return uint16(c), true
	}
	r, _, _ := procVkKeyScanW.Call(uintptr(c))
	if int16(uint16(r)) == -1 {
		return 0, false
	}
	return uint16(r) & 0xFF, true
}

// doKeyCombo handles a key or a combo like "ctrl+alt+delete" (action: down|up|press).
func doKeyCombo(combo, action string) error {
	parts := splitLower(combo, '+')
	var vks []uint16
	for _, p := range parts {
		if vk, ok := vkName[p]; ok {
			vks = append(vks, vk)
		} else if len(p) == 1 {
			if vk, ok := vkForChar(p[0]); ok {
				vks = append(vks, vk)
			}
		}
	}
	if len(vks) == 0 {
		return errors.New("unknown key: " + combo)
	}
	switch action {
	case "down":
		for _, vk := range vks {
			u := keyUnit(vk, false)
			sendInput(&u)
		}
	case "up":
		for i := len(vks) - 1; i >= 0; i-- {
			u := keyUnit(vks[i], true)
			sendInput(&u)
		}
	default:
		for _, vk := range vks {
			u := keyUnit(vk, false)
			sendInput(&u)
		}
		for i := len(vks) - 1; i >= 0; i-- {
			u := keyUnit(vks[i], true)
			sendInput(&u)
		}
	}
	return nil
}

// typeText injects a Unicode string as keystrokes.
func typeText(s string) error {
	for _, r := range s {
		if r == '\r' {
			continue
		}
		if r == '\n' || r == '\t' {
			vk := uint16(0x0D)
			if r == '\t' {
				vk = 0x09
			}
			d, u := keyUnit(vk, false), keyUnit(vk, true)
			sendInput(&d)
			sendInput(&u)
			continue
		}
		for _, u := range encodeUTF16(r) {
			in := inputUnion{typ: inputKeyboard}
			kb := (*keybdInputStruct)(unsafe.Pointer(&in.mi))
			kb.wScan = u
			kb.dwFlags = keyEventUnicode
			sendInput(&in)
			kb.dwFlags = keyEventUnicode | keyEventKeyUp
			sendInput(&in)
		}
	}
	return nil
}

func encodeUTF16(r rune) []uint16 {
	if r <= 0xFFFF {
		return []uint16{uint16(r)}
	}
	r -= 0x10000
	return []uint16{uint16(0xD800 + (r >> 10)), uint16(0xDC00 + (r & 0x3FF))}
}

func splitLower(s string, sep byte) []string {
	var out []string
	cur := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == sep {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// ---------------- clipboard (PowerShell-backed, avoids extra Win32 marshaling) ----------------

func clipboardGet() (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Clipboard -Raw")
	hideWindow(cmd)
	out, err := cmd.Output()
	return decodeConsole(out), err
}

func clipboardSet(text string) error {
	name, args := shellCommand("powershell", "Set-Clipboard -Value ([Console]::In.ReadToEnd())")
	cmd := exec.Command(name, args...)
	hideWindow(cmd)
	stdin, _ := cmd.StdinPipe()
	go func() { stdin.Write([]byte(text)); stdin.Close() }()
	return cmd.Run()
}

var _ = syscall.Handle(0)
