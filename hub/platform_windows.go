//go:build windows

// Windows capability layer for BridgeDesk — a user-run remote-desktop + file-sync hub
// (same category as RustDesk/AnyDesk). Every capability here is gated by an access key
// and per-capability toggles in config.go, and the machine owner starts the program.

package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	ntdll    = syscall.NewLazyDLL("ntdll.dll")

	procCreatePseudoConsole               = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole               = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole                = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcessW                    = kernel32.NewProc("CreateProcessW")
	procCreatePipe                        = kernel32.NewProc("CreatePipe")
	procCloseHandle                       = kernel32.NewProc("CloseHandle")
	procReadFile                          = kernel32.NewProc("ReadFile")
	procWriteFile                         = kernel32.NewProc("WriteFile")
	procGetExitCodeProcess                = kernel32.NewProc("GetExitCodeProcess")
	procWaitForSingleObject               = kernel32.NewProc("WaitForSingleObject")
	procTerminateProcess                  = kernel32.NewProc("TerminateProcess")
	procGetLogicalDrives                  = kernel32.NewProc("GetLogicalDrives")
	procMultiByteToWideChar               = kernel32.NewProc("MultiByteToWideChar")
	procAttachConsole                     = kernel32.NewProc("AttachConsole")

	procGetDC                  = user32.NewProc("GetDC")
	procReleaseDC              = user32.NewProc("ReleaseDC")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware     = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwareCtx  = user32.NewProc("SetProcessDpiAwarenessContext")
	procSendInput              = user32.NewProc("SendInput")
	procMessageBoxW            = user32.NewProc("MessageBoxW")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procRtlGetVersion          = ntdll.NewProc("RtlGetVersion")
)

func platformInit() {
	if procSetProcessDpiAwareCtx.Find() == nil {
		procSetProcessDpiAwareCtx.Call(uintptr(^uintptr(3))) // PER_MONITOR_AWARE_V2 (-4)
	} else {
		procSetProcessDPIAware.Call()
	}
}

func attachParentConsole() {
	r, _, _ := procAttachConsole.Call(uintptr(^uint32(0)))
	if r != 0 {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout, os.Stderr = f, f
		}
	}
}

func showMessage(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x40)
}

func osDescription() string {
	type osvi struct {
		size, major, minor, build, platform uint32
		csd                                 [128]uint16
	}
	v := osvi{}
	v.size = uint32(unsafe.Sizeof(v))
	procRtlGetVersion.Call(uintptr(unsafe.Pointer(&v)))
	name := "Windows 10"
	if v.build >= 22000 {
		name = "Windows 11"
	}
	return fmt.Sprintf("%s (build %d)", name, v.build)
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func powershellExe(pref string) string {
	if pref == "pwsh" {
		if p, err := exec.LookPath("pwsh.exe"); err == nil {
			return p
		}
	}
	return "powershell.exe"
}

func shellCommand(shell, command string) (string, []string) {
	switch strings.ToLower(shell) {
	case "cmd":
		return "cmd.exe", []string{"/D", "/S", "/C", command}
	default:
		script := "$ProgressPreference='SilentlyContinue';[Console]::OutputEncoding=[Text.Encoding]::UTF8;$OutputEncoding=[Text.Encoding]::UTF8;\n" + command
		u := utf16.Encode([]rune(script))
		b := make([]byte, len(u)*2)
		for i, c := range u {
			b[i*2], b[i*2+1] = byte(c), byte(c>>8)
		}
		return powershellExe(shell), []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(b)}
	}
}

func decodeConsole(b []byte) string {
	s := string(b)
	if strings.HasPrefix(s, "#< CLIXML") {
		return ""
	}
	if len(b) == 0 || utf8.Valid(b) {
		return s
	}
	n, _, _ := procMultiByteToWideChar.Call(1, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return strings.ToValidUTF8(s, "?")
	}
	w := make([]uint16, n)
	procMultiByteToWideChar.Call(1, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&w[0])), n)
	return string(utf16.Decode(w))
}

func systemRoots() []Root {
	mask, _, _ := procGetLogicalDrives.Call()
	var out []Root
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) != 0 {
			d := string(rune('A'+i)) + ":\\"
			out = append(out, Root{"Drive " + d[:2], d})
		}
	}
	return out
}

func openTarget(target string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target)
	return cmd.Start()
}

func setAutostart(enable bool) error {
	exe, _ := os.Executable()
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	var cmd *exec.Cmd
	if enable {
		cmd = exec.Command("reg.exe", "add", key, "/v", "BridgeDesk", "/t", "REG_SZ", "/d", `"`+exe+`" --background`, "/f")
	} else {
		cmd = exec.Command("reg.exe", "delete", key, "/v", "BridgeDesk", "/f")
	}
	hideWindow(cmd)
	return cmd.Run()
}

func autostartEnabled() bool {
	cmd := exec.Command("reg.exe", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "BridgeDesk")
	hideWindow(cmd)
	return cmd.Run() == nil
}

// ensureFirewall adds an inbound allow rule for this program so other PCs/phones can reach the hub.
// Best-effort: it needs admin to take effect; if it fails the user allows it via the Windows prompt.
func ensureFirewall(port int) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Remove any stale rule, then add a fresh program-scoped allow rule for private+domain networks.
	del := exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name=BridgeDesk")
	hideWindow(del)
	del.Run()
	add := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=BridgeDesk", "dir=in", "action=allow", "program="+exe,
		"enable=yes", "profile=private,domain")
	hideWindow(add)
	add.Run()
}

// desktopToast raises a native Windows notification (used to mirror phone notifications).
func desktopToast(title, text string) {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "'", "''")
		if len(s) > 180 {
			s = s[:180] + "…"
		}
		return s
	}
	ps := `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$n = $t.GetElementsByTagName('text')
$n.Item(0).AppendChild($t.CreateTextNode('` + esc(title) + `')) > $null
$n.Item(1).AppendChild($t.CreateTextNode('` + esc(text) + `')) > $null
$toast = [Windows.UI.Notifications.ToastNotification]::new($t)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('BridgeDesk').Show($toast)`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", ps)
	hideWindow(cmd)
	cmd.Start()
	go func() { cmd.Wait() }()
}
