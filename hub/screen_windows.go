//go:build windows

package main

import (
	"errors"
	"image"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	smCXSCREEN = 0
	smCYSCREEN = 1
)

func screenSupported() bool { return true }

func screenSize() (int, int) {
	w, _, _ := procGetSystemMetrics.Call(smCXSCREEN)
	h, _, _ := procGetSystemMetrics.Call(smCYSCREEN)
	return int(w), int(h)
}

// captureScreen grabs the primary display into an RGBA image via GDI BitBlt.
func captureScreen() (*image.RGBA, error) {
	w, h := screenSize()
	if w == 0 || h == 0 {
		return nil, errors.New("no display")
	}
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, errors.New("GetDC failed")
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
	if r, _, _ := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, 0, 0, SRCCOPY|CAPTUREBLT); r == 0 {
		return nil, errors.New("BitBlt failed")
	}

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

	buf := make([]byte, w*h*4)
	const DIB_RGB_COLORS = 0
	r, _, _ := procGetDIBits.Call(memDC, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), DIB_RGB_COLORS)
	if r == 0 {
		return nil, errors.New("GetDIBits failed")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// GDI gives BGRA; swap to RGBA and set alpha opaque.
	for i := 0; i < len(buf); i += 4 {
		img.Pix[i] = buf[i+2]
		img.Pix[i+1] = buf[i+1]
		img.Pix[i+2] = buf[i]
		img.Pix[i+3] = 0xFF
	}
	return img, nil
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
	mouseEventAbsolute   = 0x8000
	mouseEventVirtualDsk = 0x4000

	keyEventKeyUp   = 0x0002
	keyEventUnicode = 0x0004
)

func absCoord(norm float64, metric int) int32 {
	size, _, _ := procGetSystemMetrics.Call(uintptr(metric))
	if size <= 1 {
		return 0
	}
	return int32(norm * 65535.0)
}

func doMouse(p MouseInput) error {
	in := inputUnion{typ: inputMouse}
	in.mi.dwExtraInfo = 0
	switch p.Action {
	case "move", "down", "up", "click", "dblclick":
		in.mi.dx = int32(p.X * 65535.0)
		in.mi.dy = int32(p.Y * 65535.0)
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
		in.mi.dwFlags |= uint32(down)
		sendInput(&in)
		in.mi.dwFlags = uint32(up)
		return sendInput(&in)
	case "dblclick":
		for i := 0; i < 2; i++ {
			c := in
			c.mi.dwFlags |= uint32(down)
			sendInput(&c)
			c.mi.dwFlags = uint32(up)
			sendInput(&c)
		}
		return nil
	case "scroll":
		w := inputUnion{typ: inputMouse}
		w.mi.dwFlags = mouseEventWheel
		w.mi.mouseData = uint32(int32(-p.DeltaY * 2))
		return sendInput(&w)
	}
	return nil
}

func keyUnit(vk uint16, up bool) inputUnion {
	in := inputUnion{typ: inputKeyboard}
	kb := (*keybdInputStruct)(unsafe.Pointer(&in.mi))
	kb.wVk = vk
	if up {
		kb.dwFlags = keyEventKeyUp
	}
	return in
}

// vkName maps friendly key names from the browser to Windows virtual-key codes.
var vkName = map[string]uint16{
	"enter": 0x0D, "return": 0x0D, "tab": 0x09, "escape": 0x1B, "esc": 0x1B,
	"backspace": 0x08, "delete": 0x2E, "space": 0x20, "up": 0x26, "down": 0x28,
	"left": 0x25, "right": 0x27, "home": 0x24, "end": 0x23, "pageup": 0x21,
	"pagedown": 0x22, "insert": 0x2D, "ctrl": 0x11, "control": 0x11, "alt": 0x12,
	"shift": 0x10, "win": 0x5B, "meta": 0x5B, "cmd": 0x5B,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
}

// doKeyCombo handles a key or a combo like "ctrl+alt+delete" (action: down|up|press).
func doKeyCombo(combo, action string) error {
	parts := splitLower(combo, '+')
	var vks []uint16
	for _, p := range parts {
		if vk, ok := vkName[p]; ok {
			vks = append(vks, vk)
		} else if len(p) == 1 {
			c := p[0]
			if c >= 'a' && c <= 'z' {
				c -= 32
			}
			vks = append(vks, uint16(c))
		}
	}
	if len(vks) == 0 {
		return errors.New("unknown key: " + combo)
	}
	press := func() {
		for _, vk := range vks {
			u := keyUnit(vk, false)
			sendInput(&u)
		}
		for i := len(vks) - 1; i >= 0; i-- {
			u := keyUnit(vks[i], true)
			sendInput(&u)
		}
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
		press()
	}
	return nil
}

// typeText injects a Unicode string as keystrokes.
func typeText(s string) error {
	for _, r := range s {
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
