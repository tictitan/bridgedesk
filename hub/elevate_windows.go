//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procShellExecuteW     = shell32.NewProc("ShellExecuteW")
	procOpenProcessToken  = advapi32.NewProc("OpenProcessToken")
	procGetTokenInfo      = advapi32.NewProc("GetTokenInformation")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
)

// isElevated reports whether this process is running with an elevated (administrator) token.
func isElevated() bool {
	const tokenQuery = 0x0008
	const tokenElevation = 20 // TokenElevation
	proc, _, _ := procGetCurrentProcess.Call()
	var token syscall.Handle
	r, _, _ := procOpenProcessToken.Call(proc, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return false
	}
	defer syscall.CloseHandle(token)
	var elevated uint32
	var retLen uint32
	procGetTokenInfo.Call(uintptr(token), tokenElevation,
		uintptr(unsafe.Pointer(&elevated)), unsafe.Sizeof(elevated), uintptr(unsafe.Pointer(&retLen)))
	return elevated != 0
}

// relaunchElevated starts a new elevated instance (one UAC prompt) and returns true on success,
// so the caller can exit and hand the ports over to the elevated copy.
func relaunchElevated() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	args := strings.Join(os.Args[1:], " ")
	if !strings.Contains(args, "--background") {
		args = strings.TrimSpace(args + " --background")
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(args)
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)), 0, swShowNormal)
	return r > 32 // ShellExecute returns >32 on success; <=32 means failure (e.g. user declined UAC)
}
