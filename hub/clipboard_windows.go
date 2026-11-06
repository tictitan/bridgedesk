//go:build windows

package main

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Native clipboard access. Text set here goes through the normal Windows clipboard, so with
// clipboard history turned on every synced item also shows up in Win+V.

var (
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
)

const cfUnicodeText = 13

func openClipboard() error {
	for i := 0; i < 20; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("clipboard is busy")
}

func clipboardGet() (string, error) {
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil // no text on the clipboard (image, files…)
	}
	size, _, _ := procGlobalSize.Call(h)
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", errors.New("GlobalLock failed")
	}
	defer procGlobalUnlock.Call(h)
	n := int(size / 2)
	if n > 8<<20 {
		n = 8 << 20
	}
	buf := unsafe.Slice((*uint16)(unsafe.Pointer(p)), n)
	for i, c := range buf {
		if c == 0 {
			buf = buf[:i]
			break
		}
	}
	return string(utf16.Decode(buf)), nil
}

func clipboardSet(text string) error {
	u := utf16.Encode([]rune(text))
	u = append(u, 0)
	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	const gmemMoveable = 0x0002
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(u)*2))
	if h == 0 {
		return errors.New("GlobalAlloc failed")
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return errors.New("GlobalLock failed")
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(u)), u)
	procGlobalUnlock.Call(h)
	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return errors.New("SetClipboardData failed")
	}
	return nil
}

// clipboardSeq changes every time anything is copied (0 = unsupported).
func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

var privateFormats = []string{"ExcludeClipboardContentFromMonitorProcessing", "Clipboard Viewer Ignore", "CanIncludeInClipboardHistory"}

// clipboardPrivate reports whether the current clip is marked private — password managers
// (KeePass, Bitwarden, 1Password…) set these formats so the copy is never synced or kept in history.
func clipboardPrivate() bool {
	for _, f := range privateFormats {
		name, _ := syscall.UTF16PtrFromString(f)
		id, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name)))
		if id == 0 {
			continue
		}
		if r, _, _ := procIsClipboardFormatAvailable.Call(id); r != 0 {
			return true
		}
	}
	return false
}

const clipHistoryKey = `HKCU\Software\Microsoft\Clipboard`

// winClipHistory reports whether Windows clipboard history (Win+V) is on.
func winClipHistory() (supported, on bool) {
	cmd := exec.Command("reg", "query", clipHistoryKey, "/v", "EnableClipboardHistory")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return true, false
	}
	return true, strings.Contains(string(out), "0x1")
}

func setWinClipHistory(on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	cmd := exec.Command("reg", "add", clipHistoryKey, "/v", "EnableClipboardHistory", "/t", "REG_DWORD", "/d", v, "/f")
	hideWindow(cmd)
	return cmd.Run()
}
