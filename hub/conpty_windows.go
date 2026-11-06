//go:build windows

package main

import (
	"io"
	"log"
	"sync"
	"syscall"
	"unsafe"
)

// conPTY wraps the Win32 pseudo-console API so an interactive PowerShell/cmd session
// streams to the dashboard's terminal view. I/O uses raw ReadFile/WriteFile on the pipe
// handles (NOT os.File) — os.File routes synchronous pipes through Go's poller, which can
// silently swallow writes to a ConPTY, so keystrokes never reach the shell.
type conPTY struct {
	hpc       uintptr
	inW       syscall.Handle // we write user keystrokes here
	outR      syscall.Handle // we read shell output here
	proc      syscall.Handle
	thread    syscall.Handle
	attr      []byte
	closeOnce sync.Once
}

type coord struct{ X, Y int16 }

func makePipe() (r, w syscall.Handle, err error) {
	var sa struct {
		nLength             uint32
		lpSecurityDesc      uintptr
		bInheritHandle      int32
	}
	sa.nLength = uint32(unsafe.Sizeof(sa))
	ret, _, e := procCreatePipe.Call(uintptr(unsafe.Pointer(&r)), uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&sa)), 0)
	if ret == 0 {
		return 0, 0, e
	}
	return r, w, nil
}

// startPTY returns a ConPTY-backed terminal, falling back to a pipe-backed shell if ConPTY
// can't be set up on this machine (so the terminal always works, just without full TUI/colors).
func startPTY(shell string, cols, rows int) (PTY, error) {
	p, err := startConPTY(shell, cols, rows)
	if err != nil {
		log.Printf("ConPTY unavailable (%v); using pipe-backed shell", err)
		return startPipeShell(shell)
	}
	return p, nil
}

func startConPTY(shell string, cols, rows int) (PTY, error) {
	// Pipes: inR/inW feed the console stdin; outR/outW carry console stdout.
	inR, inW, err := makePipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := makePipe()
	if err != nil {
		return nil, err
	}

	var hpc uintptr
	size := uintptr(uint32(uint16(cols)) | uint32(uint16(rows))<<16)
	ret, _, e := procCreatePseudoConsole.Call(size, uintptr(inR), uintptr(outW), 0, uintptr(unsafe.Pointer(&hpc)))
	if ret != 0 {
		return nil, e
	}
	// The pseudo-console dup'd these ends; close ours.
	procCloseHandle.Call(uintptr(inR))
	procCloseHandle.Call(uintptr(outW))

	// Build STARTUPINFOEX with the pseudo-console attribute.
	var listSize uintptr
	procInitializeProcThreadAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&listSize)))
	attr := make([]byte, listSize)
	if r, _, e := procInitializeProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&attr[0])), 1, 0, uintptr(unsafe.Pointer(&listSize))); r == 0 {
		procClosePseudoConsole.Call(hpc)
		return nil, e
	}
	const PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = 0x00020016
	if r, _, e := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(&attr[0])), 0, PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpc, unsafe.Sizeof(hpc), 0, 0); r == 0 {
		procClosePseudoConsole.Call(hpc)
		return nil, e
	}

	type startupInfoEx struct {
		cb            uint32
		_             [unsafe.Offsetof(syscall.StartupInfo{}.Cb) + 0]byte
	}
	// Use the documented STARTUPINFOEXW layout manually.
	var si struct {
		StartupInfo syscall.StartupInfo
		AttrList    uintptr
	}
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.AttrList = uintptr(unsafe.Pointer(&attr[0]))

	cmdline, _ := syscall.UTF16PtrFromString(buildShellCmdline(shell))
	var pi syscall.ProcessInformation
	const EXTENDED_STARTUPINFO_PRESENT = 0x00080000
	r, _, e := procCreateProcessW.Call(
		0,
		uintptr(unsafe.Pointer(cmdline)),
		0, 0, 0,
		EXTENDED_STARTUPINFO_PRESENT, // matches Microsoft's ConPTY reference
		0, 0,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		procClosePseudoConsole.Call(hpc)
		return nil, e
	}
	procCloseHandle.Call(uintptr(pi.Thread))

	p := &conPTY{
		hpc:    hpc,
		inW:    inW,
		outR:   outR,
		proc:   pi.Process,
		thread: syscall.Handle(pi.Thread),
		attr:   attr,
	}
	return p, nil
}

func buildShellCmdline(shell string) string {
	switch shell {
	case "cmd":
		return "cmd.exe"
	case "pwsh":
		return "pwsh.exe -NoLogo"
	default:
		return "powershell.exe -NoLogo"
	}
}

func (p *conPTY) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var done uint32
	r, _, e := procReadFile.Call(uintptr(p.outR), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&done)), 0)
	if r == 0 || done == 0 {
		if done == 0 {
			return 0, io.EOF
		}
		return 0, e
	}
	return int(done), nil
}

func (p *conPTY) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var done uint32
	r, _, e := procWriteFile.Call(uintptr(p.inW), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&done)), 0)
	if r == 0 {
		return 0, e
	}
	return int(done), nil
}

func (p *conPTY) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	size := uintptr(uint32(uint16(cols)) | uint32(uint16(rows))<<16)
	procResizePseudoConsole.Call(p.hpc, size)
	return nil
}

func (p *conPTY) Wait() int {
	procWaitForSingleObject.Call(uintptr(p.proc), 0xFFFFFFFF)
	var code uint32
	procGetExitCodeProcess.Call(uintptr(p.proc), uintptr(unsafe.Pointer(&code)))
	p.Close()
	return int(int32(code))
}

func (p *conPTY) Close() error {
	p.closeOnce.Do(func() {
		procClosePseudoConsole.Call(p.hpc) // signals the shell to exit
		procCloseHandle.Call(uintptr(p.inW))
		procCloseHandle.Call(uintptr(p.outR))
		if len(p.attr) > 0 {
			procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&p.attr[0])))
		}
		procTerminateProcess.Call(uintptr(p.proc), 0)
		procCloseHandle.Call(uintptr(p.proc))
	})
	return nil
}
